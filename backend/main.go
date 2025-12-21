package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/automerge/automerge-go"
	"github.com/coder/websocket"
	"github.com/rs/cors"
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
	defer func() {
		// Recover from panic if channel is closed
		if r := recover(); r != nil {
			fmt.Printf("wsReadLoop: recovered from panic (channel likely closed): %v\n", r)
		}
	}()

	fmt.Println("wsReadLoop started, beginning to read...")
	// loop to read continuously
	for {
		mt, b, err := conn.Read(ctx)
		fmt.Println("wsreadloop read message")

		// Try to send the result (including errors)
		// Use select to handle channel closure gracefully
		select {
		case readUserChanges <- userReadLoopResult{mt, b, err}:
			// Successfully sent
			// If there was an error, userLoop will handle it and close the channel
			// We'll exit on the next iteration when we try to send to closed channel
			if err != nil {
				// After sending error, exit the loop
				return
			}
		case <-ctx.Done():
			// Context cancelled, exit
			return
		}
	}
}

// each connected user has an instance of this func running
func userLoop(conn *websocket.Conn, serverChanges chan *automerge.SyncMessage, userChangesAndConn chan userChangeAndConnType, leavingConns chan *websocket.Conn) {
	fmt.Println("User loop started")
	// notify docLoop when connection is terminated
	defer func() {
		fmt.Println("User loop ending, closing connection")
		conn.CloseNow()
		leavingConns <- conn
	}()

	// TODO: figure out a better context solution
	ctx := context.Background()
	// Larger buffer to prevent dropping messages
	readUserChanges := make(chan userReadLoopResult, 50)
	defer close(readUserChanges)
	go wsReadLoop(ctx, conn, readUserChanges)

	for {
		select {
		case serverChange := <-serverChanges:
			// send broadcast changes sent by the server; send to user
			fmt.Println("sending server change to user")
			if err := conn.Write(ctx, websocket.MessageType(websocket.MessageBinary), serverChange.Bytes()); err != nil {
				fmt.Printf("Error writing to connection: %v\n", err)
				return
			}

		case readUserChange := <-readUserChanges:
			// user sent changes to server; forward to doc handler
			b := readUserChange.b
			err := readUserChange.err
			mt := readUserChange.mt

			if err != nil {
				if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
					fmt.Println("User WS connection closed with error:", err.Error())
				}
				return
			}

			// Only process binary messages (Automerge sync messages)
			if mt != websocket.MessageBinary {
				fmt.Printf("Received non-binary message type: %v, ignoring\n", mt)
				continue
			}

			fmt.Printf("Received binary message of size %d bytes from connection\n", len(b))
			// send syncmessage to doc handler
			// Use blocking send - we can't drop Automerge sync messages or sync will break
			// The large buffer (100) should prevent this from blocking in normal operation
			// If it does block, it means docLoop is slow, but at least we won't lose messages
			queueLenBefore := len(userChangesAndConn)
			userChangesAndConn <- userChangeAndConnType{conn, b}
			queueLenAfter := len(userChangesAndConn)
			if queueLenBefore > 50 {
				fmt.Printf("WARNING: userChangesAndConn queue was large (%d), now %d items\n", queueLenBefore, queueLenAfter)
			} else {
				fmt.Printf("Successfully queued message to docLoop (channel has %d items)\n", queueLenAfter)
			}
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
	// Larger buffer to prevent dropping messages (dropping breaks Automerge sync)
	userChangesAndConn := make(chan userChangeAndConnType, 100)
	defer close(userChangesAndConn)
	leavingConns := make(chan *websocket.Conn, 10)
	defer close(leavingConns)

	// loop for adding / removing users & merging / broadcasting changes
	wsConnInputs := make(map[*websocket.Conn]chan *automerge.SyncMessage)
	for {
		select {
		case conn := <-joiningConns:
			// closed when userLoop dies
			fmt.Printf("docLoop: received new connection for doc %s\n", docId)
			wsConnInputs[conn] = make(chan *automerge.SyncMessage, 10)
			fmt.Printf("docLoop: starting userLoop for doc %s. total connections: %d\n", docId, len(wsConnInputs))
			go userLoop(conn, wsConnInputs[conn], userChangesAndConn, leavingConns)

			// Send initial sync message to the new connection so it gets current document state
			serverChange, _ := syncstate.GenerateMessage()
			if serverChange != nil {
				select {
				case wsConnInputs[conn] <- serverChange:
					fmt.Printf("docLoop: sent initial sync message to new connection for doc %s\n", docId)
				default:
					fmt.Printf("Warning: failed to send initial sync to new connection for doc %s\n", docId)
				}
			}

		case conn := <-leavingConns:
			close(wsConnInputs[conn])
			delete(wsConnInputs, conn)

		case userChangeAndConn := <-userChangesAndConn:
			// merge changes into doc
			fmt.Printf("docLoop: processing message from connection (queue had %d items before)\n", len(userChangesAndConn)+1)
			syncstate.ReceiveMessage(userChangeAndConn.bytes)
			fmt.Printf("docLoop: merged message, queue now has %d items\n", len(userChangesAndConn))

			// broadcast changes to everyone except the author
			serverChange, _ := syncstate.GenerateMessage()
			for conn, ch := range wsConnInputs {
				if conn != userChangeAndConn.authorConn {
					// Use non-blocking send to prevent docLoop from getting stuck
					select {
					case ch <- serverChange:
						// Successfully sent
					default:
						// Channel full, skip this connection (it will get updates on next sync)
						fmt.Printf("Warning: channel full for connection, skipping broadcast\n")
					}
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
			fmt.Printf("docLoopManager: received new connection for doc %s\n", newConnAndDocId.docId)
			// create docLoop if DNE
			_, ok := docLoops[newConnAndDocId.docId]
			if !ok {
				fmt.Printf("docLoopManager: creating new docLoop for doc %s\n", newConnAndDocId.docId)
				// add to dict
				// closed when docLoop dies
				// Use buffered channel to prevent blocking when sending new connections
				joiningConns := make(chan *websocket.Conn, 10)
				docLoops[newConnAndDocId.docId] = joiningConns

				// start the docloop
				go docLoop(docLoops[newConnAndDocId.docId], newConnAndDocId.docId, dyingDocLoopIds)
			}

			// send the connection to the docloop (non-blocking with buffered channel)
			select {
			case docLoops[newConnAndDocId.docId] <- newConnAndDocId.conn:
				fmt.Printf("docLoopManager: successfully sent connection to docLoop for doc %s\n", newConnAndDocId.docId)
			default:
				// This shouldn't happen with buffered channel, but handle gracefully
				fmt.Printf("Warning: failed to send connection to docLoop for doc %s (channel full)\n", newConnAndDocId.docId)
			}

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
	// Use buffered channel to prevent HTTP handlers from blocking
	connsAndDocIds := make(chan connAndDocId, 100)
	go docLoopManager(connsAndDocIds)

	mux := http.NewServeMux() // mux pointer

	// accepts user WS connection and starts goroutine to send / receive changes
	mux.HandleFunc("/document/{docId}", func(w http.ResponseWriter, r *http.Request) {
		// upgrade to websocket
		// conn will be closed by the userLoop when it does
		// Configure origin checking to allow localhost:5173 (Vite dev server)
		opts := &websocket.AcceptOptions{
			OriginPatterns: []string{
				"http://localhost:5173",
				"http://127.0.0.1:5173",
				"localhost:5173",
				"127.0.0.1:5173",
			},
		}
		conn, err := websocket.Accept(w, r, opts)
		if err != nil {
			fmt.Println("Failed to accept WebSocket:", err.Error())
			return
		}

		docId := r.PathValue("docId")
		fmt.Printf("WebSocket connection accepted for document: %s\n", docId)

		// send connection to doc manager
		connsAndDocIds <- connAndDocId{conn, docId}
	})

	s := &http.Server{
		Addr:    addr,
		Handler: cors.Default().Handler(mux),
	}

	fmt.Printf("Listening on %s...\n", addr)
	s.ListenAndServe()
}
