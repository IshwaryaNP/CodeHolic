package src

import (
	"bufio"
	"io"
	"os"
)

// batchMutation tracks a pending index mutation inside an uncommitted batch.
type batchMutation struct {
	op     byte
	key    string
	offset int64
}

// Recover scans the append-only log file at path sequentially and rebuilds the active Index.
//
// Recovery rules:
//  1. Any stale "<path>.tmp" file remaining from a crashed compaction is removed.
//  2. If the file does not exist or is empty, an empty index and validEnd=0 are returned.
//  3. Batches: on BatchBegin, mutations are buffered in memory. On BatchCommit, they are applied.
//     If EOF or torn tail / corruption occurs before BatchCommit, the entire uncommitted batch
//     is rolled back and the file is truncated back to the BatchBegin offset.
//     Nested BatchBegin or orphan BatchCommit records are treated as corruptions.
//  4. Any torn tail or CRC mismatch truncates the file back to the last valid boundary and syncs.
//
// Known tradeoff: A CRC failure in the middle of the file is treated as end-of-log;
// all subsequent records, even if otherwise valid, are discarded and truncated away.
func Recover(path string) (*Index, int64, error) {
	// 1. Remove stale temporary compaction files if any
	tmpPath := path + ".tmp"
	if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
		// Ignore if does not exist, otherwise report
		return nil, 0, err
	}

	// 2. Open file for reading and potential truncation
	f, err := os.OpenFile(path, os.O_RDWR, 0666)
	if err != nil {
		if os.IsNotExist(err) {
			return NewIndex(0), 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	fileSize := info.Size()
	if fileSize == 0 {
		return NewIndex(0), 0, nil
	}

	// Pre-size index map estimate (capped to avoid huge memory spikes)
	// Assuming an average record size of ~64 bytes
	sizeHint := int(fileSize / 64)
	if sizeHint < 16 {
		sizeHint = 16
	} else if sizeHint > 2_000_000 {
		sizeHint = 2_000_000
	}
	idx := NewIndex(sizeHint)

	// Sequential buffered reader (1MB buffer) for high throughput
	const readerBufSize = 1024 * 1024
	reader := bufio.NewReaderSize(f, readerBufSize)

	var validEnd int64 = 0
	var offset int64 = 0

	// Batch tracking
	inBatch := false
	var batchBeginOffset int64 = 0
	var batchMutations []batchMutation

	var hdrBuf [HeaderSize]byte
	scratch := make([]byte, 0, 64*1024) // growable key+val buffer

	for offset < fileSize {
		recordStart := offset

		// Check if at least 15 bytes remain in file
		if offset+int64(HeaderSize) > fileSize {
			// Torn tail in header
			break
		}

		// Read fixed 15-byte header
		if _, err := io.ReadFull(reader, hdrBuf[:]); err != nil {
			break
		}

		hdr, err := DecodeHeader(hdrBuf[:])
		if err != nil {
			// Malformed header / unknown op / bad TotalLen
			break
		}

		// Verify TotalLen does not exceed remaining file bytes
		if offset+int64(hdr.TotalLen) > fileSize {
			// Incomplete record payload
			break
		}

		keyValLen := int(hdr.TotalLen) - HeaderSize
		if cap(scratch) < keyValLen {
			scratch = make([]byte, keyValLen)
		} else {
			scratch = scratch[:keyValLen]
		}

		if _, err := io.ReadFull(reader, scratch); err != nil {
			// Torn tail reading key+val
			break
		}

		keyBytes := scratch[:hdr.KeyLen]
		valBytes := scratch[hdr.KeyLen:]

		if !VerifyCRC(hdrBuf[:], keyBytes, valBytes) {
			// CRC mismatch: corrupted record
			break
		}

		// Process valid record
		recordEnd := offset + int64(hdr.TotalLen)

		if hdr.OpType == OpBatchBegin {
			if inBatch {
				// Nested BatchBegin is corruption
				break
			}
			inBatch = true
			batchBeginOffset = recordStart
			batchMutations = batchMutations[:0]
			offset = recordEnd
			continue
		} else if hdr.OpType == OpBatchCommit {
			if !inBatch {
				// Orphan BatchCommit without BatchBegin is corruption
				break
			}
			// Commit the batch: apply all buffered mutations
			for _, m := range batchMutations {
				if m.op == OpPut {
					idx.Set(m.key, m.offset)
				} else if m.op == OpDelete {
					idx.Delete(m.key)
				}
			}
			inBatch = false
			batchMutations = batchMutations[:0]
			validEnd = recordEnd
			offset = recordEnd
			continue
		}

		// OpPut or OpDelete
		if inBatch {
			// Buffer mutation until BatchCommit
			batchMutations = append(batchMutations, batchMutation{
				op:     hdr.OpType,
				key:    string(keyBytes),
				offset: recordStart,
			})
		} else {
			if hdr.OpType == OpPut {
				idx.Set(string(keyBytes), recordStart)
			} else if hdr.OpType == OpDelete {
				idx.Delete(string(keyBytes))
			}
			validEnd = recordEnd
		}

		offset = recordEnd
	}

	// If scanning stopped while a batch was still open, truncate back to batchBeginOffset
	if inBatch {
		validEnd = batchBeginOffset
	}

	// If validEnd is less than the physical fileSize, truncate and sync
	if validEnd < fileSize {
		if err := f.Truncate(validEnd); err != nil {
			return nil, validEnd, err
		}
		if err := f.Sync(); err != nil {
			return nil, validEnd, err
		}
	}

	return idx, validEnd, nil
}
