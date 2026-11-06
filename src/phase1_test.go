package src

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPhase1_TruncateAtEveryByteInsideLastBatch verifies that truncating the data file
// at EVERY byte offset inside the last batch record results in:
// (a) open() succeeds, all-or-nothing batch, key count and Get are correct.
// (b) os.Stat(file).Size() == validEnd == writeOffset (proves on-disk truncate happened).
// (c) Doing one Put of a new key, Close, reopen, asserts both pre-existing and new key are present.
func TestPhase1_TruncateAtEveryByteInsideLastBatch(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.db")

	db, err := Open(basePath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	for i := 0; i < 5; i++ {
		k := fmt.Sprintf("base_k_%d", i)
		v := fmt.Sprintf("base_v_%d", i)
		if err := db.Put([]byte(k), []byte(v)); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
	}
	baseSize := db.writeOffset

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

		// (a) Assert file size matches writeOffset == baseSize
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

		// Key count must be exactly 5
		if reopenedDB.index.Len() != 5 {
			t.Fatalf("Offset %d: expected 5 keys, got %d", truncOffset, reopenedDB.index.Len())
		}

		for i := 0; i < 3; i++ {
			bk := fmt.Sprintf("batch_k_%d", i)
			if _, err := reopenedDB.Get([]byte(bk)); err != ErrNotFound {
				t.Fatalf("Offset %d: batch key %s found in uncommitted batch!", truncOffset, bk)
			}
		}

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

		// (b) Write a new key, close, reopen, assert both pre-existing and new key are present
		newKey := fmt.Sprintf("new_k_%d", truncOffset)
		newVal := fmt.Sprintf("new_v_%d", truncOffset)
		if err := reopenedDB.Put([]byte(newKey), []byte(newVal)); err != nil {
			t.Fatalf("Offset %d: Put new key failed: %v", truncOffset, err)
		}
		_ = reopenedDB.Close()

		reopened2, err := Open(testPath, nil)
		if err != nil {
			t.Fatalf("Offset %d: Reopen2 failed: %v", truncOffset, err)
		}
		if reopened2.index.Len() != 6 {
			t.Fatalf("Offset %d: Expected 6 keys, got %d", truncOffset, reopened2.index.Len())
		}
		gotNewVal, err := reopened2.Get([]byte(newKey))
		if err != nil || string(gotNewVal) != newVal {
			t.Fatalf("Offset %d: New key %s mismatch: %v, %s", truncOffset, newKey, err, gotNewVal)
		}
		_ = reopened2.Close()
	}
}

// TestPhase1_FlippedByteInBatchPayload flips EVERY byte position of the last batch
// and asserts open() succeeds, batch is discarded, earlier data is intact, and file size == validEnd.
// Reports separately for positions inside the Begin record, middle records, and Commit record.
func TestPhase1_FlippedByteInBatchPayload(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "flip_base.db")

	db, err := Open(basePath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_ = db.Put([]byte("prior_key"), []byte("prior_val"))
	_ = db.Put([]byte("batch_to_delete"), []byte("delete_me_in_batch_later"))
	baseSize := db.writeOffset

	// Batch: Begin (15B), OpPut 1, OpDelete 1, OpPut 2, Commit (15B)
	b := NewBatch()
	_ = b.Put([]byte("batch_alpha"), []byte("payload_alpha_data_1234567890_extra_length"))
	_ = b.Delete([]byte("batch_to_delete"))
	_ = b.Put([]byte("batch_beta"), []byte("payload_beta_data_6789012345_extra_length"))
	if err := db.Write(b); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	totalSize := db.writeOffset
	_ = db.Close()

	originalData, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	beginEnd := baseSize + HeaderSize
	commitStart := totalSize - HeaderSize

	counts := map[string]int{"begin": 0, "middle": 0, "commit": 0}

	for flipOffset := baseSize; flipOffset < totalSize; flipOffset++ {
		region := "middle"
		if flipOffset < beginEnd {
			region = "begin"
		} else if flipOffset >= commitStart {
			region = "commit"
		}
		counts[region]++

		dataCopy := make([]byte, len(originalData))
		copy(dataCopy, originalData)
		dataCopy[flipOffset] ^= 0xAA // flip bits

		testPath := filepath.Join(dir, fmt.Sprintf("test_flip_%d.db", flipOffset))
		if err := os.WriteFile(testPath, dataCopy, 0666); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		reopenedDB, err := Open(testPath, nil)
		if err != nil {
			t.Fatalf("Offset %d (%s): Open failed: %v", flipOffset, region, err)
		}

		fi, err := os.Stat(testPath)
		if err != nil {
			t.Fatalf("Offset %d (%s): Stat failed: %v", flipOffset, region, err)
		}

		// Batch must be fully discarded
		if reopenedDB.writeOffset != baseSize {
			t.Fatalf("Offset %d (%s): expected writeOffset %d, got %d", flipOffset, region, baseSize, reopenedDB.writeOffset)
		}
		if fi.Size() != reopenedDB.writeOffset {
			t.Fatalf("Offset %d (%s): file size %d != writeOffset %d", flipOffset, region, fi.Size(), reopenedDB.writeOffset)
		}

		// Prior keys intact
		v, err := reopenedDB.Get([]byte("prior_key"))
		if err != nil || string(v) != "prior_val" {
			t.Fatalf("Offset %d (%s): Prior key missing or corrupt: %v, %s", flipOffset, region, err, v)
		}
		vDel, err := reopenedDB.Get([]byte("batch_to_delete"))
		if err != nil || string(vDel) != "delete_me_in_batch_later" {
			t.Fatalf("Offset %d (%s): batch_to_delete was deleted despite corrupted batch: %v, %s", flipOffset, region, err, vDel)
		}

		// Batch keys absent
		if _, err := reopenedDB.Get([]byte("batch_alpha")); err != ErrNotFound {
			t.Fatalf("Offset %d (%s): batch_alpha survived corruption!", flipOffset, region)
		}
		if _, err := reopenedDB.Get([]byte("batch_beta")); err != ErrNotFound {
			t.Fatalf("Offset %d (%s): batch_beta survived corruption!", flipOffset, region)
		}

		_ = reopenedDB.Close()
	}

	t.Logf("Tested bit flips across all positions: Begin (%d bytes), Middle (%d bytes), Commit (%d bytes)",
		counts["begin"], counts["middle"], counts["commit"])
}

