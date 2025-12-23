package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/automerge/automerge-go"
	"github.com/coder/websocket"
	"github.com/rs/cors"
)

type userReadLoopResult struct {
	mt  websocket.MessageType
	b   []byte
	err error
}

type connAndDocId struct {
	conn  *websocket.Conn
	docId string
}

func wsReadLoop(ctx context.Context, conn *websocket.Conn, readUserChanges chan userReadLoopResult) {
	// loop to read continuously
	for {
		mt, b, err := conn.Read(ctx)
		slog.Debug("wsReadLoop: message read", "messageType", mt, "bytesLength", len(b), "error", err)
		readUserChanges <- userReadLoopResult{mt, b, err}

		if err != nil {
			slog.Debug("wsReadLoop: exiting due to error", "error", err)
			// error message printed in parent process
			return
		}
	}
}

// each connected user has an instance of this func running
func userLoop(conn *websocket.Conn, serverChanged chan struct{}, syncState *automerge.SyncState, userChanged chan struct{}, leavingConns chan *websocket.Conn) {
	slog.Debug("userLoop: starting", "conn", fmt.Sprintf("%p", conn))
	// notify docLoop when connection is terminated
	defer conn.CloseNow()
	defer func() {
		slog.Debug("userLoop: connection closing", "conn", fmt.Sprintf("%p", conn))
		leavingConns <- conn
	}()

	// TODO: figure out a better context solution
	ctx := context.Background()
	readUserChanges := make(chan userReadLoopResult, 16)
	defer close(readUserChanges)
	go wsReadLoop(ctx, conn, readUserChanges)

	for {
		select {
		case <-serverChanged:
			// send broadcast changes sent by the server; send to user
			// Get the current syncstate from docLoop
			msg, valid := syncState.GenerateMessage()

			if valid {
				bytes := msg.Bytes()
				slog.Debug("userLoop: sending server change to user", "conn", fmt.Sprintf("%p", conn), "bytesLength", len(bytes))
				err := conn.Write(ctx, websocket.MessageType(websocket.MessageBinary), bytes)
				if err != nil {
					slog.Debug("userLoop: error writing to connection", "conn", fmt.Sprintf("%p", conn), "error", err)
				}
			} else {
				slog.Debug("userLoop: changes invalid", "conn", fmt.Sprintf("%p", conn))
			}

		case readUserChange := <-readUserChanges:
			// user sent changes to server; forward to doc handler
			b := readUserChange.b
			err := readUserChange.err
			slog.Debug("userLoop: received user change", "conn", fmt.Sprintf("%p", conn), "bytesLength", len(b), "error", err)
			if err != nil {
				if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
					slog.Error("User WS connection closed with error", "error", err.Error())
					fmt.Println("User WS connection closed with error:", err.Error())
				} else {
					slog.Debug("userLoop: normal closure", "conn", fmt.Sprintf("%p", conn))
				}
				return
			}

			// merge changes
			// internally locks the doc, so no risk of race conditions
			// see https://pkg.go.dev/github.com/automerge/automerge-go#SyncState.ReceiveMessage
			_, err = syncState.ReceiveMessage(b)
			if err != nil {
				slog.Error("userLoop: failed to receive syncstate message.", "error", err.Error())
				continue
			}
			slog.Debug("userLoop: changes merged into doc")

			// notify server of a change (nonblocking)
			select {
			case userChanged <- struct{}{}:
			default:
			}
		}
	}
}

// every document has an instance of this func running.
// will also launch instances of userLoop for every user that connects to this doc
func docLoop(joiningConns chan *websocket.Conn, docId string, dyingDocLoopIDs chan string) {
	slog.Debug("docLoop: starting", "docId", docId)
	defer func() {
		slog.Debug("docLoop: dying", "docId", docId)
		dyingDocLoopIDs <- docId
	}()

	// set up automerge server
	// since all in memory, starting with blank doc
	// TODO: add persistence here
	// TODO: afer persistence, exit routine if no users connected
	doc := automerge.New()
	slog.Debug("docLoop: initialized automerge doc", "docId", docId)

	userChanged := make(chan struct{}, 1)
	defer close(userChanged)
	leavingConns := make(chan *websocket.Conn, 16)
	defer close(leavingConns)

	// loop for adding / removing users & merging / broadcasting changes
	changeNotif := make(map[*websocket.Conn]chan struct{})
	for {
		select {
		case conn := <-joiningConns:
			// Create a syncstate for this connection
			syncState := automerge.NewSyncState(doc)
			slog.Debug("docLoop: user joining", "docId", docId, "conn", fmt.Sprintf("%p", conn), "activeConnections", len(changeNotif)+1)

			changeNotif[conn] = make(chan struct{}, 1)
			changeNotif[conn] <- struct{}{} // immediate sync on connection establishment

			go userLoop(conn, changeNotif[conn], syncState, userChanged, leavingConns)

		case conn := <-leavingConns:
			slog.Debug("docLoop: user leaving", "docId", docId, "conn", fmt.Sprintf("%p", conn), "activeConnections", len(changeNotif)-1)
			close(changeNotif[conn])
			delete(changeNotif, conn)

		case <-userChanged:
			// Receive the message using the author's syncstate
			slog.Debug("docLoop: a user has changed the doc", "docId", docId, "content", doc.Root().GoString())

			// Notify ALL users (including the author) that there's an update
			// The sync protocol requires bidirectional communication:
			// after receiving a message, we must respond to the sender
			broadcastCount := 0
			for conn, ch := range changeNotif {
				slog.Debug("docLoop: notifying userLoop to send change", "docId", docId, "targetConn", fmt.Sprintf("%p", conn))

				select {
				case ch <- struct{}{}: // notify if channel empty
				default: // no need to notify if existing change notif exists
				}

				broadcastCount++
			}
			slog.Debug("docLoop: broadcast complete", "docId", docId, "recipients", broadcastCount)
		}
	}
}

