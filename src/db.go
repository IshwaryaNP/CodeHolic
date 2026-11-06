package src

import (
	"fmt"
	"os"
	"sync"
)

// dbFile defines the unexported file interface required by DB.
// *os.File satisfies it, and tests can inject mock wrappers.
type dbFile interface {
	Write([]byte) (int, error)
	ReadAt([]byte, int64) (int, error)
	Sync() error
	Truncate(int64) error
	Close() error
}

// SyncPolicy dictates when writes are flushed to durable storage.
type SyncPolicy int

const (
	// SyncAlways flushes (fsync) after every single write operation (Put, Delete, Write batch).
	// Provides the strongest durability guarantees against crashes or power loss at the expense of IO latency.
	SyncAlways SyncPolicy = iota

	// SyncOnBatch flushes (fsync) only when committing a Batch, but skips fsync on individual Put/Delete calls.
	// Balances high throughput for single operations with durability boundaries on atomic batches.
	SyncOnBatch

	// SyncNever leaves flushing entirely to the operating system's buffer cache.
	// Offers maximum throughput, but recent writes may be lost if an OS crash or power failure occurs.
	SyncNever
)

// Options configures the DB engine behavior.
type Options struct {
	// SyncPolicy specifies the fsync persistence strategy. Default is SyncAlways.
	SyncPolicy SyncPolicy
}

// DefaultOptions returns standard configuration with SyncAlways.
func DefaultOptions() *Options {
	return &Options{
		SyncPolicy: SyncAlways,
	}
}

// DB represents the append-only key-value storage engine.
// Fields are unexported and match Member 2's contract for compaction.
type DB struct {
	path        string
	file        dbFile
	mu          sync.RWMutex
	index       *Index
	writeOffset int64
	closed      bool
	broken      bool
	syncPolicy  SyncPolicy

	// encBuf is a reusable per-DB byte buffer for encoding records,
	// guarded by DB.mu (Write lock) to avoid allocations on writes.
	encBuf []byte
}

// Open opens or creates an append-only database file at the given path.
// It initiates recovery via Recover(path) to rebuild the index and determine validEnd.
func Open(path string, opts *Options) (*DB, error) {
	if opts == nil {
		opts = DefaultOptions()
	}

	idx, validEnd, err := Recover(path)
	if err != nil {
		return nil, fmt.Errorf("recovery failed: %w", err)
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}

	db := &DB{
		path:        path,
		file:        f,
		index:       idx,
		writeOffset: validEnd,
		closed:      false,
		broken:      false,
		syncPolicy:  opts.SyncPolicy,
		encBuf:      make([]byte, 0, 4096),
	}

	return db, nil
}

// Close flushes data to disk, closes the underlying file, and marks the DB as closed.
// It is idempotent; repeated calls return ErrClosed or nil.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return ErrClosed
	}

	db.closed = true

	var syncErr error
	if db.file != nil {
		syncErr = db.file.Sync()
		closeErr := db.file.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}

	return nil
}
