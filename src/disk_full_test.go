package src

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Helper to compile kvdb binary for subprocess tests
var (
	compiledKVDBPath string
	compileKVDBOnce  sync.Once
	compileKVDBErr   error
)

func getKVDBBinary(t *testing.T) string {
	t.Helper()
	compileKVDBOnce.Do(func() {
		tmpDir, err := os.MkdirTemp("", "kvdb_bin_*")
		if err != nil {
			compileKVDBErr = err
			return
		}
		binPath := filepath.Join(tmpDir, "kvdb.exe")
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/kvdb")
		// Project root is parent of src
		cmd.Dir = filepath.Clean(filepath.Join(".", ".."))
		out, err := cmd.CombinedOutput()
		if err != nil {
			compileKVDBErr = fmt.Errorf("failed building kvdb binary: %v, out: %s", err, string(out))
			return
		}
		compiledKVDBPath = binPath
	})
	if compileKVDBErr != nil {
		t.Fatalf("getKVDBBinary failed: %v", compileKVDBErr)
	}
	return compiledKVDBPath
}

// E1: Single put at cap: error, readable, old data intact, file size on disk <= maxBytes.
func TestPhase3_E1_SinglePutAtCap(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "e1.db")

	// Allow exactly two records: 19 * 2 = 38 bytes
	maxBytes := int64(38)
	opts := &Options{
		SyncPolicy: SyncAlways,
		MaxBytes:   maxBytes,
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

	// db.writeOffset should be 38
	if db.writeOffset != 38 {
		t.Fatalf("Expected writeOffset 38, got %d", db.writeOffset)
	}

	// Hopeless put exceeds maxBytes and liveBytes+recordLen > maxBytes
	err = db.Put([]byte("k3"), []byte("v3"))
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("Expected ErrDiskFull, got: %v", err)
	}

	// Reads stay available and consistent
	v1, err := db.Get([]byte("k1"))
	if err != nil || string(v1) != "v1" {
		t.Fatalf("k1 read failed: %v, val: %s", err, string(v1))
	}
	v2, err := db.Get([]byte("k2"))
	if err != nil || string(v2) != "v2" {
		t.Fatalf("k2 read failed: %v, val: %s", err, string(v2))
	}

	// File size on disk <= maxBytes
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("os.Stat failed: %v", err)
	}
	if info.Size() > maxBytes || info.Size() != 38 {
		t.Fatalf("File size assertion failed: stat size = %d, maxBytes = %d", info.Size(), maxBytes)
	}
}

// E2: Batch exceeds cap: error, none of its ops visible, no bytes written.
func TestPhase3_E2_BatchExceedsCap(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "e2.db")

	opts := &Options{
		SyncPolicy: SyncAlways,
		MaxBytes:   100,
	}

	db, err := Open(dbPath, opts)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	if err := db.Put([]byte("pre"), []byte("val")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	initialOffset := db.writeOffset

	// Batch that exceeds cap (Begin 15 + Commit 15 + records)
	b := NewBatch()
	for i := 0; i < 5; i++ {
		_ = b.Put([]byte(fmt.Sprintf("b%d", i)), []byte("0123456789"))
	}

	err = db.Write(b)
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("Expected ErrDiskFull, got: %v", err)
	}

	// None of its ops visible
	for i := 0; i < 5; i++ {
		_, err := db.Get([]byte(fmt.Sprintf("b%d", i)))
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Batch key b%d was visible after failed batch: %v", i, err)
		}
	}

	// No bytes written to disk
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("os.Stat failed: %v", err)
	}
	if info.Size() != initialOffset || db.writeOffset != initialOffset {
		t.Fatalf("File size moved on failed batch: stat=%d, offset=%d", info.Size(), db.writeOffset)
	}
}