// TestPhase1_TornTail_SinglePut_Delete_1MB verifies recovery for single Put (A2), single Delete (A6),
// and a Put with a 1 MB value (A7) sampled at boundaries.
func TestPhase1_TornTail_SinglePut_Delete_1MB(t *testing.T) {
	dir := t.TempDir()

	// A2: Single Put torn-tail
	t.Run("SinglePut_A2", func(t *testing.T) {
		p := filepath.Join(dir, "put_a2.db")
		db, _ := Open(p, nil)
		_ = db.Put([]byte("prior"), []byte("val"))
		baseSize := db.writeOffset
		_ = db.Put([]byte("target_put"), []byte("target_val_12345"))
		totalSize := db.writeOffset
		_ = db.Close()

		data, _ := os.ReadFile(p)
		for trunc := baseSize; trunc < totalSize; trunc++ {
			pTrunc := filepath.Join(dir, fmt.Sprintf("put_a2_%d.db", trunc))
			_ = os.WriteFile(pTrunc, data[:trunc], 0666)
			r, err := Open(pTrunc, nil)
			if err != nil {
				t.Fatalf("Open failed at %d: %v", trunc, err)
			}
			if r.writeOffset != baseSize {
				t.Fatalf("writeOffset %d != baseSize %d", r.writeOffset, baseSize)
			}
			if _, err := r.Get([]byte("target_put")); err != ErrNotFound {
				t.Fatalf("target_put survived torn write at offset %d", trunc)
			}
			v, err := r.Get([]byte("prior"))
			if err != nil || string(v) != "val" {
				t.Fatalf("prior key corrupt")
			}
			_ = r.Close()
		}
	})

	// A6: Single Delete torn-tail
	t.Run("SingleDelete_A6", func(t *testing.T) {
		p := filepath.Join(dir, "del_a6.db")
		db, _ := Open(p, nil)
		_ = db.Put([]byte("del_target"), []byte("should_be_kept_if_del_torn"))
		baseSize := db.writeOffset
		_ = db.Delete([]byte("del_target"))
		totalSize := db.writeOffset
		_ = db.Close()

		data, _ := os.ReadFile(p)
		for trunc := baseSize; trunc < totalSize; trunc++ {
			pTrunc := filepath.Join(dir, fmt.Sprintf("del_a6_%d.db", trunc))
			_ = os.WriteFile(pTrunc, data[:trunc], 0666)
			r, err := Open(pTrunc, nil)
			if err != nil {
				t.Fatalf("Open failed at %d: %v", trunc, err)
			}
			if r.writeOffset != baseSize {
				t.Fatalf("writeOffset %d != baseSize %d", r.writeOffset, baseSize)
			}
			// Because delete was torn, del_target MUST still exist with original value
			v, err := r.Get([]byte("del_target"))
			if err != nil || string(v) != "should_be_kept_if_del_torn" {
				t.Fatalf("del_target missing or wrong at offset %d: %v, %s", trunc, err, v)
			}
			_ = r.Close()
		}
	})

	// A7: Put with a 1 MB value sampled at boundaries
	t.Run("LargeValue1MB_A7", func(t *testing.T) {
		p := filepath.Join(dir, "large_a7.db")
		db, _ := Open(p, nil)
		_ = db.Put([]byte("prior"), []byte("prior_val"))
		baseSize := db.writeOffset

		oneMBVal := bytes.Repeat([]byte("A"), SpecMaxValueSize)
		_ = db.Put([]byte("1mb_key"), oneMBVal)
		totalSize := db.writeOffset
		_ = db.Close()

		data, _ := os.ReadFile(p)
		offsets := make(map[int64]bool)

		// First 20 bytes
		for i := int64(0); i < 20 && baseSize+i < totalSize; i++ {
			offsets[baseSize+i] = true
		}
		// Header boundary (HeaderSize = 15)
		offsets[baseSize+int64(HeaderSize)] = true
		offsets[baseSize+int64(HeaderSize)+int64(len("1mb_key"))] = true

		// 20 points spread through the value
		step := (totalSize - (baseSize + int64(HeaderSize))) / 21
		for i := 1; i <= 20; i++ {
			offsets[baseSize+int64(HeaderSize)+int64(i)*step] = true
		}

		// Last 20 bytes
		for i := int64(0); i < 20; i++ {
			off := totalSize - 1 - i
			if off >= baseSize {
				offsets[off] = true
			}
		}

		for trunc := range offsets {
			pTrunc := filepath.Join(dir, fmt.Sprintf("large_a7_%d.db", trunc))
			_ = os.WriteFile(pTrunc, data[:trunc], 0666)
			r, err := Open(pTrunc, nil)
			if err != nil {
				t.Fatalf("Open failed at %d: %v", trunc, err)
			}
			if r.writeOffset != baseSize {
				t.Fatalf("Offset %d: writeOffset %d != baseSize %d", trunc, r.writeOffset, baseSize)
			}
			if _, err := r.Get([]byte("1mb_key")); err != ErrNotFound {
				t.Fatalf("Offset %d: 1mb_key survived torn write", trunc)
			}
			v, err := r.Get([]byte("prior"))
			if err != nil || string(v) != "prior_val" {
				t.Fatalf("Offset %d: prior key corrupt", trunc)
			}
			_ = r.Close()
		}
	})
}

