package src

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
)

// ErrCompactionFatal is returned if compaction fails catastrophically during file swap
// such that the engine can no longer safely proceed.
var ErrCompactionFatal = errors.New("fatal compaction error: database is broken")

// compactor handles compacting the append-only log to reclaim space from deleted or overwritten records.
// Tests can construct a compactor directly with hook functions to test failure injection without global state.
type compactor struct {
	db           *DB
	beforeWrite  func() error
	beforeSync   func() error
	beforeClose  func() error
	beforeRename func() error
	afterRename  func() error
	beforeReopen func() error
	afterCompaction func()
}

type liveEntry struct {
	key       string
	oldOffset int64
}

// Compact reclaims disk space by writing all active live records to a temporary file,
// fsyncing, closing, and atomically replacing the active log file.
// Note: Compaction temp files (*.tmp) do NOT count against --max-bytes;
// --max-bytes bounds only the primary active data log.
func (db *DB) Compact() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	c := &compactor{db: db}
	return c.runLocked()
}

// liveBytesLocked calculates the sum of all live record sizes (header + key + val)
// currently indexed in the database. Caller MUST hold db.mu (RLock or Lock).
func (db *DB) liveBytesLocked() int64 {
	var total int64
	var hdrBuf [HeaderSize]byte
	db.index.Range(func(key string, off int64) bool {
		if _, err := db.file.ReadAt(hdrBuf[:], off); err == nil {
			if hdr, err := DecodeHeader(hdrBuf[:]); err == nil && hdr.OpType == OpPut {
				total += int64(hdr.TotalLen)
			}
		}
		return true
	})
	return total
}

func (c *compactor) run() error {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	return c.runLocked()
}

