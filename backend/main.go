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

func wsReadLoop(ctx context.Context, conn *websocket.Conn, readUserChanges chan userReadLoopResult) {
	defer close(readUserChanges)

	// loop to read continuously
	for {
		mt, b, err := conn.Read(ctx)
		readUserChanges <- userReadLoopResult{mt, b, err}

		if err != nil {
			fmt.Println(err.Error())
			return
		}
	}
}

// each connected user has an instance of this func running
func userLoop(conn *websocket.Conn, serverChanges chan *automerge.SyncMessage, userChangesAndConn chan userChangeAndConnType) {
	// FIXME: handle deliberate disconnection
	// FIXME: close the connections / channels passed here from the owner process

	// TODO: figure out a better context solution
	ctx := context.Background()
	readUserChanges := make(chan userReadLoopResult, 5)
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
				fmt.Println(err.Error())
				return
			}

			// send syncmessage to doc handler
			userChangesAndConn <- userChangeAndConnType{conn, b}
		}
	}
}

// every document has an instance of this func running.
// will also launch instances of userLoop for every user that connects to this doc
func docLoop(joiningConns chan *websocket.Conn, leavingConns chan *websocket.Conn) {
	// set up automerge server
	// since all in memory, starting with blank doc & syncstate
	// TODO: add persistence here
	doc := automerge.New()
	syncstate := automerge.NewSyncState(doc)
	userChangesAndConn := make(chan userChangeAndConnType, 10)
	defer close(userChangesAndConn)

	// loop for adding / removing users & merging / broadcasting changes
	wsConnInputs := make(map[*websocket.Conn]chan *automerge.SyncMessage)
	for {
		select {
		case conn := <-joiningConns:
			wsConnInputs[conn] = make(chan *automerge.SyncMessage, 10)
			go userLoop(conn, wsConnInputs[conn], userChangesAndConn)

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

// FIXME: handle the case when HandleUserConn returns. TBD if both failure or also clean close
// accepts user WS connection and starts goroutine to send / receive changes
func acceptUserConnection(w http.ResponseWriter, r *http.Request) {
	// upgrade to websocket
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	// FIXME: figure out when to close the connection

	// launch user connection handler goroutine
	// TODO:

}

func main() {
	// set up server
	const addr = ":8080"

	mux := http.NewServeMux() // mux pointer
	s := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	fmt.Printf("Listening on %s...", addr)
	s.ListenAndServe()
}