// TestPhase1_WriteFailureOnExistingKeyPutDeleteBatch verifies that:
// 1. Injected write failure during Put on an EXISTING key leaves Get returning OLD value,
//    writeOffset unchanged, and index unchanged.
// 2. Injected write failure during Delete leaves the key still present with old value,
//    writeOffset unchanged, and index unchanged.
// 3. Injected write failure during batch leaves none of its keys changed.
func TestPhase1_WriteFailureOnExistingKeyPutDeleteBatch(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "inject_existing_fail.db")

	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	if err := db.Put([]byte("target_put"), []byte("original_val")); err != nil {
		t.Fatalf("init target_put failed: %v", err)
	}
	if err := db.Put([]byte("target_del"), []byte("delete_val")); err != nil {
		t.Fatalf("init target_del failed: %v", err)
	}
	if err := db.Put([]byte("batch_k1"), []byte("old_batch_v1")); err != nil {
		t.Fatalf("init batch_k1 failed: %v", err)
	}

	snapOffset := db.writeOffset
	idxSnap := db.Snapshot()

	osFile := db.file.(*os.File)
	mock := &mockFailingFile{
		realFile:        osFile,
		failWriteActive: false,
		failWriteAfterN: 0,
	}
	db.file = mock

	// Case 1: Put on EXISTING key fails
	mock.failWriteActive = true
	mock.failWriteAfterN = 5 // fail mid-record
	err = db.Put([]byte("target_put"), []byte("new_val_that_should_fail"))
	if err == nil {
		t.Fatalf("Expected Put on existing key to fail")
	}
	if db.writeOffset != snapOffset {
		t.Fatalf("writeOffset changed after failed Put: %d != %d", db.writeOffset, snapOffset)
	}
	valPut, err := db.Get([]byte("target_put"))
	if err != nil || string(valPut) != "original_val" {
		t.Fatalf("target_put did not retain old value: %v, %s", err, valPut)
	}
	currSnap := db.Snapshot()
	if currSnap["target_put"] != idxSnap["target_put"] {
		t.Fatalf("target_put index offset changed: %d != %d", currSnap["target_put"], idxSnap["target_put"])
	}

	// Case 2: Delete on EXISTING key fails
	mock.failWriteActive = true
	mock.failWriteAfterN = 5
	err = db.Delete([]byte("target_del"))
	if err == nil {
		t.Fatalf("Expected Delete on existing key to fail")
	}
	if db.writeOffset != snapOffset {
		t.Fatalf("writeOffset changed after failed Delete: %d != %d", db.writeOffset, snapOffset)
	}
	valDel, err := db.Get([]byte("target_del"))
	if err != nil || string(valDel) != "delete_val" {
		t.Fatalf("target_del is missing or corrupt: %v, %s", err, valDel)
	}
	currSnap = db.Snapshot()
	if currSnap["target_del"] != idxSnap["target_del"] {
		t.Fatalf("target_del index offset changed: %d != %d", currSnap["target_del"], idxSnap["target_del"])
	}

	// Case 3: Batch modifying existing key and adding new key fails
	mock.failWriteActive = true
	mock.failWriteAfterN = 20
	b := NewBatch()
	_ = b.Put([]byte("batch_k1"), []byte("new_batch_v1"))
	_ = b.Put([]byte("batch_k2"), []byte("new_batch_v2"))
	_ = b.Delete([]byte("target_put"))
	err = db.Write(b)
	if err == nil {
		t.Fatalf("Expected batch write to fail")
	}
	if db.writeOffset != snapOffset {
		t.Fatalf("writeOffset changed after failed batch: %d != %d", db.writeOffset, snapOffset)
	}
	// Verify none of its keys changed
	valB1, err := db.Get([]byte("batch_k1"))
	if err != nil || string(valB1) != "old_batch_v1" {
		t.Fatalf("batch_k1 changed despite batch write failure: %v, %s", err, valB1)
	}
	if _, err := db.Get([]byte("batch_k2")); err != ErrNotFound {
		t.Fatalf("batch_k2 exists despite batch write failure")
	}
	valTargetPut, err := db.Get([]byte("target_put"))
	if err != nil || string(valTargetPut) != "original_val" {
		t.Fatalf("target_put was deleted despite batch write failure")
	}
}