// runLocked executes compaction. Precondition: caller MUST hold c.db.mu.Lock.
func (c *compactor) runLocked() error {
	if c.db.closed {
		return ErrClosed
	}
	if c.db.broken {
		return ErrBroken
	}

	tmpPath := c.db.path + ".tmp"

	// Create tmp file O_CREATE|O_TRUNC|O_WRONLY
	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0666)
	if err != nil {
		return fmt.Errorf("failed to create compaction tmp file: %w", err)
	}

	// Ensure cleanup on failure before successful rename
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			if tmpFile != nil {
				_ = tmpFile.Close()
			}
			_ = os.Remove(tmpPath)
		}
	}()

	// Snapshot live keys and offsets
	live := make([]liveEntry, 0, c.db.index.Len())
	c.db.index.Range(func(key string, off int64) bool {
		live = append(live, liveEntry{key: key, oldOffset: off})
		return true
	})

	// Sort by oldOffset to preserve sequential read locality
	sort.Slice(live, func(i, j int) bool {
		return live[i].oldOffset < live[j].oldOffset
	})

	bufWriter := bufio.NewWriterSize(tmpFile, 1024*1024)
	newMap := make(map[string]int64, len(live))
	var newOffset int64 = 0

	var hdrBuf [HeaderSize]byte
	scratch := make([]byte, 0, 64*1024)

	for _, entry := range live {
		if c.beforeWrite != nil {
			if err := c.beforeWrite(); err != nil {
				return fmt.Errorf("compaction beforeWrite hook failed: %w", err)
			}
		}

		// Read header from old file
		if _, err := c.db.file.ReadAt(hdrBuf[:], entry.oldOffset); err != nil {
			return fmt.Errorf("%w: failed reading header at offset %d: %v", ErrCorrupt, entry.oldOffset, err)
		}

		hdr, err := DecodeHeader(hdrBuf[:])
		if err != nil {
			return fmt.Errorf("%w: failed decoding header at offset %d: %v", ErrCorrupt, entry.oldOffset, err)
		}

		if entry.oldOffset+int64(hdr.TotalLen) > c.db.writeOffset {
			return fmt.Errorf("%w: record at offset %d extends past writeOffset %d", ErrCorrupt, entry.oldOffset, c.db.writeOffset)
		}

		if hdr.OpType != OpPut {
			return fmt.Errorf("%w: expected OpPut at indexed offset %d, got op %d", ErrCorrupt, entry.oldOffset, hdr.OpType)
		}

		keyValLen := int(hdr.TotalLen) - HeaderSize
		if cap(scratch) < keyValLen {
			scratch = make([]byte, keyValLen)
		} else {
			scratch = scratch[:keyValLen]
		}

		if _, err := c.db.file.ReadAt(scratch, entry.oldOffset+int64(HeaderSize)); err != nil {
			return fmt.Errorf("%w: failed reading payload at offset %d: %v", ErrCorrupt, entry.oldOffset, err)
		}

		keyBytes := scratch[:hdr.KeyLen]
		valBytes := scratch[hdr.KeyLen:]

		if !VerifyCRC(hdrBuf[:], keyBytes, valBytes) {
			return fmt.Errorf("%w: CRC verification failed at offset %d", ErrCorrupt, entry.oldOffset)
		}

		if !bytes.Equal(keyBytes, []byte(entry.key)) {
			return fmt.Errorf("%w: key at offset %d mismatch (expected %s)", ErrCorrupt, entry.oldOffset, entry.key)
		}

		// Write raw record bytes unchanged into tmp
		if _, err := bufWriter.Write(hdrBuf[:]); err != nil {
			return fmt.Errorf("failed writing header to tmp: %w", err)
		}
		if _, err := bufWriter.Write(scratch); err != nil {
			return fmt.Errorf("failed writing payload to tmp: %w", err)
		}

		newMap[entry.key] = newOffset
		newOffset += int64(hdr.TotalLen)
	}

	if err := bufWriter.Flush(); err != nil {
		return fmt.Errorf("failed flushing tmp buffer: %w", err)
	}

	if c.beforeSync != nil {
		if err := c.beforeSync(); err != nil {
			return fmt.Errorf("compaction beforeSync hook failed: %w", err)
		}
	}

	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("failed syncing tmp file: %w", err)
	}

	// Close tmp file before renaming
	if c.beforeClose != nil {
		if err := c.beforeClose(); err != nil {
			return fmt.Errorf("compaction beforeClose hook failed: %w", err)
		}
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed closing tmp file: %w", err)
	}
	tmpFile = nil

	// Close old db.file before rename (strictly required on Windows to release file lock)
	_ = c.db.file.Close()

	if c.beforeRename != nil {
		if err := c.beforeRename(); err != nil {
			// Hook error: reopen original file and return error
			newOrigFile, openErr := os.OpenFile(c.db.path, os.O_RDWR, 0666)
			if openErr != nil {
				c.db.broken = true
				return fmt.Errorf("%w: rename hook failed and failed reopening original file: %v", ErrCompactionFatal, openErr)
			}
			c.db.file = newOrigFile
			return fmt.Errorf("compaction beforeRename hook failed: %w", err)
		}
	}

	// Atomic rename
	if err := os.Rename(tmpPath, c.db.path); err != nil {
		// Rename failed: attempt reopening original file to keep DB functional
		newOrigFile, openErr := os.OpenFile(c.db.path, os.O_RDWR, 0666)
		if openErr != nil {
			c.db.broken = true
			return fmt.Errorf("%w: rename failed (%v) and failed reopening original file (%v)", ErrCompactionFatal, err, openErr)
		}
		c.db.file = newOrigFile
		return fmt.Errorf("rename failed: %w", err)
	}

	cleanupTmp = false

	if c.afterRename != nil {
		if err := c.afterRename(); err != nil {
			// Rename succeeded so the new file is authoritative on disk!
			// Update in-memory state to reflect the compacted file before returning error
			c.db.index.Replace(newMap)
			c.db.writeOffset = newOffset
			// Try to reopen authoritative file
			newF, openErr := os.OpenFile(c.db.path, os.O_RDWR, 0666)
			if openErr != nil {
				c.db.broken = true
				return fmt.Errorf("%w: afterRename hook failed and reopening new file failed: %v", ErrCompactionFatal, openErr)
			}
			c.db.file = newF
			return fmt.Errorf("compaction afterRename hook failed: %w", err)
		}
	}

	// On Unix-like systems, fsync directory to guarantee durable directory entry metadata.
	// On Windows, directories cannot be opened with standard os.OpenFile and will fail with Access Denied,
	// so directory sync is skipped on Windows.
	if runtime.GOOS != "windows" {
		syncDir(c.db.path)
	}

	if c.beforeReopen != nil {
		if err := c.beforeReopen(); err != nil {
			// Rename succeeded: new file is authoritative
			c.db.index.Replace(newMap)
			c.db.writeOffset = newOffset
			c.db.broken = true
			return fmt.Errorf("%w: beforeReopen hook injected fatal reopen error: %v", ErrCompactionFatal, err)
		}
	}

	// Reopen the newly compacted db.path
	newF, err := os.OpenFile(c.db.path, os.O_RDWR, 0666)
	if err != nil {
		// Rename succeeded: swap in-memory index/offset, but mark DB broken since file handle failed
		c.db.index.Replace(newMap)
		c.db.writeOffset = newOffset
		c.db.broken = true
		return fmt.Errorf("%w: failed reopening compacted file: %v", ErrCompactionFatal, err)
	}

	// Swap into DB state atomically under db.mu
	c.db.file = newF
	c.db.index.Replace(newMap)
	c.db.writeOffset = newOffset

	if c.afterCompaction != nil {
		c.afterCompaction()
	}

	return nil
}

func syncDir(filePath string) {
	dir := os.DirFS(filePath)
	_ = dir
	// Parent dir fsync helper for POSIX
	dirPath := "."
	for i := len(filePath) - 1; i >= 0; i-- {
		if filePath[i] == '/' || filePath[i] == '\\' {
			dirPath = filePath[:i]
			break
		}
	}
	if d, err := os.Open(dirPath); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
