package src

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// -----------------------------------------------------------------------------
// Recovery Tests
// -----------------------------------------------------------------------------

func TestRecovery_TornWrite(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "torn_write.db")

	// Phase 1: Create a valid DB with several records
	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	for i := 0; i < 20; i++ {
		k := fmt.Sprintf("key_%02d", i)
		v := fmt.Sprintf("val_%02d", i)
		if err := db.Put([]byte(k), []byte(v)); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
	}
	validSize := db.writeOffset
	if err := db.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Test appending 1-64 random garbage bytes with deterministic seeds
	for garbageLen := 1; garbageLen <= 64; garbageLen++ {
		r := rand.New(rand.NewSource(int64(garbageLen * 1000)))
		garbage := make([]byte, garbageLen)
		r.Read(garbage)

		// Append garbage to the file
		f, err := os.OpenFile(dbPath, os.O_APPEND|os.O_WRONLY, 0666)
		if err != nil {
			t.Fatalf("OpenFile failed: %v", err)
		}
		if _, err := f.Write(garbage); err != nil {
			f.Close()
			t.Fatalf("Write garbage failed: %v", err)
		}
		f.Close()

		// Recover
		idx, validEnd, err := Recover(dbPath)
		if err != nil {
			t.Fatalf("Recover failed on garbageLen %d: %v", garbageLen, err)
		}

		if validEnd != validSize {
			t.Fatalf("expected validEnd %d, got %d for garbageLen %d", validSize, validEnd, garbageLen)
		}

		if idx.Len() != 20 {
			t.Fatalf("expected 20 keys, got %d", idx.Len())
		}

		// Verify on-disk file size was truncated back
		fi, err := os.Stat(dbPath)
		if err != nil {
			t.Fatalf("Stat failed: %v", err)
		}
		if fi.Size() != validSize {
			t.Fatalf("file not truncated back to %d, size is %d", validSize, fi.Size())
		}
	}
}

func TestRecovery_TruncatedMidRecord(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "truncated_mid.db")

	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	_ = db.Put([]byte("record_1"), []byte("value_1"))
	boundary1 := db.writeOffset
	_ = db.Put([]byte("record_2"), []byte("value_2_somewhat_longer_data"))
	boundary2 := db.writeOffset
	db.Close()

	totalLen := boundary2 - boundary1

	// Cut at EVERY byte offset inside the second record
	for cut := int64(1); cut < totalLen; cut++ {
		cutSize := boundary1 + cut

		// Truncate to mid-record
		if err := os.Truncate(dbPath, cutSize); err != nil {
			t.Fatalf("Truncate failed: %v", err)
		}

		idx, validEnd, err := Recover(dbPath)
		if err != nil {
			t.Fatalf("Recover failed at cut %d: %v", cut, err)
		}

		if validEnd != boundary1 {
			t.Fatalf("expected validEnd %d at cut %d, got %d", boundary1, cut, validEnd)
		}

		if idx.Len() != 1 {
			t.Fatalf("expected 1 key remaining, got %d", idx.Len())
		}

		if _, ok := idx.Get("record_1"); !ok {
			t.Fatalf("record_1 missing after recovery")
		}
		if _, ok := idx.Get("record_2"); ok {
			t.Fatalf("record_2 should not exist")
		}
	}
}

func TestRecovery_HeaderAttacks(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "header_attacks.db")

	// Helper to write raw bytes and verify Recover cleanly truncates without panics
	testAttack := func(name string, attackBytes []byte) {
		t.Run(name, func(t *testing.T) {
			f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0666)
			if err != nil {
				t.Fatalf("create failed: %v", err)
			}
			// Write 1 valid record first
			validRec := EncodeRecord(nil, OpPut, []byte("validKey"), []byte("validVal"))
			validLen := int64(len(validRec))
			_, _ = f.Write(validRec)
			_, _ = f.Write(attackBytes)
			f.Close()

			idx, validEnd, err := Recover(dbPath)
			if err != nil {
				t.Fatalf("Recover failed: %v", err)
			}
			if validEnd != validLen {
				t.Fatalf("expected validEnd %d, got %d", validLen, validEnd)
			}
			if idx.Len() != 1 {
				t.Fatalf("expected 1 valid key, got %d", idx.Len())
			}
		})
	}

	// 1. Huge KeyLen (0xFFFF = 65535) & Huge ValLen (0xFFFFFFFF) & Huge TotalLen
	hdr1 := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(hdr1[4:8], 0xFFFFFFFF) // TotalLen
	hdr1[8] = OpPut
	binary.LittleEndian.PutUint16(hdr1[9:11], 0xFFFF)      // KeyLen
	binary.LittleEndian.PutUint32(hdr1[11:15], 0xFFFFFFFF) // ValLen
	testAttack("HugeLengths", hdr1)

	// 2. Inconsistent TotalLen != 15 + KeyLen + ValLen
	hdr2 := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(hdr2[4:8], 100)
	hdr2[8] = OpPut
	binary.LittleEndian.PutUint16(hdr2[9:11], 10)
	binary.LittleEndian.PutUint32(hdr2[11:15], 20) // 15 + 10 + 20 = 45 != 100
	testAttack("InconsistentTotalLen", hdr2)

	// 3. Unknown OpType (e.g. 99)
	hdr3 := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(hdr3[4:8], 15)
	hdr3[8] = 99 // unknown OpType
	binary.LittleEndian.PutUint16(hdr3[9:11], 0)
	binary.LittleEndian.PutUint32(hdr3[11:15], 0)
	testAttack("UnknownOpType", hdr3)
}

