package main

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"time"

	"github.com/automerge/automerge-go"
	"github.com/cockroachdb/pebble"
)

// PebbleStorage implements the StorageSubsystem interface using Pebble.
type PebbleStorage struct {
	db            *pebble.DB
	lastSaveTimes map[string]time.Time
}

// NewPebbleStorage creates a new PebbleStorage instance and establishes the database connection.
func NewPebbleStorage(dir string) (*PebbleStorage, error) {
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		return nil, fmt.Errorf("NewPebbleStorage: failed to open Pebble: %w", err)
	}
	return &PebbleStorage{
		db:            db,
		lastSaveTimes: make(map[string]time.Time),
	}, nil
}

// key helpers for hierarchical keys
// Keys are structured as: docId + "\x00" + component + "\x00" + subcomponent
func makeSnapshotKey(docId string) []byte {
	return []byte(docId + "\x00snapshot")
}

func makeSeqKey(docId string) []byte {
	return []byte(docId + "\x00seq")
}

func makeChangeKey(docId string, seq int) []byte {
	key := docId + "\x00changes\x00"
	seqBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(seqBytes, uint64(seq))
	return append([]byte(key), seqBytes...)
}

func makeChangesPrefix(docId string) []byte {
	return []byte(docId + "\x00changes\x00")
}

func makeDocPrefix(docId string) []byte {
	return []byte(docId + "\x00")
}

// CreateOrLoadDoc loads a document from the database if it exists, or creates a new blank document.
// It loads the snapshot (if present) and applies all incremental changes on top of it.
// Returns the document and any error encountered during loading.
func (s *PebbleStorage) CreateOrLoadDoc(docId string) (*automerge.Doc, error) {
	slog.Debug("CreateOrLoadDoc: attempting to load doc from DB", "docId", docId)

	// Load snapshot if exists
	snapshotKey := makeSnapshotKey(docId)
	val, closer, err := s.db.Get(snapshotKey)
	doc := automerge.New() // blank doc if snapshot DNE
	if err == nil {
		if val != nil {
			doc, err = automerge.Load(val)
			closer.Close()
			if err != nil {
				return nil, fmt.Errorf("CreateOrLoadDoc: failed to load snapshot: %w", err)
			}
		} else {
			closer.Close()
		}
	} else if err != pebble.ErrNotFound {
		return nil, fmt.Errorf("CreateOrLoadDoc: failed to get snapshot: %w", err)
	}

	// Apply all incremental changes on top of snapshot if they exist
	changesPrefix := makeChangesPrefix(docId)
	iter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: changesPrefix,
		UpperBound: func() []byte {
			// Upper bound is the prefix incremented by 1
			upper := make([]byte, len(changesPrefix))
			copy(upper, changesPrefix)
			for i := len(upper) - 1; i >= 0; i-- {
				if upper[i] < 0xFF {
					upper[i]++
					break
				}
				upper[i] = 0
			}
			return upper
		}(),
	})
	if err != nil {
		return nil, fmt.Errorf("CreateOrLoadDoc: failed to create iterator: %w", err)
	}
	defer iter.Close()

	for iter.First(); iter.Valid(); iter.Next() {
		slog.Debug("CreateOrLoadDoc: loading incremental change", "docId", docId, "content", string(iter.Key()))
		err := doc.LoadIncremental(iter.Value())
		if err != nil {
			return nil, fmt.Errorf("CreateOrLoadDoc: failed to load incremental change: %w", err)
		}
	}

	slog.Debug("CreateOrLoadDoc: loaded doc", "docId", docId, "content", doc.Root().GoString())

	return doc, nil
}

// DeleteDoc deletes a document and all its associated data (snapshot and changes) from the database.
func (s *PebbleStorage) DeleteDoc(docId string) error {
	slog.Debug("DeleteDoc: deleting document", "docId", docId)

	docPrefix := makeDocPrefix(docId)
	// Delete everything in the docId namespace using a range delete
	err := s.db.DeleteRange(docPrefix, func() []byte {
		// Upper bound is the prefix incremented by 1
		upper := make([]byte, len(docPrefix))
		copy(upper, docPrefix)
		for i := len(upper) - 1; i >= 0; i-- {
			if upper[i] < 0xFF {
				upper[i]++
				break
			}
			upper[i] = 0
		}
		return upper
	}(), &pebble.WriteOptions{})
	if err != nil {
		return fmt.Errorf("DeleteDoc: failed to delete doc from DB: %w", err)
	}
	slog.Debug("DeleteDoc: document deleted successfully", "docId", docId)
	return nil
}

// SaveDocChanges saves document changes to the database.
// This function should be throttled and periodically create snapshots.
// It saves the most recent changes and periodically creates snapshots for efficiency.
func (s *PebbleStorage) SaveDocChanges(docId string, doc *automerge.Doc) error {
	// Throttle: return early if called within 100ms of last save for this docId
	lastSave, exists := s.lastSaveTimes[docId]
	now := time.Now()
	if exists && now.Sub(lastSave) < 100*time.Millisecond {
		slog.Debug("SaveDocChanges: throttled, skipping save", "docId", docId, "timeSinceLastSave", now.Sub(lastSave))
		return nil
	}
	s.lastSaveTimes[docId] = now

	slog.Debug("SaveDocChanges: proceeding with save", "docId", docId)

	// Use a batch for atomic operations
	batch := s.db.NewBatch()

	// Get current sequence number and increment
	seqKey := makeSeqKey(docId)
	seqBytes, closer, err := s.db.Get(seqKey)
	var seq int
	if err == nil {
		if seqBytes != nil {
			seq = int(binary.BigEndian.Uint64(seqBytes))
		}
		closer.Close()
	} else if err != pebble.ErrNotFound {
		batch.Close()
		return fmt.Errorf("SaveDocChanges: failed to get sequence: %w", err)
	}

	seq++ // Increment sequence

	// Update sequence number (using BigEndian for proper incremental sorting)
	seqBytes = make([]byte, 8)
	binary.BigEndian.PutUint64(seqBytes, uint64(seq))
	err = batch.Set(seqKey, seqBytes, nil)
	if err != nil {
		batch.Close()
		return fmt.Errorf("SaveDocChanges: failed to set sequence: %w", err)
	}

	// Save incremental change with sequence number as part of key
	changeKey := makeChangeKey(docId, seq)
	err = batch.Set(changeKey, doc.SaveIncremental(), nil)
	slog.Debug("SaveDocChanges: saved incremental change with key:", "docId", docId, "content", string(changeKey))
	if err != nil {
		batch.Close()
		return fmt.Errorf("SaveDocChanges: failed to set change: %w", err)
	}

	// Commit the batch
	err = batch.Commit(&pebble.WriteOptions{})
	batch.Close()
	if err != nil {
		return fmt.Errorf("SaveDocChanges: failed to commit batch: %w", err)
	}

	return nil
}
