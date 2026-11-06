package src

import (
	"bytes"
	"fmt"
)

// Put inserts or updates a key-value pair in the database.
// It takes a write lock, checks closed/broken state, encodes the record into
// a reusable per-DB buffer (guarded by db.mu to avoid per-write heap allocations),
// writes to the append-only file, flushes per SyncPolicy, and updates the index.
func (db *DB) Put(key, val []byte) error {
	if len(key) == 0 {
		return ErrEmptyKey
	}
	if len(key) > MaxKeySize {
		return ErrKeyTooLarge
	}
	if len(val) > MaxValueSize {
		return ErrValueTooLarge
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return ErrClosed
	}
	if db.broken {
		return ErrBroken
	}

	writeOffsetBefore := db.writeOffset

	// Reuse per-DB buffer guarded by write lock db.mu
	db.encBuf = db.encBuf[:0]
	db.encBuf = EncodeRecord(db.encBuf, OpPut, key, val)

	n, err := db.file.Write(db.encBuf)
	if err != nil || n != len(db.encBuf) {
		db.handleWriteFailure(writeOffsetBefore)
		if err != nil {
			return fmt.Errorf("write error: %w", err)
		}
		return fmt.Errorf("short write: wrote %d of %d bytes: %w", n, len(db.encBuf), ErrCorrupt)
	}

	if db.syncPolicy == SyncAlways {
		if err := db.file.Sync(); err != nil {
			db.handleWriteFailure(writeOffsetBefore)
			return fmt.Errorf("sync error: %w", err)
		}
	}

	db.index.Set(string(key), writeOffsetBefore)
	db.writeOffset += int64(n)
	return nil
}

// Delete marks a key as deleted.
// If the key is not present in the index, it returns nil immediately without writing to disk
// to avoid log pollution from non-existent deletions.
// If the key exists, an OpDelete tombstone (ValLen = 0) is appended following the same
// durability and rollback rules as Put, then removed from the index.
func (db *DB) Delete(key []byte) error {
	if len(key) == 0 {
		return ErrEmptyKey
	}
	if len(key) > MaxKeySize {
		return ErrKeyTooLarge
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return ErrClosed
	}
	if db.broken {
		return ErrBroken
	}

	// If key is not in index, skip appending tombstone
	if _, ok := db.index.Get(string(key)); !ok {
		return nil
	}

	writeOffsetBefore := db.writeOffset

	db.encBuf = db.encBuf[:0]
	db.encBuf = EncodeRecord(db.encBuf, OpDelete, key, nil)

	n, err := db.file.Write(db.encBuf)
	if err != nil || n != len(db.encBuf) {
		db.handleWriteFailure(writeOffsetBefore)
		if err != nil {
			return fmt.Errorf("write error: %w", err)
		}
		return fmt.Errorf("short write: wrote %d of %d bytes: %w", n, len(db.encBuf), ErrCorrupt)
	}

	if db.syncPolicy == SyncAlways {
		if err := db.file.Sync(); err != nil {
			db.handleWriteFailure(writeOffsetBefore)
			return fmt.Errorf("sync error: %w", err)
		}
	}

	db.index.Delete(string(key))
	db.writeOffset += int64(n)
	return nil
}

// Get retrieves the value for key from the database.
// It acquires an RLock, reads the record offset from index, reads and validates
// the 15-byte header, bounds-checks against writeOffset, reads the key+value payload,
// verifies the CRC32, ensures OpPut and key match, and returns a newly allocated COPY
// of the value so callers can safely mutate it without data races.
func (db *DB) Get(key []byte) ([]byte, error) {
	if len(key) == 0 {
		return nil, ErrEmptyKey
	}
	if len(key) > MaxKeySize {
		return nil, ErrKeyTooLarge
	}

	db.mu.RLock()
	defer db.mu.RUnlock()

	if db.closed {
		return nil, ErrClosed
	}
	if db.broken {
		return nil, ErrBroken
	}

	offset, ok := db.index.Get(string(key))
	if !ok {
		return nil, ErrNotFound
	}

	// Header read
	hdrBuf := make([]byte, HeaderSize)
	if _, err := db.file.ReadAt(hdrBuf, offset); err != nil {
		return nil, fmt.Errorf("%w: failed to read header: %v", ErrCorrupt, err)
	}

	hdr, err := DecodeHeader(hdrBuf)
	if err != nil {
		return nil, fmt.Errorf("%w: header decode failed: %v", ErrCorrupt, err)
	}

	if hdr.OpType != OpPut {
		return nil, fmt.Errorf("%w: expected OpPut (%d) but found op %d", ErrCorrupt, OpPut, hdr.OpType)
	}

	// Bounds check against writeOffset
	if offset+int64(hdr.TotalLen) > db.writeOffset {
		return nil, fmt.Errorf("%w: record extends past write offset", ErrCorrupt)
	}

	// Payload read
	keyValLen := int(hdr.TotalLen) - HeaderSize
	payload := make([]byte, keyValLen)
	if _, err := db.file.ReadAt(payload, offset+int64(HeaderSize)); err != nil {
		return nil, fmt.Errorf("%w: failed to read payload: %v", ErrCorrupt, err)
	}

	recKey := payload[:hdr.KeyLen]
	recVal := payload[hdr.KeyLen:]

	if !VerifyCRC(hdrBuf, recKey, recVal) {
		return nil, fmt.Errorf("%w: CRC verification failed", ErrCorrupt)
	}

	if !bytes.Equal(recKey, key) {
		return nil, fmt.Errorf("%w: key at offset does not match query key", ErrCorrupt)
	}

	// Always return an isolated copy
	valCopy := make([]byte, len(recVal))
	copy(valCopy, recVal)
	return valCopy, nil
}

// handleWriteFailure is called when a write or sync fails.
// It attempts to roll back the file to writeOffsetBefore via Truncate.
// If Truncate fails, the database is marked broken to avoid silent data corruption.
// Precondition: caller holds db.mu Lock.
func (db *DB) handleWriteFailure(writeOffsetBefore int64) {
	if err := db.file.Truncate(writeOffsetBefore); err != nil {
		db.broken = true
	}
}
