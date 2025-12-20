package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/automerge/automerge-go"
	"github.com/coder/websocket"
)

type wsHandler struct{} // implements the Handler interface
type userReadLoopResult struct {
	mt  websocket.MessageType
	b   []byte
	err error
}

func WSReadLoop(ctx context.Context, conn *websocket.Conn, readClientChanges chan userReadLoopResult) {
	defer close(readClientChanges)

	// loop to read continuously
	for {
		mt, b, err := conn.Read(ctx)
		readClientChanges <- userReadLoopResult{mt, b, err}

		if err != nil {
			fmt.Println(err.Error())
			return
		}
	}
}

// each connected user has an instance of this func running
func HandleUserConn(conn *websocket.Conn, serverChanges chan *automerge.SyncMessage, clientChanges chan *automerge.SyncMessage) {
	defer conn.CloseNow()
	defer close(serverChanges)
	defer close(clientChanges)

	// FIXME: handle deliberate disconnection

	// TODO: figure out a better context solution
	ctx := context.Background()
	readClientChanges := make(chan userReadLoopResult, 5)
	go WSReadLoop(ctx, conn, readClientChanges)

	for {
		select {
		case serverChange := <-serverChanges:
			// send broadcast changes sent by the server; send to client
			conn.Write(ctx, websocket.MessageType(websocket.MessageBinary), serverChange.Bytes())

		case readClientChange := <-readClientChanges:
			// client sent changes to server; forward to doc handler
			b := readClientChange.b
			err := readClientChange.err
			if err != nil {
				fmt.Println(err.Error())
				return
			}

			// decode & send syncmessage to doc handler
			clientChange, err := automerge.LoadSyncMessage(b)
			if err != nil {
				fmt.Println(err.Error())
				return
			}
			clientChanges <- clientChange
		}
	}
}

// handles each new user connection to a document
// TODO: convert this to be a new instance for every new document.
// FIXME: handle the case when HandleUserConn returns. TBD if both failure or also clean close
func (s *wsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// upgrade to websocket
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	defer conn.CloseNow()

	// set up automerge server
	// since all in memory, starting with blank doc & syncstate
	doc := automerge.New()
	syncstate := automerge.NewSyncState(doc)

  // launch user connection handler goroutine
  serverChanges := make(chan *automerge.SyncMessage, 5)
  clientChanges := make(chan *automerge.SyncMessage, 5)
  go HandleUserConn(conn, serverChanges, clientChanges)

}

func main() {
	// set up server
	const origin = ":8080"

	s := &http.Server{
		Addr:    origin + "/document/0", // hardcoding doc id for now
		Handler: &wsHandler{},
	}

	fmt.Printf("Listening on %s...", origin)
	s.ListenAndServe()
}