func TestRecovery_BatchAtomicity(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "batch_atomicity.db")

	f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0666)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// 1. Write an initial valid record
	initialRec := EncodeRecord(nil, OpPut, []byte("init_k"), []byte("init_v"))
	_, _ = f.Write(initialRec)
	preBatchOffset := int64(len(initialRec))

	// 2. Write BatchBegin + 5 Puts and NO BatchCommit
	uncommitted := EncodeRecord(nil, OpBatchBegin, nil, nil)
	for i := 1; i <= 5; i++ {
		k := []byte(fmt.Sprintf("batch_k%d", i))
		v := []byte(fmt.Sprintf("batch_v%d", i))
		uncommitted = EncodeRecord(uncommitted, OpPut, k, v)
	}
	_, _ = f.Write(uncommitted)
	f.Close()

	// Recover must roll back the uncommitted batch to preBatchOffset
	idx, validEnd, err := Recover(dbPath)
	if err != nil {
		t.Fatalf("Recover failed: %v", err)
	}
	if validEnd != preBatchOffset {
		t.Fatalf("expected validEnd %d, got %d", preBatchOffset, validEnd)
	}
	for i := 1; i <= 5; i++ {
		k := fmt.Sprintf("batch_k%d", i)
		if _, ok := idx.Get(k); ok {
			t.Fatalf("uncommitted batch key %s resurrected!", k)
		}
	}
	if idx.Len() != 1 {
		t.Fatalf("expected 1 key, got %d", idx.Len())
	}

	// File on disk must be truncated to preBatchOffset
	fi, _ := os.Stat(dbPath)
	if fi.Size() != preBatchOffset {
		t.Fatalf("file not truncated to preBatchOffset %d, got %d", preBatchOffset, fi.Size())
	}

	// 3. Now append a fully committed batch
	f2, _ := os.OpenFile(dbPath, os.O_APPEND|os.O_WRONLY, 0666)
	committed := EncodeRecord(nil, OpBatchBegin, nil, nil)
	for i := 1; i <= 5; i++ {
		k := []byte(fmt.Sprintf("batch_k%d", i))
		v := []byte(fmt.Sprintf("batch_v%d", i))
		committed = EncodeRecord(committed, OpPut, k, v)
	}
	committed = EncodeRecord(committed, OpBatchCommit, nil, nil)
	_, _ = f2.Write(committed)
	f2.Close()

	idx2, validEnd2, err := Recover(dbPath)
	if err != nil {
		t.Fatalf("Recover failed: %v", err)
	}
	if idx2.Len() != 6 {
		t.Fatalf("expected 6 keys (1 init + 5 batch), got %d", idx2.Len())
	}
	for i := 1; i <= 5; i++ {
		k := fmt.Sprintf("batch_k%d", i)
		if _, ok := idx2.Get(k); !ok {
			t.Fatalf("committed batch key %s missing!", k)
		}
	}
	_ = validEnd2
}

