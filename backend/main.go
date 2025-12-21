package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/automerge/automerge-go"
	"github.com/coder/websocket"
)

type userReadLoopResult struct {
	mt  websocket.MessageType
	b   []byte
	err error
}

type userChangeAndConnType struct {
	authorConn *websocket.Conn
	bytes      []byte // cuz func (*SyncState) ReceiveMessage receives bytes
}

type connAndDocId struct {
	conn  *websocket.Conn
	docId string
}

func wsReadLoop(ctx context.Context, conn *websocket.Conn, readUserChanges chan userReadLoopResult) {
	// loop to read continuously
	for {
		mt, b, err := conn.Read(ctx)
		readUserChanges <- userReadLoopResult{mt, b, err}

		if err != nil {
			// error message printed in parent process
			return
		}
	}
}

// each connected user has an instance of this func running
func userLoop(conn *websocket.Conn, serverChanges chan *automerge.SyncMessage, userChangesAndConn chan userChangeAndConnType, leavingConns chan *websocket.Conn) {
	// notify docLoop when connection is terminated
	defer conn.CloseNow()
	defer func() { leavingConns <- conn }()

	// TODO: figure out a better context solution
	ctx := context.Background()
	readUserChanges := make(chan userReadLoopResult, 5)
  defer close(readUserChanges)
	go wsReadLoop(ctx, conn, readUserChanges)

	for {
		select {
		case serverChange := <-serverChanges:
			// send broadcast changes sent by the server; send to user
			conn.Write(ctx, websocket.MessageType(websocket.MessageBinary), serverChange.Bytes())

		case readUserChange := <-readUserChanges:
			// user sent changes to server; forward to doc handler
			b := readUserChange.b
			err := readUserChange.err
			if err != nil {
				if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
					fmt.Println("User WS connection closed with error:", err.Error())
				}
				return
			}

			// send syncmessage to doc handler
			userChangesAndConn <- userChangeAndConnType{conn, b}
		}
	}
}

// every document has an instance of this func running.
// will also launch instances of userLoop for every user that connects to this doc
func docLoop(joiningConns chan *websocket.Conn, docId string, dyingDocLoopIDs chan string) {
	defer func() { dyingDocLoopIDs <- docId }()

	// set up automerge server
	// since all in memory, starting with blank doc & syncstate
	// TODO: add persistence here
	// TODO: afer persistence, exit routine if no users connected
	doc := automerge.New()
	syncstate := automerge.NewSyncState(doc)
	userChangesAndConn := make(chan userChangeAndConnType, 10)
	defer close(userChangesAndConn)
	leavingConns := make(chan *websocket.Conn, 10)
	defer close(leavingConns)

	// loop for adding / removing users & merging / broadcasting changes
	wsConnInputs := make(map[*websocket.Conn]chan *automerge.SyncMessage)
	for {
		select {
		case conn := <-joiningConns:
      // closed when userLoop dies
			wsConnInputs[conn] = make(chan *automerge.SyncMessage, 10)
			go userLoop(conn, wsConnInputs[conn], userChangesAndConn, leavingConns)

		case conn := <-leavingConns:
			close(wsConnInputs[conn])
			delete(wsConnInputs, conn)

		case userChangeAndConn := <-userChangesAndConn:
			// merge changes into doc
			syncstate.ReceiveMessage(userChangeAndConn.bytes)

			// broadcast changes to everyone except the author
			serverChange, _ := syncstate.GenerateMessage()
			for conn, ch := range wsConnInputs {
				if conn != userChangeAndConn.authorConn {
					ch <- serverChange
				}
			}
		}
	}
}

// stores info about running docLoops
// receives a user connection through channel
// creates docLoop for the target document if it doesn't exist
// passes the user connection to the doc loop
func docLoopManager(connsAndDocIds chan connAndDocId) {
	// channel to notify when a docloop dies
	dyingDocLoopIds := make(chan string, 10)
  defer close(dyingDocLoopIds)

	// maps doc IDs to their respective joiningConns channel
	docLoops := map[string]chan *websocket.Conn{}

	// loop to listen on new connections
	for {
		select {
		case newConnAndDocId := <-connsAndDocIds:
			// create docLoop if DNE
			_, ok := docLoops[newConnAndDocId.docId]
			if !ok {
				// add to dict
        // closed when docLoop dies
				joiningConns := make(chan *websocket.Conn)
				docLoops[newConnAndDocId.docId] = joiningConns

				// start the docloop
				go docLoop(docLoops[newConnAndDocId.docId], newConnAndDocId.docId, dyingDocLoopIds)
			}

			// send the connection to the docloop
			docLoops[newConnAndDocId.docId] <- newConnAndDocId.conn

		case dyingDocLoopId := <-dyingDocLoopIds:
			// clear docloop resources

			close(docLoops[dyingDocLoopId])
			delete(docLoops, dyingDocLoopId)
		}
	}
}

func main() {
	// set up server
	const addr = ":8080"

	// start doc loop manager
	connsAndDocIds := make(chan connAndDocId)
	go docLoopManager(connsAndDocIds)

	mux := http.NewServeMux() // mux pointer

	// accepts user WS connection and starts goroutine to send / receive changes
	mux.HandleFunc("/document/{docId}", func(w http.ResponseWriter, r *http.Request) {
		// upgrade to websocket
		// conn will be closed by the userLoop when it does
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			fmt.Println(err.Error())
			return
		}

		// send connection to doc manager
		connsAndDocIds <- connAndDocId{conn, r.PathValue("docId")}
	})

	s := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	fmt.Printf("Listening on %s...", addr)
	s.ListenAndServe()
}
