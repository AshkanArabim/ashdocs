package main

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/automerge/automerge-go"
)

// createOrLoadDoc loads a document from the database if it exists, or creates a new blank document.
// It loads the snapshot (if present) and applies all incremental changes on top of it.
// Returns the document and any error encountered during loading.
func createOrLoadDoc(db *fdb.Database, docId string) (*automerge.Doc, error) {
	slog.Debug("createOrLoadDoc: attempting to load doc from DB", "docId", docId)
	docInterface, err := db.Transact(func(t fdb.Transaction) (interface{}, error) {
		// load doc snapshot if exists
		val := t.Get(tuple.Tuple{docId, "snapshot"}.FDBKey()).MustGet()
		doc := automerge.New() // blank doc if snapshot DNE
		if val != nil {
			var err error
			doc, err = automerge.Load(val)
			if err != nil {
				return nil, err
			}
		}

		// apply all incremental changes on top of snapshot if they exist
		pr, err := fdb.PrefixRange(tuple.Tuple{docId, "changes"}.Pack())
		if err != nil {
			return nil, err
		}
		kvs := t.GetRange(pr, fdb.RangeOptions{}).GetSliceOrPanic()
		for _, kv := range kvs {
			// assuming loop won't run if size of prefix range is 0
			err := doc.LoadIncremental(kv.Value)
			if err != nil {
				return nil, err
			}
		}

		return doc, nil
	})
	if err != nil {
		return nil, fmt.Errorf("createOrLoadDoc: failed to load doc from DB: %w", err)
	}
	doc, ok := docInterface.(*automerge.Doc)
	if !ok {
		return nil, fmt.Errorf("createOrLoadDoc: unexpected type from DB transaction")
	}
	return doc, nil
}

// deleteDoc deletes a document and all its associated data (snapshot and changes) from the database.
func deleteDoc(db *fdb.Database, docId string) error {
	slog.Debug("deleteDoc: deleting document", "docId", docId)
	_, err := db.Transact(func(t fdb.Transaction) (interface{}, error) {
		// Delete everything in the docId namespace using a single range delete
		pr, err := fdb.PrefixRange(tuple.Tuple{docId}.Pack())
		if err != nil {
			return nil, fmt.Errorf("deleteDoc: failed to create prefix range: %w", err)
		}
		t.ClearRange(pr)
		return nil, nil
	})
	if err != nil {
		return fmt.Errorf("deleteDoc: failed to delete doc from DB: %w", err)
	}
	slog.Debug("deleteDoc: document deleted successfully", "docId", docId)
	return nil
}

var lastSaveTimes = make(map[string]time.Time)

// saveDocChanges saves document changes to the database.
// This function should be throttled and periodically create snapshots.
// It saves the most recent changes and periodically creates snapshots for efficiency.
func saveDocChanges(db *fdb.Database, docId string, doc *automerge.Doc) error {
	// Throttle: return early if called within 100ms of last save for this docId
	lastSave, exists := lastSaveTimes[docId]
	now := time.Now()
	if exists && now.Sub(lastSave) < 100*time.Millisecond {
		slog.Debug("saveDocChanges: throttled, skipping save", "docId", docId, "timeSinceLastSave", now.Sub(lastSave))
		return nil
	}
	lastSaveTimes[docId] = now

	slog.Debug("saveDocChanges: proceeding with save", "docId", docId)

	// using SaveIncremental. will consider other methods if this doesn't work
	_, err := db.Transact(func(t fdb.Transaction) (interface{}, error) {
		// update / create iterator for this doc
		// Increment by 1 (64-bit little-endian)
		t.Add(tuple.Tuple{docId, "seq"}.FDBKey(), []byte{1, 0, 0, 0, 0, 0, 0, 0})

		// get the new seq
		bytes := t.Get(tuple.Tuple{docId, "seq"}.FDBKey()).MustGet()
		var seq int
		if bytes != nil {
			// decode 64-bit little endian
			seq = int(binary.LittleEndian.Uint64(bytes))
		}

		// save with a seq as the key
		t.Set(tuple.Tuple{docId, "changes", seq}.FDBKey(), doc.SaveIncremental())

		return nil, nil
	})

	return err
}
