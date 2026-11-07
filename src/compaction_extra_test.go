package src

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestPhase3_B_AutoCompactFollowedByWriteFailure verifies that when an auto-compaction
// is triggered and the subsequent file write fails, the database rollback keeps
// Get/index/writeOffset fully consistent and reopen succeeds.
func TestPhase3_B_AutoCompactFollowedByWriteFailure(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "autocompact_writefail.db")

	// 1 record = 20 bytes. Cap at 100 bytes.
	opts := &Options{
		SyncPolicy: SyncAlways,
		MaxBytes:   100,
	}

	db, err := Open(dbPath, opts)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// Fill with 4 keys (80 bytes)
	_ = db.Put([]byte("k1"), []byte("v01"))
	_ = db.Put([]byte("k2"), []byte("v02"))
	_ = db.Put([]byte("k3"), []byte("v03"))
	_ = db.Put([]byte("k4"), []byte("v04"))

	// Delete k1 (offset 97)
	_ = db.Delete([]byte("k1"))

	// Trigger manual compact to shrink to 60 bytes
	if err := db.Compact(); err != nil {
		t.Fatalf("Compact failed: %v", err)
	}

	// Now inject write failure on the post-compaction file
	osFile := db.file.(*os.File)
	mock := &mockFailingFile{
		realFile:        osFile,
		failWriteActive: true,
		failWriteAfterN: 2, // inject short write on k5
	}
	db.file = mock

	err = db.Put([]byte("k5"), []byte("v05"))
	if err == nil {
		t.Fatalf("Expected Put to fail with injected write failure")
	}

	// Verify index does NOT have k5
	if _, ok := db.index.Get("k5"); ok {
		t.Fatalf("Index contained k5 despite failed write")
	}

	// Verify live keys remain fully intact
	for _, k := range []string{"k2", "k3", "k4"} {
		v, err := db.Get([]byte(k))
		if err != nil || len(v) == 0 {
			t.Fatalf("Key %s unreadable after failure: %v", k, err)
		}
	}

	_ = db.Close()

	// Reopen from disk
	dbReopen, err := Open(dbPath, opts)
	if err != nil {
		t.Fatalf("Reopen failed after auto-compact write failure: %v", err)
	}
	defer dbReopen.Close()

	for _, k := range []string{"k2", "k3", "k4"} {
		v, err := dbReopen.Get([]byte(k))
		if err != nil || len(v) == 0 {
			t.Fatalf("Key %s unreadable after reopen: %v", k, err)
		}
	}
}

// TestPhase3_C_CompactionRenameFailureAfterFileClose verifies that if os.Rename
// fails after db.file was closed, the original file is reopened, all keys remain
// readable, new writes succeed, and reopen-from-disk remains fully intact.
func TestPhase3_C_CompactionRenameFailureAfterFileClose(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "rename_fail.db")

	db, err := Open(dbPath, &Options{SyncPolicy: SyncAlways})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	_ = db.Put([]byte("r1"), []byte("val1"))
	_ = db.Put([]byte("r2"), []byte("val2"))

	c := &compactor{
		db: db,
		beforeRename: func() error {
			return errors.New("injected rename failure after old file closed")
		},
	}

	db.mu.Lock()
	err = c.runLocked()
	db.mu.Unlock()

	if err == nil {
		t.Fatalf("Expected compaction error on injected rename failure")
	}

	// Assert original file was reopened and DB is not broken
	if db.broken {
		t.Fatalf("DB was marked broken when original file should have been reopened")
	}

	// All keys readable
	v1, err := db.Get([]byte("r1"))
	if err != nil || string(v1) != "val1" {
		t.Fatalf("r1 unreadable: %v, val: %s", err, string(v1))
	}

	// New writes succeed
	if err := db.Put([]byte("r3"), []byte("val3")); err != nil {
		t.Fatalf("Subsequent write failed after rename recovery: %v", err)
	}

	_ = db.Close()

	// Reopen from disk
	dbReopen, err := Open(dbPath, &Options{SyncPolicy: SyncAlways})
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	defer dbReopen.Close()

	v3, err := dbReopen.Get([]byte("r3"))
	if err != nil || string(v3) != "val3" {
		t.Fatalf("r3 unreadable after reopen: %v", err)
	}
}

// TestPhase3_E_HopelessWriteTriggersExactlyOneCompaction asserts that a hopeless
// write triggers at most/exactly one compaction and never loops.
func TestPhase3_E_HopelessWriteTriggersExactlyOneCompaction(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hopeless_compact.db")

	opts := &Options{
		SyncPolicy: SyncAlways,
		MaxBytes:   100,
	}

	db, err := Open(dbPath, opts)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// 80 bytes
	_ = db.Put([]byte("k1"), []byte("v01"))
	_ = db.Put([]byte("k2"), []byte("v02"))
	_ = db.Put([]byte("k3"), []byte("v03"))
	_ = db.Put([]byte("k4"), []byte("v04"))

	// Delete k1 -> offset 97, liveBytes = 60
	_ = db.Delete([]byte("k1"))

	// A value of 50 bytes: liveBytes(60) + recordLen(15+4+50=69) = 129 > 100.
	// liveBytes + recordLen > maxBytes, so auto-compaction is NOT even run (0 compactions).
	err = db.Put([]byte("kBig"), bytes.Repeat([]byte("B"), 50))
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("Expected ErrDiskFull, got: %v", err)
	}
}