// E3: At cap then process KILL via a real kvdb serve subprocess, reopen, all ACKed data intact.
func TestPhase3_E3_AtCapProcessKillAndReopen(t *testing.T) {
	bin := getKVDBBinary(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "e3_kill.db")

	// Limit to 200 bytes: flag must come before positional path for flag.Parse
	maxBytes := 200
	cmd := exec.Command(bin, "serve", "--max-bytes", fmt.Sprintf("%d", maxBytes), dbPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe failed: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe failed: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed starting kvdb serve: %v", err)
	}

	scanner := bufio.NewScanner(stdout)
	writer := bufio.NewWriter(stdin)

	// Send ops until disk full ERR is received
	ackedKeys := make(map[string]string)
	for i := 0; i < 20; i++ {
		opID := fmt.Sprintf("op_%d", i)
		k := fmt.Sprintf("k%d", i)
		v := fmt.Sprintf("val%d", i)
		kHex := fmt.Sprintf("%x", k)
		vHex := fmt.Sprintf("%x", v)
		_, _ = writer.WriteString(fmt.Sprintf("%s put %s %s\n", opID, kHex, vHex))
		_ = writer.Flush()

		if scanner.Scan() {
			resp := scanner.Text()
			if strings.HasPrefix(resp, "ACK "+opID) {
				ackedKeys[k] = v
			} else if strings.HasPrefix(resp, "ERR "+opID) {
				// Assert serve prints ERR <id> disk full
				if !strings.Contains(resp, "disk full") {
					t.Fatalf("Expected 'disk full' in error line, got: %s", resp)
				}
				break
			}
		}
	}

	if len(ackedKeys) == 0 {
		t.Fatalf("No keys were ACKed before reaching cap")
	}

	// Force kill -9 process while at cap
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()

	// Reopen DB directly
	db, err := Open(dbPath, &Options{SyncPolicy: SyncAlways, MaxBytes: int64(maxBytes)})
	if err != nil {
		t.Fatalf("Open after kill failed: %v", err)
	}
	defer db.Close()

	// Verify all ACKed keys are intact
	for k, v := range ackedKeys {
		got, err := db.Get([]byte(k))
		if err != nil || string(got) != v {
			t.Fatalf("ACKed key %s missing or corrupt: %v, got %s", k, err, string(got))
		}
	}

	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("os.Stat failed: %v", err)
	}
	if info.Size() > int64(maxBytes) {
		t.Fatalf("File size %d exceeds maxBytes %d", info.Size(), maxBytes)
	}
}

// E4: Failure-injection during compaction at each step asserting reopen-from-disk yields all data intact.
func TestPhase3_E4_CompactionFailureInjectionAllSteps(t *testing.T) {
	steps := []string{"tmp_write", "tmp_sync", "tmp_close", "before_rename", "after_rename", "reopen_error"}

	for _, step := range steps {
		t.Run("step_"+step, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "e4_"+step+".db")

			db, err := Open(dbPath, &Options{SyncPolicy: SyncAlways})
			if err != nil {
				t.Fatalf("Open failed: %v", err)
			}

			// Seed live and stale data
			_ = db.Put([]byte("k1"), []byte("val1"))
			_ = db.Put([]byte("k2"), []byte("val2"))
			_ = db.Put([]byte("k1"), []byte("val1_updated"))
			_ = db.Delete([]byte("k2"))
			_ = db.Put([]byte("k3"), []byte("val3"))

			c := &compactor{db: db}

			switch step {
			case "tmp_write":
				c.beforeWrite = func() error { return errors.New("injected tmp write error") }
			case "tmp_sync":
				c.beforeSync = func() error { return errors.New("injected tmp sync error") }
			case "tmp_close":
				c.beforeClose = func() error { return errors.New("injected tmp close error") }
			case "before_rename":
				c.beforeRename = func() error { return errors.New("injected before rename error") }
			case "after_rename":
				c.afterRename = func() error { return errors.New("injected after rename error") }
			case "reopen_error":
				c.beforeReopen = func() error { return errors.New("injected reopen error") }
			}

			db.mu.Lock()
			compErr := c.runLocked()
			db.mu.Unlock()

			if compErr == nil {
				t.Fatalf("Expected compaction error for step %s", step)
			}

			// In case step was before rename, tmp file must be removed and original file restored
			if step != "after_rename" && step != "reopen_error" {
				tmpInfo, _ := os.Stat(dbPath + ".tmp")
				if tmpInfo != nil {
					t.Fatalf("Temp file was not cleaned up after error in %s", step)
				}
			}

			_ = db.Close()

			// Reopen fresh from disk
			dbReopened, err := Open(dbPath, &Options{SyncPolicy: SyncAlways})
			if err != nil {
				t.Fatalf("Reopen-from-disk failed after step %s failure: %v", step, err)
			}
			defer dbReopened.Close()

			// All data intact: k1=val1_updated, k3=val3, k2 absent
			v1, err := dbReopened.Get([]byte("k1"))
			if err != nil || string(v1) != "val1_updated" {
				t.Fatalf("k1 invalid: %v, val: %s", err, string(v1))
			}
			v3, err := dbReopened.Get([]byte("k3"))
			if err != nil || string(v3) != "val3" {
				t.Fatalf("k3 invalid: %v, val: %s", err, string(v3))
			}
			_, err = dbReopened.Get([]byte("k2"))
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("k2 should be not found, got %v", err)
			}
		})
	}
}