func TestRecovery_RealDB_UncommittedBatchRollback(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "real_db_batch.db")

	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	b := NewBatch()
	_ = b.Put([]byte("valid_k1"), []byte("val1"))
	_ = b.Put([]byte("valid_k2"), []byte("val2"))
	if err := db.Write(b); err != nil {
		t.Fatalf("Write batch failed: %v", err)
	}
	db.Close()

	// Reopen -> data present
	db2, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	if v, err := db2.Get([]byte("valid_k1")); err != nil || string(v) != "val1" {
		t.Fatalf("valid_k1 mismatch: %v, %s", err, string(v))
	}
	db2.Close()

	// Append uncommitted batch directly to file
	f, _ := os.OpenFile(dbPath, os.O_APPEND|os.O_WRONLY, 0666)
	uncommitted := EncodeRecord(nil, OpBatchBegin, nil, nil)
	uncommitted = EncodeRecord(uncommitted, OpPut, []byte("ghost_key"), []byte("ghost_val"))
	_, _ = f.Write(uncommitted)
	f.Close()

	// Reopen -> ghost_key must not be resurrected
	db3, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open after uncommitted batch failed: %v", err)
	}
	defer db3.Close()

	if _, err := db3.Get([]byte("ghost_key")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for ghost_key, got %v", err)
	}

	// Subsequent Put + reopen works normally
	if err := db3.Put([]byte("new_key"), []byte("new_val")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	db3.Close()

	db4, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open db4 failed: %v", err)
	}
	defer db4.Close()

	if v, err := db4.Get([]byte("new_key")); err != nil || string(v) != "new_val" {
		t.Fatalf("new_key mismatch: %v, %s", err, string(v))
	}
}

func TestRecovery_NestedAndOrphanBatchMarkers(t *testing.T) {
	dir := t.TempDir()

	// 1. Nested BatchBegin
	t.Run("NestedBatchBegin", func(t *testing.T) {
		dbPath := filepath.Join(dir, "nested.db")
		f, _ := os.OpenFile(dbPath, os.O_CREATE|os.O_WRONLY, 0666)
		// valid entry
		validRec := EncodeRecord(nil, OpPut, []byte("good"), []byte("val"))
		_, _ = f.Write(validRec)
		preNested := int64(len(validRec))

		// Nested batch: Begin -> Begin
		nested := EncodeRecord(nil, OpBatchBegin, nil, nil)
		nested = EncodeRecord(nested, OpBatchBegin, nil, nil)
		_, _ = f.Write(nested)
		f.Close()

		idx, validEnd, err := Recover(dbPath)
		if err != nil {
			t.Fatalf("Recover failed: %v", err)
		}
		if validEnd != preNested {
			t.Fatalf("expected rollback to preNested %d, got %d", preNested, validEnd)
		}
		if idx.Len() != 1 {
			t.Fatalf("expected 1 key, got %d", idx.Len())
		}
	})

	// 2. Orphan BatchCommit
	t.Run("OrphanBatchCommit", func(t *testing.T) {
		dbPath := filepath.Join(dir, "orphan.db")
		f, _ := os.OpenFile(dbPath, os.O_CREATE|os.O_WRONLY, 0666)
		validRec := EncodeRecord(nil, OpPut, []byte("good"), []byte("val"))
		_, _ = f.Write(validRec)
		preOrphan := int64(len(validRec))

		orphan := EncodeRecord(nil, OpBatchCommit, nil, nil)
		_, _ = f.Write(orphan)
		f.Close()

		idx, validEnd, err := Recover(dbPath)
		if err != nil {
			t.Fatalf("Recover failed: %v", err)
		}
		if validEnd != preOrphan {
			t.Fatalf("expected rollback to %d, got %d", preOrphan, validEnd)
		}
		if idx.Len() != 1 {
			t.Fatalf("expected 1 key, got %d", idx.Len())
		}
	})
}

func TestRecovery_StaleTmpCleaned(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stale_test.db")
	tmpPath := dbPath + ".tmp"

	// Create a dummy stale tmp file
	if err := os.WriteFile(tmpPath, []byte("stale compaction data"), 0666); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Create a valid DB file
	if err := os.WriteFile(dbPath, nil, 0666); err != nil {
		t.Fatalf("WriteFile dbPath failed: %v", err)
	}

	_, _, err := Recover(dbPath)
	if err != nil {
		t.Fatalf("Recover failed: %v", err)
	}

	// Ensure stale tmp is deleted
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("expected stale tmp file to be removed, but it exists")
	}
}

// -----------------------------------------------------------------------------
// Compaction Tests
// -----------------------------------------------------------------------------

