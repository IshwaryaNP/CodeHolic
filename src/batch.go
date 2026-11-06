package src

import (
	"fmt"
)

// MaxBatchBufferSize defines the maximum permitted encoded batch size in bytes (128MB).
const MaxBatchBufferSize = 128 * 1024 * 1024

// batchOp represents an individual pending operation in a Batch.
type batchOp struct {
	op  byte
	key []byte
	val []byte
}

// Batch allows grouping multiple Put and Delete operations to be written atomically.
// Operations are buffered in memory and sizes are validated immediately upon addition.
type Batch struct {
	ops             []batchOp
	estimatedLength int
}

// NewBatch creates an empty Batch.
func NewBatch() *Batch {
	return &Batch{
		ops:             make([]batchOp, 0, 16),
		estimatedLength: HeaderSize * 2, // Accounting for BatchBegin and BatchCommit markers
	}
}

// Put adds a key-value write operation to the batch.
// Validates sizes immediately.
func (b *Batch) Put(key, val []byte) error {
	if len(key) == 0 {
		return ErrEmptyKey
	}
	if len(key) > MaxKeySize {
		return ErrKeyTooLarge
	}
	if len(val) > MaxValueSize {
		return ErrValueTooLarge
	}

	keyCopy := make([]byte, len(key))
	copy(keyCopy, key)

	valCopy := make([]byte, len(val))
	copy(valCopy, val)

	b.ops = append(b.ops, batchOp{
		op:  OpPut,
		key: keyCopy,
		val: valCopy,
	})
	b.estimatedLength += HeaderSize + len(key) + len(val)
	return nil
}

// Delete adds a key deletion to the batch.
// Validates sizes immediately.
func (b *Batch) Delete(key []byte) error {
	if len(key) == 0 {
		return ErrEmptyKey
	}
	if len(key) > MaxKeySize {
		return ErrKeyTooLarge
	}

	keyCopy := make([]byte, len(key))
	copy(keyCopy, key)

	b.ops = append(b.ops, batchOp{
		op:  OpDelete,
		key: keyCopy,
		val: nil,
	})
	b.estimatedLength += HeaderSize + len(key)
	return nil
}

// Reset clears all operations from the batch.
func (b *Batch) Reset() {
	b.ops = b.ops[:0]
	b.estimatedLength = HeaderSize * 2
}

// itemOffset tracks the in-batch file offset and operation details for index updates.
type itemOffset struct {
	op     byte
	key    string
	offset int64
}

// Write atomically commits all buffered batch operations to the database log file.
// An empty batch is treated as a no-op.
// It encodes BatchBegin + all records + BatchCommit into ONE contiguous buffer,
// performs a single file.Write, syncs per SyncPolicy (SyncAlways and SyncOnBatch both sync once),
// and finally applies mutations to the index in order.
// Each Put's offset in the index is set to batchStart + running position of THAT record.
func (db *DB) Write(b *Batch) error {
	if b == nil || len(b.ops) == 0 {
		return nil
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

	// Reuse db.encBuf
	db.encBuf = db.encBuf[:0]

	// 1. BatchBegin marker (KeyLen=0, ValLen=0)
	db.encBuf = EncodeRecord(db.encBuf, OpBatchBegin, nil, nil)

	// Keep track of index mutations to apply post-write
	mutations := make([]itemOffset, 0, len(b.ops))

	for _, item := range b.ops {
		recordStartOffset := writeOffsetBefore + int64(len(db.encBuf))

		if item.op == OpPut {
			mutations = append(mutations, itemOffset{
				op:     OpPut,
				key:    string(item.key),
				offset: recordStartOffset,
			})
			db.encBuf = EncodeRecord(db.encBuf, OpPut, item.key, item.val)
		} else if item.op == OpDelete {
			mutations = append(mutations, itemOffset{
				op:  OpDelete,
				key: string(item.key),
			})
			db.encBuf = EncodeRecord(db.encBuf, OpDelete, item.key, nil)
		}

		if len(db.encBuf) > MaxBatchBufferSize {
			db.encBuf = db.encBuf[:0]
			return ErrBatchTooLarge
		}
	}

	// 2. BatchCommit marker (KeyLen=0, ValLen=0)
	db.encBuf = EncodeRecord(db.encBuf, OpBatchCommit, nil, nil)

	if len(db.encBuf) > MaxBatchBufferSize {
		db.encBuf = db.encBuf[:0]
		return ErrBatchTooLarge
	}

	// Single atomic write to disk
	n, err := db.file.Write(db.encBuf)
	if err != nil || n != len(db.encBuf) {
		db.handleWriteFailure(writeOffsetBefore)
		if err != nil {
			return fmt.Errorf("batch write error: %w", err)
		}
		return fmt.Errorf("batch short write: wrote %d of %d bytes: %w", n, len(db.encBuf), ErrCorrupt)
	}

	// Sync per policy: SyncAlways and SyncOnBatch each sync once; SyncNever does not sync
	if db.syncPolicy == SyncAlways || db.syncPolicy == SyncOnBatch {
		if err := db.file.Sync(); err != nil {
			db.handleWriteFailure(writeOffsetBefore)
			return fmt.Errorf("batch sync error: %w", err)
		}
	}

	// Apply index mutations in order after successful write and sync
	for _, mut := range mutations {
		if mut.op == OpPut {
			db.index.Set(mut.key, mut.offset)
		} else if mut.op == OpDelete {
			db.index.Delete(mut.key)
		}
	}

	db.writeOffset += int64(n)
	return nil
}
