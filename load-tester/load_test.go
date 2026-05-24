package main

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/automerge/automerge-go"
	"github.com/coder/websocket"
)

// Client represents a single client connection to the backend
type Client struct {
	docId       string
	conn        *websocket.Conn
	doc         *automerge.Doc
	syncState   *automerge.SyncState
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	editCount   int64
	errorCount  int64
	mu          sync.Mutex
	text        string
	cursorPos   int
	msgReceived chan struct{}
}

// NewClient creates a new client and connects to the backend
func NewClient(docId string, serverURL string) (*Client, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// Connect to WebSocket
	url := fmt.Sprintf("ws://%s/document/%s", serverURL, docId)
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to connect: %w", err)
	}

	// Initialize Automerge document
	doc := automerge.New()
	syncState := automerge.NewSyncState(doc)

	client := &Client{
		docId:       docId,
		conn:        conn,
		doc:         doc,
		syncState:   syncState,
		ctx:         ctx,
		cancel:      cancel,
		cursorPos:   0,
		msgReceived: make(chan struct{}, 1),
	}

	// Start receiving messages
	client.wg.Add(1)
	go client.receiveLoop()

	// Send initial sync message
	client.sendSyncMessage()

	return client, nil
}

// receiveLoop continuously receives messages from the server
func (c *Client) receiveLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			mt, b, err := c.conn.Read(c.ctx)
			if err != nil {
				atomic.AddInt64(&c.errorCount, 1)
				return
			}

			if mt == websocket.MessageBinary {
				c.mu.Lock()
				_, err = c.syncState.ReceiveMessage(b)
				if err != nil {
					atomic.AddInt64(&c.errorCount, 1)
					c.mu.Unlock()
					continue
				}

				// Update local text from document
				textVal, err := c.doc.Path("text").Get()
				if err == nil && textVal != nil {
					if text, err := automerge.As[string](textVal); err == nil {
						c.text = text
						// Keep cursor within bounds
						if c.cursorPos > len(c.text) {
							c.cursorPos = len(c.text)
						}
					}
				}
				c.mu.Unlock()

				// Send sync message back
				c.sendSyncMessage()

				select {
				case c.msgReceived <- struct{}{}:
				default:
				}
			}
		}
	}
}

// WaitForMessage blocks until the server sends a message or the timeout elapses.
func (c *Client) WaitForMessage(timeout time.Duration) bool {
	select {
	case <-c.msgReceived:
		return true
	case <-time.After(timeout):
		return false
	}
}

// sendSyncMessageLocked sends a sync message; caller must hold c.mu.
func (c *Client) sendSyncMessageLocked() {
	msg, valid := c.syncState.GenerateMessage()
	if valid {
		bytes := msg.Bytes()
		ctx, cancel := context.WithTimeout(c.ctx, 500*time.Millisecond)
		defer cancel()
		err := c.conn.Write(ctx, websocket.MessageBinary, bytes)
		if err != nil {
			atomic.AddInt64(&c.errorCount, 1)
		}
	}
}

// sendSyncMessage acquires the lock and sends a sync message.
func (c *Client) sendSyncMessage() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sendSyncMessageLocked()
}

// Edit performs an edit operation (insert or delete)
func (c *Client) Edit() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Get current text
	textVal, err := c.doc.Path("text").Get()
	var currentText string
	if err == nil && textVal != nil {
		if text, err := automerge.As[string](textVal); err == nil {
			currentText = text
		}
	}

	// Decide on edit type: 80% insert, 20% delete
	editType := rand.Float32()
	var newText string

	if editType < 0.2 && len(currentText) > 0 && c.cursorPos > 0 {
		// Delete 1 character (backspace)
		newText = currentText[:c.cursorPos-1] + currentText[c.cursorPos:]
		c.cursorPos--
	} else {
		// Insert 1-3 characters
		insertLen := rand.Intn(3) + 1
		chars := make([]byte, insertLen)
		for i := range chars {
			chars[i] = byte('a' + rand.Intn(26))
		}
		insertText := string(chars)

		if c.cursorPos > len(currentText) {
			c.cursorPos = len(currentText)
		}
		newText = currentText[:c.cursorPos] + insertText + currentText[c.cursorPos:]
		c.cursorPos += len(insertText)
	}

	// Update document
	err = c.doc.Path("text").Set(newText)
	if err != nil {
		return fmt.Errorf("failed to update document: %w", err)
	}

	c.text = newText
	atomic.AddInt64(&c.editCount, 1)

	// Send sync message
	c.sendSyncMessageLocked()

	return nil
}

// MoveCursor moves the cursor to a random position
func (c *Client) MoveCursor() {
	c.mu.Lock()
	defer c.mu.Unlock()

	textVal, err := c.doc.Path("text").Get()
	var textLen int
	if err == nil && textVal != nil {
		if text, err := automerge.As[string](textVal); err == nil {
			textLen = len(text)
		}
	}

	if textLen > 0 {
		c.cursorPos = rand.Intn(textLen + 1) // +1 to allow end of string
	} else {
		c.cursorPos = 0
	}
}

