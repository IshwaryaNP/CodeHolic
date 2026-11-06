package src

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPhase1_TruncateAtEveryByteInsideLastBatch verifies that truncating the data file
// at EVERY byte offset inside the last batch record results in a clean open(), all-or-nothing
// batch semantics, correct key count, and writeOffset == file size.
func TestPhase1_TruncateAtEveryByteInsideLastBatch(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.db")

	db, err := Open(basePath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Write 5 base keys
	for i := 0; i < 5; i++ {
		k := fmt.Sprintf("base_k_%d", i)
		v := fmt.Sprintf("base_v_%d", i)
		if err := db.Put([]byte(k), []byte(v)); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
	}
	baseSize := db.writeOffset

	// Add batch of 3 keys
	b := NewBatch()
	_ = b.Put([]byte("batch_k_0"), []byte("batch_v_0"))
	_ = b.Put([]byte("batch_k_1"), []byte("batch_v_1"))
	_ = b.Put([]byte("batch_k_2"), []byte("batch_v_2"))
	if err := db.Write(b); err != nil {
		t.Fatalf("Batch Write failed: %v", err)
	}
	totalSize := db.writeOffset
	_ = db.Close()

	if totalSize <= baseSize {
		t.Fatalf("Expected totalSize > baseSize, got totalSize=%d, baseSize=%d", totalSize, baseSize)
	}

	// Read entire original data file into memory
	originalData, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	// Truncate at EVERY byte offset strictly between baseSize and totalSize
	for truncOffset := baseSize; truncOffset < totalSize; truncOffset++ {
		testPath := filepath.Join(dir, fmt.Sprintf("test_trunc_%d.db", truncOffset))
		if err := os.WriteFile(testPath, originalData[:truncOffset], 0666); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		reopenedDB, err := Open(testPath, nil)
		if err != nil {
			t.Fatalf("Offset %d: Open failed: %v", truncOffset, err)
		}

		// Assert file size matches writeOffset
		fi, err := os.Stat(testPath)
		if err != nil {
			t.Fatalf("Offset %d: stat failed: %v", truncOffset, err)
		}
		if fi.Size() != reopenedDB.writeOffset {
			t.Fatalf("Offset %d: file size %d != writeOffset %d", truncOffset, fi.Size(), reopenedDB.writeOffset)
		}
		if reopenedDB.writeOffset != baseSize {
			t.Fatalf("Offset %d: expected writeOffset %d, got %d", truncOffset, baseSize, reopenedDB.writeOffset)
		}

		// Key count must be exactly 5 (batch completely absent)
		if reopenedDB.index.Len() != 5 {
			t.Fatalf("Offset %d: expected 5 keys, got %d", truncOffset, reopenedDB.index.Len())
		}

		// None of the batch keys should exist
		for i := 0; i < 3; i++ {
			bk := fmt.Sprintf("batch_k_%d", i)
			if _, err := reopenedDB.Get([]byte(bk)); err != ErrNotFound {
				t.Fatalf("Offset %d: batch key %s found in uncommitted batch!", truncOffset, bk)
			}
		}

		// All 5 base keys must be intact
		for i := 0; i < 5; i++ {
			k := fmt.Sprintf("base_k_%d", i)
			expectedV := fmt.Sprintf("base_v_%d", i)
			gotV, err := reopenedDB.Get([]byte(k))
			if err != nil {
				t.Fatalf("Offset %d: base key %s error: %v", truncOffset, k, err)
			}
			if string(gotV) != expectedV {
				t.Fatalf("Offset %d: base key %s mismatch (got %s, expected %s)", truncOffset, k, gotV, expectedV)
			}
		}

		_ = reopenedDB.Close()
	}
}

// TestPhase1_FlippedByteInBatchPayload verifies that bit flipping inside a batch payload
// causes the entire batch to be discarded.
func TestPhase1_FlippedByteInBatchPayload(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "flipped.db")

	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_ = db.Put([]byte("prior_key"), []byte("prior_val"))
	baseSize := db.writeOffset

	b := NewBatch()
	_ = b.Put([]byte("batch_alpha"), []byte("payload_alpha_data_12345"))
	_ = b.Put([]byte("batch_beta"), []byte("payload_beta_data_67890"))
	if err := db.Write(b); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	_ = db.Close()

	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	// Corrupt a byte in the payload of the first batch item (approx baseSize + 25)
	flipIdx := baseSize + 25
	data[flipIdx] ^= 0xFF
	if err := os.WriteFile(dbPath, data, 0666); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	reopenedDB, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer reopenedDB.Close()

	// Prior key must be intact
	v, err := reopenedDB.Get([]byte("prior_key"))
	if err != nil || string(v) != "prior_val" {
		t.Fatalf("Prior key missing or corrupt: %v, %s", err, v)
	}

	// Batch keys must both be absent
	if _, err := reopenedDB.Get([]byte("batch_alpha")); err != ErrNotFound {
		t.Fatalf("batch_alpha survived payload corruption!")
	}
	if _, err := reopenedDB.Get([]byte("batch_beta")); err != ErrNotFound {
		t.Fatalf("batch_beta survived payload corruption!")
	}
}

