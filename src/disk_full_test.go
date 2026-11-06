package src

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestDiskFull_PutBatchDelete(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "diskfull.db")

	// Set cap to 1000 bytes. Put should fail when offset + record + headroom > cap.
	// Headroom is 64KB, so cap must be larger than 64KB + record to allow any Put.
	// Let's set maxBytes to 65536 + 200 = 65736 bytes.
	maxBytes := int64(65536 + 200)
	opts := &Options{
		SyncPolicy: SyncAlways,
		MaxBytes:   maxBytes,
	}

	db, err := Open(dbPath, opts)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// Put small record: header (15) + key (4) + val (10) = 29 bytes.
	// 29 + 65536 = 65565 <= 65736 -> should succeed
	err = db.Put([]byte("k001"), []byte("val1234567"))
	if err != nil {
		t.Fatalf("First Put failed: %v", err)
	}

	// Now try putting a huge value that exceeds maxBytes:
	hugeVal := make([]byte, 500)
	err = db.Put([]byte("k002"), hugeVal)
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("Expected ErrDiskFull for large put, got: %v", err)
	}

	// Verify database is still completely readable
	val, err := db.Get([]byte("k001"))
	if err != nil || string(val) != "val1234567" {
		t.Fatalf("Database unreadable after ErrDiskFull: %v, val: %s", err, string(val))
	}

	// Batch that exceeds cap should fail with ErrDiskFull upfront without writing anything
	b := NewBatch()
	for i := 0; i < 10; i++ {
		_ = b.Put([]byte("batchKey"), []byte("valueBytes1234567890"))
	}
	err = db.Write(b)
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("Expected ErrDiskFull for batch, got: %v", err)
	}

	// Verify delete still succeeds because headroom is preserved for deletes
	err = db.Delete([]byte("k001"))
	if err != nil {
		t.Fatalf("Delete failed at disk full boundary: %v", err)
	}

	// Verify key deleted
	_, err = db.Get([]byte("k001"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Expected ErrNotFound after delete, got: %v", err)
	}

	// Run compaction: shrinks file and recovers space (E5)
	err = db.Compact()
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}
}