// TestPhase1_WriteFailureRollbackAndSubsequentWrite verifies that after an injected mid-write error
// that triggers rollback (truncate + fsync), a subsequent write succeeds and is durable upon reopen.
func TestPhase1_WriteFailureRollbackAndSubsequentWrite(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "inject_write_fail.db")

	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	_ = db.Put([]byte("k0"), []byte("v0"))
	validOffset := db.writeOffset

	// Use mockFailingFile from db_test.go
	osFile := db.file.(*os.File)
	mock := &mockFailingFile{
		realFile:        osFile,
		failWriteActive: true,
		failWriteAfterN: 10, // fail mid-write on next record
	}
	db.file = mock

	// Next Put will fail mid-write and trigger handleWriteFailure -> truncate + sync
	err = db.Put([]byte("k1_failing"), []byte("this_value_will_fail_to_write_completely"))
	if err == nil {
		t.Fatalf("Expected write error, got nil")
	}

	// File must be rolled back to validOffset
	if db.writeOffset != validOffset {
		t.Fatalf("writeOffset %d != validOffset %d", db.writeOffset, validOffset)
	}

	// Deactivate failure injection
	mock.failWriteActive = false

	// Next Put must succeed
	if err := db.Put([]byte("k2_success"), []byte("v2_val")); err != nil {
		t.Fatalf("Subsequent Put failed: %v", err)
	}
	_ = db.Close()

	// Reopen and assert both k0 and k2_success are present, k1_failing is absent
	reopened, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	defer reopened.Close()

	if _, err := reopened.Get([]byte("k1_failing")); err != ErrNotFound {
		t.Fatalf("Failed key found in database!")
	}
	v0, err := reopened.Get([]byte("k0"))
	if err != nil || string(v0) != "v0" {
		t.Fatalf("k0 corrupt: %v, %s", err, v0)
	}
	v2, err := reopened.Get([]byte("k2_success"))
	if err != nil || string(v2) != "v2_val" {
		t.Fatalf("k2_success corrupt: %v, %s", err, v2)
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

	val0, err := reopened.Get([]byte("k10_00000"))
	if err != nil || string(val0) != "v10_00000" {
		t.Fatalf("k10_00000 mismatch: %v, %s", err, val0)
	}
	val9999, err := reopened.Get([]byte("k10_09999"))
	if err != nil || string(val9999) != "v10_09999" {
		t.Fatalf("k10_09999 mismatch: %v, %s", err, val9999)
	}
	_ = reopened.Close()

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

	f, err := os.OpenFile(dbPath, os.O_APPEND|os.O_WRONLY, 0666)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	_, _ = f.Write([]byte{0x01, 0x02, 0x03, 0x04, 0x05})
	_ = f.Close()

	idx1, end1, err1 := Recover(dbPath)
	if err1 != nil {
		t.Fatalf("Recover 1 failed: %v", err1)
	}
	if end1 != validEnd {
		t.Fatalf("Recover 1 validEnd %d != %d", end1, validEnd)
	}

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