// Close closes the client connection
func (c *Client) Close() {
	c.cancel()
	c.conn.CloseNow()
	c.wg.Wait()
}

// GetStats returns edit and error counts
func (c *Client) GetStats() (int64, int64) {
	return atomic.LoadInt64(&c.editCount), atomic.LoadInt64(&c.errorCount)
}

// BenchmarkSingleDocContention tests single document with increasing users
func BenchmarkSingleDocContention(b *testing.B) {
	serverURL := "localhost:8080"
	fixedEditRate := 10 // edits per second per user
	runId := rand.Int63()

	for numUsers := 1; numUsers <= 50; numUsers *= 2 {
		docId := fmt.Sprintf("test-contention-%d-%d", runId, numUsers)
		b.Run(fmt.Sprintf("Users_%d", numUsers), func(b *testing.B) {
			clients := make([]*Client, numUsers)
			var err error

			// Create clients
			for i := 0; i < numUsers; i++ {
				clients[i], err = NewClient(docId, serverURL)
				if err != nil {
					b.Fatalf("Failed to create client %d: %v", i, err)
				}
				defer clients[i].Close()
			}

			// Wait for initial sync
			time.Sleep(500 * time.Millisecond)

			// Run benchmark
			b.ResetTimer()
			editInterval := time.Second / time.Duration(fixedEditRate)
			done := make(chan struct{})
			var wg sync.WaitGroup

			// Start edit goroutines for each client
			for i := 0; i < numUsers; i++ {
				wg.Add(1)
				client := clients[i]
				go func() {
					defer wg.Done()
					ticker := time.NewTicker(editInterval)
					defer ticker.Stop()

					for {
						select {
						case <-done:
							return
						case <-ticker.C:
							// Occasionally move cursor
							if rand.Float32() < 0.1 {
								client.MoveCursor()
							}
							client.Edit()
						}
					}
				}()
			}

			// Run for fixed test duration (10 seconds)
			testDuration := 10 * time.Second
			b.StopTimer()
			time.Sleep(testDuration)
			b.StartTimer()
			close(done)
			wg.Wait()

			// Report stats
			var totalEdits, totalErrors int64
			for _, client := range clients {
				edits, errors := client.GetStats()
				totalEdits += edits
				totalErrors += errors
			}
			b.ReportMetric(float64(totalEdits)/testDuration.Seconds(), "edits/sec")
			b.ReportMetric(float64(totalEdits), "total-edits")
			b.ReportMetric(float64(totalErrors), "errors")
		})
	}
}

// BenchmarkThroughputSaturation tests throughput with fixed users and increasing edit rate
func BenchmarkThroughputSaturation(b *testing.B) {
	serverURL := "localhost:8080"
	fixedUsers := 25
	runId := rand.Int63()

	for editsPerSec := 5; editsPerSec <= 100; editsPerSec *= 2 {
		docId := fmt.Sprintf("test-throughput-%d-%d", runId, editsPerSec)
		b.Run(fmt.Sprintf("EditsPerSec_%d", editsPerSec), func(b *testing.B) {
			clients := make([]*Client, fixedUsers)
			var err error

			// Create clients
			for i := 0; i < fixedUsers; i++ {
				clients[i], err = NewClient(docId, serverURL)
				if err != nil {
					b.Fatalf("Failed to create client %d: %v", i, err)
				}
				defer clients[i].Close()
			}

			// Wait for initial sync
			time.Sleep(500 * time.Millisecond)

			// Run benchmark
			b.ResetTimer()
			editInterval := time.Second / time.Duration(editsPerSec)
			done := make(chan struct{})
			var wg sync.WaitGroup

			// Start edit goroutines for each client
			for i := 0; i < fixedUsers; i++ {
				wg.Add(1)
				client := clients[i]
				go func() {
					defer wg.Done()
					ticker := time.NewTicker(editInterval)
					defer ticker.Stop()

					for {
						select {
						case <-done:
							return
						case <-ticker.C:
							// Occasionally move cursor
							if rand.Float32() < 0.1 {
								client.MoveCursor()
							}
							client.Edit()
						}
					}
				}()
			}

			// Run for fixed test duration (10 seconds)
			testDuration := 10 * time.Second
			b.StopTimer()
			time.Sleep(testDuration)
			b.StartTimer()
			close(done)
			wg.Wait()

			// Report stats
			var totalEdits, totalErrors int64
			for _, client := range clients {
				edits, errors := client.GetStats()
				totalEdits += edits
				totalErrors += errors
			}
			b.ReportMetric(float64(totalEdits)/testDuration.Seconds(), "edits/sec")
			b.ReportMetric(float64(totalEdits), "total-edits")
			b.ReportMetric(float64(totalErrors), "errors")
		})
	}
}

