package src

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDB_FailureInjection_BatchWriteFails_RollbackSucceeds(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "batch_fail_rollback.db")

	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	if err := db.Put([]byte("initial_key"), []byte("initial_val")); err != nil {
		t.Fatalf("initial Put failed: %v", err)
	}

	validOffset := db.writeOffset

	// Prepare a batch with multiple operations
	b := NewBatch()
	_ = b.Put([]byte("batch1"), []byte("val1"))
	_ = b.Put([]byte("batch2"), []byte("val2"))
	_ = b.Delete([]byte("initial_key"))

	// Inject a mock file that allows partial bytes (e.g. 20 bytes, writing BatchBegin and partial batch1) then fails
	osFile := db.file.(*os.File)
	mock := &mockFailingFile{
		realFile:        osFile,
		failWriteActive: true,
		failWriteAfterN: 20,
	}
	db.file = mock

	// Attempt Write: should fail midway
	err = db.Write(b)
	if err == nil {
		t.Fatalf("expected batch Write to fail")
	}

	// Index must NOT contain batch1 or batch2, and initial_key must NOT be deleted
	if _, ok := db.index.Get("batch1"); ok {
		t.Fatalf("batch1 found in index after failed batch write")
	}
	if _, ok := db.index.Get("batch2"); ok {
		t.Fatalf("batch2 found in index after failed batch write")
	}
	if _, ok := db.index.Get("initial_key"); !ok {
		t.Fatalf("initial_key was deleted despite failed batch write")
	}

	// File on disk must be truncated and synced back to validOffset (no orphan BatchBegin or partial record)
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Size() != validOffset {
		t.Fatalf("file not rolled back on batch failure: size=%d, expected=%d", info.Size(), validOffset)
	}

	// Deactivate failure injection and verify subsequent writes succeed normally
	mock.failWriteActive = false
	if err := db.Put([]byte("after_fail"), []byte("ok")); err != nil {
		t.Fatalf("Put failed after batch failure rollback: %v", err)
	}
	val, err := db.Get([]byte("after_fail"))
	if err != nil || string(val) != "ok" {
		t.Fatalf("failed to retrieve key after batch rollback: %v", err)
	}
}