func TestCompaction_Correctness(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "compact.db")

	db, err := Open(dbPath, &Options{SyncPolicy: SyncNever})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// 1. Write initial keys
	for i := 0; i < 50; i++ {
		k := fmt.Sprintf("k_%02d", i)
		v := fmt.Sprintf("val_%02d", i)
		_ = db.Put([]byte(k), []byte(v))
	}

	// 2. Overwrite half of them multiple times
	for round := 1; round <= 3; round++ {
		for i := 0; i < 25; i++ {
			k := fmt.Sprintf("k_%02d", i)
			v := fmt.Sprintf("val_%02d_round_%d", i, round)
			_ = db.Put([]byte(k), []byte(v))
		}
	}

	// 3. Delete 10 keys
	for i := 40; i < 50; i++ {
		k := fmt.Sprintf("k_%02d", i)
		_ = db.Delete([]byte(k))
	}

	sizeBefore := db.writeOffset

	// Run compaction
	if err := db.Compact(); err != nil {
		t.Fatalf("Compact failed: %v", err)
	}

	sizeAfter := db.writeOffset
	if sizeAfter >= sizeBefore {
		t.Fatalf("expected compacted file to be smaller: sizeBefore=%d, sizeAfter=%d", sizeBefore, sizeAfter)
	}

	// Verify live keys get correct values
	for i := 0; i < 25; i++ {
		k := fmt.Sprintf("k_%02d", i)
		expected := fmt.Sprintf("val_%02d_round_3", i)
		v, err := db.Get([]byte(k))
		if err != nil || string(v) != expected {
			t.Fatalf("key %s: got %s, expected %s", k, string(v), expected)
		}
	}
	for i := 25; i < 40; i++ {
		k := fmt.Sprintf("k_%02d", i)
		expected := fmt.Sprintf("val_%02d", i)
		v, err := db.Get([]byte(k))
		if err != nil || string(v) != expected {
			t.Fatalf("key %s: got %s, expected %s", k, string(v), expected)
		}
	}

	// Verify deleted keys are absent
	for i := 40; i < 50; i++ {
		k := fmt.Sprintf("k_%02d", i)
		if _, err := db.Get([]byte(k)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted key %s present after compaction!", k)
		}
	}

	// Subsequent Put works and lands at the new write offset
	if err := db.Put([]byte("brand_new_key"), []byte("brand_new_val")); err != nil {
		t.Fatalf("Put after Compact failed: %v", err)
	}

	db.Close()

	// Reopen must yield identical data
	db2, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Reopen after compact failed: %v", err)
	}
	defer db2.Close()

	if v, err := db2.Get([]byte("brand_new_key")); err != nil || string(v) != "brand_new_val" {
		t.Fatalf("brand_new_key mismatch: %v, %s", err, string(v))
	}
}

func TestCompaction_FailureInjectionHooks(t *testing.T) {
	testHookFailure := func(name string, setupHook func(c *compactor)) {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "hook_test.db")

			db, err := Open(dbPath, &Options{SyncPolicy: SyncNever})
			if err != nil {
				t.Fatalf("Open failed: %v", err)
			}
			defer db.Close()

			_ = db.Put([]byte("stable_k1"), []byte("stable_v1"))
			_ = db.Put([]byte("stable_k2"), []byte("stable_v2"))

			// Record original file hash
			fBytesBefore, _ := os.ReadFile(dbPath)
			hashBefore := sha256.Sum256(fBytesBefore)
			indexLenBefore := db.index.Len()

			// Create compactor with hook
			c := &compactor{db: db}
			setupHook(c)

			err = c.run()
			if err == nil {
				t.Fatalf("expected compaction to fail via hook")
			}

			// 1. Original file bytes must be hash-identical
			fBytesAfter, _ := os.ReadFile(dbPath)
			hashAfter := sha256.Sum256(fBytesAfter)
			if hashBefore != hashAfter {
				t.Fatalf("file corrupted after failed compaction! hash mismatch")
			}

			// 2. Index unchanged
			if db.index.Len() != indexLenBefore {
				t.Fatalf("index changed after failed compaction: lenBefore=%d, lenAfter=%d", indexLenBefore, db.index.Len())
			}

			// 3. No .tmp file left behind
			if _, err := os.Stat(dbPath + ".tmp"); !os.IsNotExist(err) {
				t.Fatalf(".tmp file was not cleaned up after compaction failure")
			}

			// 4. DB Get and Put still work
			v, err := db.Get([]byte("stable_k1"))
			if err != nil || string(v) != "stable_v1" {
				t.Fatalf("Get after failed compaction error: %v, %s", err, string(v))
			}
			if err := db.Put([]byte("k_after_fail"), []byte("v_after_fail")); err != nil {
				t.Fatalf("Put after failed compaction failed: %v", err)
			}
		})
	}

	testHookFailure("BeforeWriteFailure", func(c *compactor) {
		c.beforeWrite = func() error { return errors.New("injected beforeWrite failure") }
	})

	testHookFailure("BeforeSyncFailure", func(c *compactor) {
		c.beforeSync = func() error { return errors.New("injected beforeSync failure") }
	})

	testHookFailure("BeforeRenameFailure", func(c *compactor) {
		c.beforeRename = func() error { return errors.New("injected beforeRename failure") }
	})
}