// TestPhase1_BatchOrderLastOpWins asserts that ops within a batch apply strictly in order,
// and last op on a key wins.
func TestPhase1_BatchOrderLastOpWins(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "order.db")

	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Case 1: Put X then Delete X in same batch -> X absent
	b1 := NewBatch()
	_ = b1.Put([]byte("key_X"), []byte("val_X1"))
	_ = b1.Delete([]byte("key_X"))
	if err := db.Write(b1); err != nil {
		t.Fatalf("b1 Write failed: %v", err)
	}

	if _, err := db.Get([]byte("key_X")); err != ErrNotFound {
		t.Fatalf("Expected key_X to be absent, got err: %v", err)
	}

	// Case 2: Delete Y then Put Y then Put Y (v3) in same batch -> Y is v3
	b2 := NewBatch()
	_ = b2.Delete([]byte("key_Y"))
	_ = b2.Put([]byte("key_Y"), []byte("val_Y1"))
	_ = b2.Put([]byte("key_Y"), []byte("val_Y_final"))
	if err := db.Write(b2); err != nil {
		t.Fatalf("b2 Write failed: %v", err)
	}

	valY, err := db.Get([]byte("key_Y"))
	if err != nil || string(valY) != "val_Y_final" {
		t.Fatalf("Expected val_Y_final, got err: %v, val: %s", err, valY)
	}

	// Verify durability after reopen
	_ = db.Close()
	reopened, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	defer reopened.Close()

	if _, err := reopened.Get([]byte("key_X")); err != ErrNotFound {
		t.Fatalf("Reopened: expected key_X absent, got: %v", err)
	}
	valY2, err := reopened.Get([]byte("key_Y"))
	if err != nil || string(valY2) != "val_Y_final" {
		t.Fatalf("Reopened: expected val_Y_final, got err: %v, val: %s", err, valY2)
	}
}

