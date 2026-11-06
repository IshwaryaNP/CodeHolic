package src

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestPhase3_E1_DiskFull_ExactBoundary verifies that Put enforces maxBytes exactly
// without writing partial data when the cap is reached.
func TestPhase3_E1_DiskFull_ExactBoundary(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "e1.db")

	// 1 record of key "k1" (2 bytes), val "v1" (2 bytes)
	// HeaderSize (15) + 2 + 2 = 19 bytes.
	// Allow exactly two records: 19 * 2 = 38 bytes.
	opts := &Options{
		SyncPolicy: SyncAlways,
		MaxBytes:   38,
	}

	db, err := Open(dbPath, opts)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	if err := db.Put([]byte("k1"), []byte("v1")); err != nil {
		t.Fatalf("Put k1 failed: %v", err)
	}
	if err := db.Put([]byte("k2"), []byte("v2")); err != nil {
		t.Fatalf("Put k2 failed: %v", err)
	}

	// db.writeOffset should be exactly 38
	if db.writeOffset != 38 {
		t.Fatalf("Expected writeOffset 38, got %d", db.writeOffset)
	}

	// Third Put must fail with ErrDiskFull
	err = db.Put([]byte("k3"), []byte("v3"))
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("Expected ErrDiskFull, got: %v", err)
	}

	// Assert file size on disk is still exactly 38 bytes (no bytes written)
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("os.Stat failed: %v", err)
	}
	if info.Size() != 38 {
		t.Fatalf("Expected file size 38, got %d (file was mutated during failed Put)", info.Size())
	}
}

// TestPhase3_E2_DiskFull_BatchPreCheck verifies that a batch exceeding maxBytes
// is rejected before writing any bytes to disk.
func TestPhase3_E2_DiskFull_BatchPreCheck(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "e2.db")

	// Limit to 100 bytes
	opts := &Options{
		SyncPolicy: SyncAlways,
		MaxBytes:   100,
	}

	db, err := Open(dbPath, opts)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// Initial put: 15 + 2 + 2 = 19 bytes
	if err := db.Put([]byte("k1"), []byte("v1")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	initialOffset := db.writeOffset

	// Batch with 5 items: Begin (15) + Commit (15) + 5*(15+2+10) = 30 + 135 = 165 bytes.
	// 19 + 165 = 184 > 100 -> rejected
	b := NewBatch()
	for i := 0; i < 5; i++ {
		_ = b.Put([]byte(fmt.Sprintf("b%d", i)), []byte("0123456789"))
	}

	err = db.Write(b)
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("Expected ErrDiskFull for batch, got: %v", err)
	}

	// Assert file size and writeOffset did not move at all
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("os.Stat failed: %v", err)
	}
	if info.Size() != initialOffset || db.writeOffset != initialOffset {
		t.Fatalf("Batch mutated disk/offset on failure: stat=%d, writeOffset=%d", info.Size(), db.writeOffset)
	}
}

// TestPhase3_E3_DiskFull_DBReadableAfterError verifies that Get continues to succeed
// cleanly after ErrDiskFull is returned.
func TestPhase3_E3_DiskFull_DBReadableAfterError(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "e3.db")

	opts := &Options{
		SyncPolicy: SyncAlways,
		MaxBytes:   50,
	}

	db, err := Open(dbPath, opts)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	_ = db.Put([]byte("alpha"), []byte("val_a"))
	_ = db.Put([]byte("beta"), []byte("val_b"))

	// Trigger ErrDiskFull
	err = db.Put([]byte("overflow"), []byte("toolargecontentforremainingroom"))
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("Expected ErrDiskFull, got: %v", err)
	}

	// Verify existing keys are readable
	valA, err := db.Get([]byte("alpha"))
	if err != nil || string(valA) != "val_a" {
		t.Fatalf("alpha read failed: %v, val: %s", err, string(valA))
	}
	valB, err := db.Get([]byte("beta"))
	if err != nil || string(valB) != "val_b" {
		t.Fatalf("beta read failed: %v, val: %s", err, string(valB))
	}
}

// TestPhase3_E4_DiskFull_CrashAndReopen verifies that open() succeeds after an
// engine encountered ErrDiskFull and terminated without unlinking or corrupting data.
func TestPhase3_E4_DiskFull_CrashAndReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "e4.db")

	opts := &Options{
		SyncPolicy: SyncAlways,
		MaxBytes:   60,
	}

	db, err := Open(dbPath, opts)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_ = db.Put([]byte("k1"), []byte("v1"))
	_ = db.Put([]byte("k2"), []byte("v2"))
	_ = db.Put([]byte("overflow"), []byte("bigval")) // Fails with ErrDiskFull

	// Simulate sudden crash without graceful Close()
	_ = db.file.Close()

	// Reopen
	db2, err := Open(dbPath, opts)
	if err != nil {
		t.Fatalf("Reopen failed after disk full: %v", err)
	}
	defer db2.Close()

	val1, err := db2.Get([]byte("k1"))
	if err != nil || string(val1) != "v1" {
		t.Fatalf("k1 read failed: %v, val: %s", err, string(val1))
	}
	val2, err := db2.Get([]byte("k2"))
	if err != nil || string(val2) != "v2" {
		t.Fatalf("k2 read failed: %v, val: %s", err, string(val2))
	}
}

// TestPhase3_E5_DiskFull_SpaceReclamationAndAutoCompaction verifies that overwrites
// and deletes create stale bytes that allow subsequent writes either via auto-compact
// or manual compact.
func TestPhase3_E5_DiskFull_SpaceReclamationAndAutoCompaction(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "e5.db")

	// Record size: 15 + 2 + 3 = 20 bytes
	// Allow 5 records = 100 bytes
	opts := &Options{
		SyncPolicy: SyncAlways,
		MaxBytes:   100,
	}

	db, err := Open(dbPath, opts)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// Insert 4 keys = 80 bytes
	_ = db.Put([]byte("k1"), []byte("v01"))
	_ = db.Put([]byte("k2"), []byte("v02"))
	_ = db.Put([]byte("k3"), []byte("v03"))
	_ = db.Put([]byte("k4"), []byte("v04"))

	// Delete key k1: record size = 15 + 2 = 17 bytes -> writeOffset becomes 80 + 17 = 97 bytes.
	if err := db.Delete([]byte("k1")); err != nil {
		t.Fatalf("Delete k1 failed: %v", err)
	}

	// Current file has 97 bytes, max is 100.
	// Trying to put "k5" ("v05", 20 bytes) would exceed 100 without auto-compaction.
	// Reclaimable space = 97 - liveBytes (3 live keys * 20 = 60) = 37 bytes.
	// Since 37 >= 20, auto-compaction should trigger and allow Put to succeed!
	err = db.Put([]byte("k5"), []byte("v05"))
	if err != nil {
		t.Fatalf("Put k5 failed (auto-compaction should have reclaimed space): %v", err)
	}

	// Verify all live keys are present
	for _, key := range []string{"k2", "k3", "k4", "k5"} {
		if _, err := db.Get([]byte(key)); err != nil {
			t.Fatalf("Key %s missing after auto-compaction: %v", key, err)
		}
	}
	// And k1 is still deleted
	if _, err := db.Get([]byte("k1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("k1 should be deleted, got: %v", err)
	}
}