// stores info about running docLoops
// receives a user connection through channel
// creates docLoop for the target document if it doesn't exist
// passes the user connection to the doc loop
func docLoopManager(connsAndDocIds chan connAndDocId) {
	slog.Debug("docLoopManager: starting")
	// channel to notify when a docloop dies
	dyingDocLoopIds := make(chan string, 16)
	defer close(dyingDocLoopIds)

	// maps doc IDs to their respective joiningConns channel
	docLoops := map[string]chan *websocket.Conn{}

	// loop to listen on new connections
	for {
		select {
		case newConnAndDocId := <-connsAndDocIds:
			slog.Debug("docLoopManager: new connection request", "docId", newConnAndDocId.docId, "conn", fmt.Sprintf("%p", newConnAndDocId.conn))
			// create docLoop if DNE
			_, ok := docLoops[newConnAndDocId.docId]
			if !ok {
				slog.Debug("docLoopManager: creating new docLoop", "docId", newConnAndDocId.docId)
				// add to dict
				// closed when docLoop dies
				joiningConns := make(chan *websocket.Conn, 16)
				docLoops[newConnAndDocId.docId] = joiningConns

				// start the docloop
				go docLoop(docLoops[newConnAndDocId.docId], newConnAndDocId.docId, dyingDocLoopIds)
			} else {
				slog.Debug("docLoopManager: docLoop already exists", "docId", newConnAndDocId.docId)
			}

			// send the connection to the docloop
			slog.Debug("docLoopManager: sending connection to docLoop", "docId", newConnAndDocId.docId, "conn", fmt.Sprintf("%p", newConnAndDocId.conn))
			docLoops[newConnAndDocId.docId] <- newConnAndDocId.conn

		case dyingDocLoopId := <-dyingDocLoopIds:
			// clear docloop resources
			slog.Debug("docLoopManager: docLoop died, cleaning up", "docId", dyingDocLoopId)
			close(docLoops[dyingDocLoopId])
			delete(docLoops, dyingDocLoopId)
		}
	}
}

func main() {
	// Set up debug-level logging
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	logger := slog.New(handler)
	slog.SetDefault(logger)
	slog.Debug("main: debug-level logging enabled")

	// set up server
	const addr = ":8080"
	slog.Debug("main: initializing server", "addr", addr)

	// start doc loop manager
	connsAndDocIds := make(chan connAndDocId, 16)
	go docLoopManager(connsAndDocIds)
	slog.Debug("main: docLoopManager started")

	mux := http.NewServeMux() // mux pointer

	// accepts user WS connection and starts goroutine to send / receive changes
	mux.HandleFunc("/document/{docId}", func(w http.ResponseWriter, r *http.Request) {
		docId := r.PathValue("docId")
		slog.Debug("main: WebSocket connection request", "docId", docId, "remoteAddr", r.RemoteAddr)
		// upgrade to websocket
		// conn will be closed by the userLoop when it does
		// Configure origin checking to allow localhost:5173 (Vite dev server)
		// TODO: remove cors in production
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
			slog.Error("main: WebSocket accept error", "docId", docId, "error", err)
			fmt.Println(err.Error())
			return
		}

		slog.Debug("main: WebSocket connection accepted", "docId", docId, "conn", fmt.Sprintf("%p", conn))
		// send connection to doc manager
		connsAndDocIds <- connAndDocId{conn, docId}
		slog.Debug("main: connection sent to docLoopManager", "docId", docId)
	})

	s := &http.Server{
		Addr:    addr,
		Handler: cors.Default().Handler(mux),
	}

	slog.Info("Server starting", "addr", addr)
	fmt.Printf("Listening on %s...\n", addr)
	if err := s.ListenAndServe(); err != nil {
		slog.Error("Server error", "error", err)
	}
}