// TestPhase1_10000OpBatchRoundTripAndKilledRecovery tests round-trip of 10,000 ops
// and recovery of a killed 10,000-op batch in < 2 seconds.
func TestPhase1_10000OpBatchRoundTripAndKilledRecovery(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "batch_10k.db")

	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	b := NewBatch()
	for i := 0; i < 10000; i++ {
		k := fmt.Sprintf("k10_%05d", i)
		v := fmt.Sprintf("v10_%05d", i)
		_ = b.Put([]byte(k), []byte(v))
	}

	t0 := time.Now()
	if err := db.Write(b); err != nil {
		t.Fatalf("Write 10k batch failed: %v", err)
	}
	t.Logf("10,000 op batch written in %v", time.Since(t0))

	fullSize := db.writeOffset
	_ = db.Close()

	// 1. Reopen full 10k batch and verify
	tOpen := time.Now()
	reopened, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open full 10k failed: %v", err)
	}
	openDur := time.Since(tOpen)
	t.Logf("Full 10k recovery took %v", openDur)
	if openDur > 2*time.Second {
		t.Fatalf("Recovery exceeded 2s limit: took %v", openDur)
	}

	// Verify sample keys
	val0, err := reopened.Get([]byte("k10_00000"))
	if err != nil || string(val0) != "v10_00000" {
		t.Fatalf("k10_00000 mismatch: %v, %s", err, val0)
	}
	val9999, err := reopened.Get([]byte("k10_09999"))
	if err != nil || string(val9999) != "v10_09999" {
		t.Fatalf("k10_09999 mismatch: %v, %s", err, val9999)
	}
	_ = reopened.Close()

	// 2. Simulate killed 10,000-op batch: truncate the file halfway through the 10k batch
	halfway := fullSize / 2
	if err := os.Truncate(dbPath, halfway); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	tKilled := time.Now()
	killedDB, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open killed batch failed: %v", err)
	}
	killedDur := time.Since(tKilled)
	t.Logf("Killed 10k recovery took %v", killedDur)
	if killedDur > 2*time.Second {
		t.Fatalf("Killed recovery exceeded 2s limit: took %v", killedDur)
	}

	// All 10,000 keys must be absent (all-or-nothing rollback)
	if killedDB.index.Len() != 0 {
		t.Fatalf("Expected 0 keys after uncommitted batch rollback, got %d", killedDB.index.Len())
	}
	_ = killedDB.Close()
}

// TestPhase1_CrashInRecoverySimulated runs recovery twice consecutively on the same
// torn file and asserts the results and state are completely identical.
func TestPhase1_CrashInRecoverySimulated(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crash_rec.db")

	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	_ = db.Put([]byte("valid1"), []byte("val1"))
	_ = db.Put([]byte("valid2"), []byte("val2"))
	validEnd := db.writeOffset
	_ = db.Close()

	// Append a torn record (half of a header)
	f, err := os.OpenFile(dbPath, os.O_APPEND|os.O_WRONLY, 0666)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	_, _ = f.Write([]byte{0x01, 0x02, 0x03, 0x04, 0x05})
	_ = f.Close()

	// Recovery run 1
	idx1, end1, err1 := Recover(dbPath)
	if err1 != nil {
		t.Fatalf("Recover 1 failed: %v", err1)
	}
	if end1 != validEnd {
		t.Fatalf("Recover 1 validEnd %d != %d", end1, validEnd)
	}

	// Recovery run 2 (simulating immediate crash & rerun)
	idx2, end2, err2 := Recover(dbPath)
	if err2 != nil {
		t.Fatalf("Recover 2 failed: %v", err2)
	}
	if end2 != end1 {
		t.Fatalf("Recover 2 validEnd %d != %d", end2, end1)
	}
	if idx1.Len() != idx2.Len() {
		t.Fatalf("Index len mismatch: %d vs %d", idx1.Len(), idx2.Len())
	}

	// Assert contents match
	v1, ok1 := idx1.Get("valid1")
	v2, ok2 := idx2.Get("valid1")
	if !ok1 || !ok2 || v1 != v2 {
		t.Fatalf("valid1 offset mismatch")
	}
}

// TestPhase1_NestedAndOrphanBatchMarkers confirms that nested BatchBegin and orphan BatchCommit
// stop recovery at the offending record boundary.
func TestPhase1_NestedAndOrphanBatchMarkers(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "orphan.db")

	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	_ = db.Put([]byte("k_init"), []byte("v_init"))
	cleanEnd := db.writeOffset
	_ = db.Close()

	// Append an orphan BatchCommit marker
	f, err := os.OpenFile(dbPath, os.O_APPEND|os.O_WRONLY, 0666)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	orphanCommit := EncodeRecord(nil, OpBatchCommit, nil, nil)
	_, _ = f.Write(orphanCommit)
	_ = f.Close()

	reopened, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if reopened.writeOffset != cleanEnd {
		t.Fatalf("Expected writeOffset %d, got %d", cleanEnd, reopened.writeOffset)
	}
	if reopened.index.Len() != 1 {
		t.Fatalf("Expected 1 key, got %d", reopened.index.Len())
	}
	_ = reopened.Close()
}