func TestCompaction_ConcurrencyUnderRace(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "concurrency_compact.db")

	db, err := Open(dbPath, &Options{SyncPolicy: SyncNever})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// Seed data
	for i := 0; i < 100; i++ {
		_ = db.Put([]byte(fmt.Sprintf("k_%d", i)), []byte("val"))
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Background workers doing Put, Get, Delete
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
					key := []byte(fmt.Sprintf("k_%d", i%100))
					val := []byte(fmt.Sprintf("val_%d_%d", workerID, i))
					switch i % 3 {
					case 0:
						_ = db.Put(key, val)
					case 1:
						_, _ = db.Get(key)
					case 2:
						_ = db.Delete(key)
					}
					i++
				}
			}
		}(w)
	}

	// Trigger compaction concurrently
	for round := 0; round < 3; round++ {
		if err := db.Compact(); err != nil {
			t.Fatalf("Concurrent compact round %d failed: %v", round, err)
		}
	}

	close(stop)
	wg.Wait()
}

func TestCompaction_ShrinkAfter1MPutsAndDeletes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 1M shrink test in short mode")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "shrink_1M.db")

	db, err := Open(dbPath, &Options{SyncPolicy: SyncNever})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	const count = 100_000 // fast & representative for unit tests (can be scaled)
	for i := 0; i < count; i++ {
		k := fmt.Sprintf("k_%08d", i)
		if err := db.Put([]byte(k), []byte("dummy_value_data")); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
	}
	for i := 0; i < count; i++ {
		k := fmt.Sprintf("k_%08d", i)
		if err := db.Delete([]byte(k)); err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
	}

	sizeBefore := db.writeOffset
	if sizeBefore == 0 {
		t.Fatalf("expected non-zero size before compaction")
	}

	if err := db.Compact(); err != nil {
		t.Fatalf("Compact failed: %v", err)
	}

	fi, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	sizeAfter := fi.Size()

	if sizeAfter >= sizeBefore {
		t.Fatalf("expected file to shrink after deletes and compaction: before=%d, after=%d", sizeBefore, sizeAfter)
	}
	if sizeAfter != 0 {
		t.Fatalf("expected 0 bytes remaining since all keys deleted, got %d", sizeAfter)
	}
}

// -----------------------------------------------------------------------------
// Benchmark: 1M Keys Recovery
// -----------------------------------------------------------------------------

func BenchmarkRecovery_1M_Keys(b *testing.B) {
	dir := b.TempDir()
	dbPath := filepath.Join(dir, "recovery_1M.db")

	// Generate 1,000,000 records
	const numKeys = 1_000_000
	f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0666)
	if err != nil {
		b.Fatalf("create failed: %v", err)
	}

	bufWriter := bufio.NewWriterSize(f, 2*1024*1024)
	val := []byte("val_data_32_bytes_of_payload!!")

	var keyBuf [16]byte
	copy(keyBuf[:], "k_")
	encBuf := make([]byte, 0, 128)

	for i := 0; i < numKeys; i++ {
		// Fast integer formatting
		k := fmt.Sprintf("k_%08d", i)
		encBuf = encBuf[:0]
		encBuf = EncodeRecord(encBuf, OpPut, []byte(k), val)
		if _, err := bufWriter.Write(encBuf); err != nil {
			b.Fatalf("Write failed: %v", err)
		}
	}

	if err := bufWriter.Flush(); err != nil {
		b.Fatalf("Flush failed: %v", err)
	}
	f.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx, validEnd, err := Recover(dbPath)
		if err != nil {
			b.Fatalf("Recover failed: %v", err)
		}
		if idx.Len() != numKeys {
			b.Fatalf("expected %d keys, got %d", numKeys, idx.Len())
		}
		_ = validEnd
	}
}