// BenchmarkManyDocFanout tests many documents with 1-2 clients per doc
func BenchmarkManyDocFanout(b *testing.B) {
	serverURL := "localhost:8080"
	fixedEditRate := 10 // edits per second per client

	for numDocs := 1; numDocs <= 1024; numDocs *= 2 {
		b.Run(fmt.Sprintf("Docs_%d", numDocs), func(b *testing.B) {
			var allClients []*Client
			var wg sync.WaitGroup

			// Create 1-2 clients per document
			runId := rand.Int63()
			for docNum := 0; docNum < numDocs; docNum++ {
				docId := fmt.Sprintf("test-doc-fanout-%d-%d", runId, docNum)
				numClientsForDoc := 1
				if rand.Float32() < 0.5 {
					numClientsForDoc = 2
				}

				for i := 0; i < numClientsForDoc; i++ {
					client, err := NewClient(docId, serverURL)
					if err != nil {
						b.Fatalf("Failed to create client for doc %s: %v", docId, err)
					}
					allClients = append(allClients, client)
					defer client.Close()
				}
			}

			// Wait for initial sync
			time.Sleep(500 * time.Millisecond)

			// Run benchmark
			b.ResetTimer()
			editInterval := time.Second / time.Duration(fixedEditRate)
			done := make(chan struct{})

			// Start edit goroutines for each client
			for _, client := range allClients {
				wg.Add(1)
				go func(c *Client) {
					defer wg.Done()
					ticker := time.NewTicker(editInterval)
					defer ticker.Stop()

					for {
						select {
						case <-done:
							return
						case <-ticker.C:
							// Occasionally move cursor
							if rand.Float32() < 0.1 {
								c.MoveCursor()
							}
							c.Edit()
						}
					}
				}(client)
			}

			// Run for fixed test duration (10 seconds)
			testDuration := 10 * time.Second
			b.StopTimer()
			time.Sleep(testDuration)
			b.StartTimer()
			close(done)
			wg.Wait()

			// Report stats
			var totalEdits, totalErrors int64
			for _, client := range allClients {
				edits, errors := client.GetStats()
				totalEdits += edits
				totalErrors += errors
			}
			b.ReportMetric(float64(totalEdits)/testDuration.Seconds(), "edits/sec")
			b.ReportMetric(float64(totalEdits), "total-edits")
			b.ReportMetric(float64(totalErrors), "errors")

			// Let the server finish tearing down docLoops before the next sub-benchmark
			// connects; without this, new connections arrive while old docLoops are
			// mid-shutdown and get stranded on a dead joiningConns channel.
			time.Sleep(200 * time.Millisecond)
		})
	}
}

// BenchmarkRoundTripLatency measures edit-to-acknowledgement latency under varying
// numbers of concurrent writers on the same document.
func BenchmarkRoundTripLatency(b *testing.B) {
	serverURL := "localhost:8080"
	testDuration := 5 * time.Second
	editInterval := 50 * time.Millisecond // 20 edits/sec from the measured client

	for _, numBystanders := range []int{0, 5, 20} {
		b.Run(fmt.Sprintf("Bystanders_%d", numBystanders), func(b *testing.B) {
			docId := fmt.Sprintf("test-latency-%d-%d", numBystanders, rand.Int63())

			// Bystanders write continuously to create realistic server load.
			bystanders := make([]*Client, numBystanders)
			for i := range bystanders {
				var err error
				bystanders[i], err = NewClient(docId, serverURL)
				if err != nil {
					b.Fatalf("failed to create bystander: %v", err)
				}
				defer bystanders[i].Close()
			}

			measured, err := NewClient(docId, serverURL)
			if err != nil {
				b.Fatalf("failed to create measured client: %v", err)
			}
			defer measured.Close()

			// Wait for initial sync across all clients.
			time.Sleep(300 * time.Millisecond)

			// Start bystander edit loops.
			bystanderDone := make(chan struct{})
			var bystanderWg sync.WaitGroup
			for _, c := range bystanders {
				bystanderWg.Add(1)
				go func(c *Client) {
					defer bystanderWg.Done()
					ticker := time.NewTicker(editInterval)
					defer ticker.Stop()
					for {
						select {
						case <-bystanderDone:
							return
						case <-ticker.C:
							c.Edit()
						}
					}
				}(c)
			}

			// Collect latency samples from the measured client.
			var latencies []time.Duration
			deadline := time.Now().Add(testDuration)

			b.ResetTimer()
			b.StopTimer()
			for time.Now().Before(deadline) {
				start := time.Now()
				if err := measured.Edit(); err != nil {
					b.Errorf("edit failed: %v", err)
					break
				}
				measured.WaitForMessage(2 * time.Second)
				latencies = append(latencies, time.Since(start))
				time.Sleep(editInterval)
			}
			b.StartTimer()

			close(bystanderDone)
			bystanderWg.Wait()

			if len(latencies) == 0 {
				return
			}
			var total time.Duration
			minL, maxL := latencies[0], latencies[0]
			for _, l := range latencies {
				total += l
				if l < minL {
					minL = l
				}
				if l > maxL {
					maxL = l
				}
			}
			avg := total / time.Duration(len(latencies))
			b.ReportMetric(float64(avg.Microseconds()), "avg_μs/op")
			b.ReportMetric(float64(minL.Microseconds()), "min_μs/op")
			b.ReportMetric(float64(maxL.Microseconds()), "max_μs/op")
			b.ReportMetric(float64(len(latencies)), "samples")
		})
	}
}
