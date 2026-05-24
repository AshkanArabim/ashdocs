package main

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"time"

	"github.com/automerge/automerge-go"
	"github.com/cockroachdb/pebble"
)

// After this many incremental saves, compact everything into a single snapshot.
const snapshotThreshold = 100

type PebbleStorage struct {
	db            *pebble.DB
	lastSaveTimes map[string]time.Time
	changeCounts  map[string]int
}

func NewPebbleStorage(dir string) (*PebbleStorage, error) {
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		return nil, fmt.Errorf("NewPebbleStorage: failed to open Pebble: %w", err)
	}
	return &PebbleStorage{
		db:            db,
		lastSaveTimes: make(map[string]time.Time),
		changeCounts:  make(map[string]int),
	}, nil
}

// Key schema (null byte as separator):
//   <docId>\x00snapshot          full Automerge save
//   <docId>\x00seq               big-endian uint64 sequence counter
//   <docId>\x00changes\x00<seq>  incremental saves, ordered for replay

func makeSnapshotKey(docId string) []byte {
	return []byte(docId + "\x00snapshot")
}

func makeSeqKey(docId string) []byte {
	return []byte(docId + "\x00seq")
}

func makeChangeKey(docId string, seq int) []byte {
	seqBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(seqBytes, uint64(seq))
	return append([]byte(docId+"\x00changes\x00"), seqBytes...)
}

func makeChangesPrefix(docId string) []byte {
	return []byte(docId + "\x00changes\x00")
}

func makeDocPrefix(docId string) []byte {
	return []byte(docId + "\x00")
}

// prefixUpperBound returns the smallest key greater than all keys with the given prefix.
func prefixUpperBound(prefix []byte) []byte {
	upper := make([]byte, len(prefix))
	copy(upper, prefix)
	for i := len(upper) - 1; i >= 0; i-- {
		if upper[i] < 0xFF {
			upper[i]++
			return upper
		}
		upper[i] = 0
	}
	return nil
}

// CreateOrLoadDoc loads the document from Pebble (snapshot + any subsequent incremental
// changes), or returns a blank document if none exists.
func (s *PebbleStorage) CreateOrLoadDoc(docId string) (*automerge.Doc, error) {
	slog.Debug("CreateOrLoadDoc: loading", "docId", docId)

	doc := automerge.New()

	// Load snapshot if present.
	val, closer, err := s.db.Get(makeSnapshotKey(docId))
	if err == nil {
		doc, err = automerge.Load(val)
		closer.Close()
		if err != nil {
			return nil, fmt.Errorf("CreateOrLoadDoc: failed to load snapshot: %w", err)
		}
		slog.Debug("CreateOrLoadDoc: snapshot loaded", "docId", docId)
	} else if err != pebble.ErrNotFound {
		return nil, fmt.Errorf("CreateOrLoadDoc: failed to get snapshot: %w", err)
	}

	// Apply any incremental changes on top of the snapshot.
	changesPrefix := makeChangesPrefix(docId)
	iter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: changesPrefix,
		UpperBound: prefixUpperBound(changesPrefix),
	})
	if err != nil {
		return nil, fmt.Errorf("CreateOrLoadDoc: failed to create iterator: %w", err)
	}
	defer iter.Close()

	changeCount := 0
	for iter.First(); iter.Valid(); iter.Next() {
		changeCount++
		if err := doc.LoadIncremental(iter.Value()); err != nil {
			return nil, fmt.Errorf("CreateOrLoadDoc: failed to apply incremental change %d: %w", changeCount, err)
		}
	}
	slog.Debug("CreateOrLoadDoc: done", "docId", docId, "incrementalChanges", changeCount)

	return doc, nil
}

// DeleteDoc removes all keys for the document.
func (s *PebbleStorage) DeleteDoc(docId string) error {
	docPrefix := makeDocPrefix(docId)
	if err := s.db.DeleteRange(docPrefix, prefixUpperBound(docPrefix), &pebble.WriteOptions{}); err != nil {
		return fmt.Errorf("DeleteDoc: %w", err)
	}
	delete(s.lastSaveTimes, docId)
	delete(s.changeCounts, docId)
	slog.Debug("DeleteDoc: deleted", "docId", docId)
	return nil
}

// compactSnapshot replaces all incremental changes for a document with a single full
// snapshot, capping future replay time to zero incremental changes.
func (s *PebbleStorage) compactSnapshot(docId string, doc *automerge.Doc) error {
	slog.Debug("compactSnapshot: starting", "docId", docId)
	batch := s.db.NewBatch()
	defer batch.Close()

	if err := batch.Set(makeSnapshotKey(docId), doc.Save(), nil); err != nil {
		return fmt.Errorf("compactSnapshot: write snapshot: %w", err)
	}

	changesPrefix := makeChangesPrefix(docId)
	if err := batch.DeleteRange(changesPrefix, prefixUpperBound(changesPrefix), nil); err != nil {
		return fmt.Errorf("compactSnapshot: delete changes: %w", err)
	}

	// Reset sequence counter so future change keys start from 1 again.
	seqBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(seqBytes, 0)
	if err := batch.Set(makeSeqKey(docId), seqBytes, nil); err != nil {
		return fmt.Errorf("compactSnapshot: reset seq: %w", err)
	}

	if err := batch.Commit(&pebble.WriteOptions{Sync: true}); err != nil {
		return fmt.Errorf("compactSnapshot: commit: %w", err)
	}

	s.changeCounts[docId] = 0
	slog.Debug("compactSnapshot: done", "docId", docId)
	return nil
}

// SaveDocChanges appends an incremental change for the document, throttled to at most
// one write per 100ms. Every snapshotThreshold saves it compacts everything into a
// full snapshot to cap replay time.
func (s *PebbleStorage) SaveDocChanges(docId string, doc *automerge.Doc) error {
	now := time.Now()
	if last, ok := s.lastSaveTimes[docId]; ok && now.Sub(last) < 100*time.Millisecond {
		return nil
	}
	s.lastSaveTimes[docId] = now

	batch := s.db.NewBatch()
	defer batch.Close()

	// Read and increment sequence number.
	seqKey := makeSeqKey(docId)
	var seq int
	seqBytes, closer, err := s.db.Get(seqKey)
	if err == nil {
		seq = int(binary.BigEndian.Uint64(seqBytes))
		closer.Close()
	} else if err != pebble.ErrNotFound {
		return fmt.Errorf("SaveDocChanges: read seq: %w", err)
	}
	seq++

	newSeqBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(newSeqBytes, uint64(seq))
	if err := batch.Set(seqKey, newSeqBytes, nil); err != nil {
		return fmt.Errorf("SaveDocChanges: set seq: %w", err)
	}
	if err := batch.Set(makeChangeKey(docId, seq), doc.SaveIncremental(), nil); err != nil {
		return fmt.Errorf("SaveDocChanges: set change: %w", err)
	}
	if err := batch.Commit(&pebble.WriteOptions{Sync: true}); err != nil {
		return fmt.Errorf("SaveDocChanges: commit: %w", err)
	}

	s.changeCounts[docId]++
	if s.changeCounts[docId] >= snapshotThreshold {
		if err := s.compactSnapshot(docId, doc); err != nil {
			slog.Error("SaveDocChanges: snapshot compaction failed", "docId", docId, "error", err)
			// Incremental save succeeded; snapshot is an optimisation, not required.
		}
	}

	return nil
}