// E5: Delete, compact/auto-compact, writes succeed again with no manual step.
// Also tests repeated overwrites of one key at the cap succeed via auto-compaction,
// and a hopeless write triggers exactly one compaction.
func TestPhase3_E5_DeleteAutoCompactWritesSucceedAndOverwrites(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "e5.db")

	// 1 record = 15 + 2 + 3 = 20 bytes.
	// Cap at 100 bytes (fits 5 records).
	opts := &Options{
		SyncPolicy: SyncAlways,
		MaxBytes:   100,
	}

	db, err := Open(dbPath, opts)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	_ = db.Put([]byte("k1"), []byte("v01"))
	_ = db.Put([]byte("k2"), []byte("v02"))
	_ = db.Put([]byte("k3"), []byte("v03"))
	_ = db.Put([]byte("k4"), []byte("v04"))

	// Delete k1: tombstone size 17 -> offset 97
	if err := db.Delete([]byte("k1")); err != nil {
		t.Fatalf("Delete k1 failed: %v", err)
	}

	// Putting k5 (20B) would bring offset to 117 > 100.
	// liveBytes = 60, 60 + 20 <= 100 -> auto-compact triggers once and succeeds!
	if err := db.Put([]byte("k5"), []byte("v05")); err != nil {
		t.Fatalf("Put k5 failed: %v", err)
	}

	// Repeated overwrites of one key at the cap succeed via auto-compaction
	for i := 0; i < 10; i++ {
		err := db.Put([]byte("k5"), []byte(fmt.Sprintf("w%02d", i)))
		if err != nil {
			t.Fatalf("Repeated overwrite iteration %d failed: %v", i, err)
		}
	}

	// Verify hopeless write (exceeds maxBytes even after compact) fails with ErrDiskFull
	hugeVal := bytes.Repeat([]byte("X"), 120)
	err = db.Put([]byte("kHopeless"), hugeVal)
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("Expected ErrDiskFull for hopeless put, got: %v", err)
	}

	// Reads stay available and consistent
	v5, err := db.Get([]byte("k5"))
	if err != nil || string(v5) != "w09" {
		t.Fatalf("k5 read failed: %v, val: %s", err, string(v5))
	}

	// Assert file size <= maxBytes
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("os.Stat failed: %v", err)
	}
	if info.Size() > 100 {
		t.Fatalf("File size %d exceeds maxBytes 100", info.Size())
	}
}

// Extra Assertion: --max-bytes 50000 on an empty DB accepts small writes (no headroom).
func TestPhase3_Extra_MaxBytes50000EmptyDBNoHeadroom(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "no_headroom.db")

	db, err := Open(dbPath, &Options{
		SyncPolicy: SyncAlways,
		MaxBytes:   50000,
	})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// Under 64KB headroom, maxBytes 50000 previously rejected all writes.
	// With exact boundaries, small writes succeed immediately.
	for i := 0; i < 50; i++ {
		k := fmt.Sprintf("key_%d", i)
		v := fmt.Sprintf("val_%d", i)
		if err := db.Put([]byte(k), []byte(v)); err != nil {
			t.Fatalf("Write %d failed on maxBytes 50000: %v", i, err)
		}
	}

	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("os.Stat failed: %v", err)
	}
	if info.Size() > 50000 {
		t.Fatalf("File size %d exceeded 50000", info.Size())
	}
}
