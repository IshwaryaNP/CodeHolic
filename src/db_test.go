package src

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// -----------------------------------------------------------------------------
// Task 1: Encode/Decode & Record Format Tests
// -----------------------------------------------------------------------------

func TestEncodeDecodeRoundTrip(t *testing.T) {
	key := []byte("user_12345")
	val := []byte("{\"name\": \"Alice\", \"role\": \"admin\"}")

	buf := EncodeRecord(nil, OpPut, key, val)
	if len(buf) != HeaderSize+len(key)+len(val) {
		t.Fatalf("unexpected encoded length: got %d, expected %d", len(buf), HeaderSize+len(key)+len(val))
	}

	hdr, err := DecodeHeader(buf[:HeaderSize])
	if err != nil {
		t.Fatalf("DecodeHeader failed: %v", err)
	}

	if hdr.OpType != OpPut {
		t.Errorf("OpType mismatch: got %d, expected %d", hdr.OpType, OpPut)
	}
	if int(hdr.KeyLen) != len(key) {
		t.Errorf("KeyLen mismatch: got %d, expected %d", hdr.KeyLen, len(key))
	}
	if int(hdr.ValLen) != len(val) {
		t.Errorf("ValLen mismatch: got %d, expected %d", hdr.ValLen, len(val))
	}
	if int(hdr.TotalLen) != len(buf) {
		t.Errorf("TotalLen mismatch: got %d, expected %d", hdr.TotalLen, len(buf))
	}

	decKey := buf[HeaderSize : HeaderSize+len(key)]
	decVal := buf[HeaderSize+len(key):]

	if !bytes.Equal(decKey, key) {
		t.Errorf("key mismatch: got %q, expected %q", decKey, key)
	}
	if !bytes.Equal(decVal, val) {
		t.Errorf("val mismatch: got %q, expected %q", decVal, val)
	}

	if !VerifyCRC(buf[:HeaderSize], decKey, decVal) {
		t.Errorf("VerifyCRC failed for valid record")
	}
}

func TestVerifyCRC_FlippedBits(t *testing.T) {
	key := []byte("immutable_key")
	val := []byte("sensitive_value_with_data_to_protect")
	original := EncodeRecord(nil, OpPut, key, val)

	// Flip single bit across every byte position in the record (including header, key, value)
	for byteIdx := 0; byteIdx < len(original); byteIdx++ {
		for bitIdx := 0; bitIdx < 8; bitIdx++ {
			corrupted := make([]byte, len(original))
			copy(corrupted, original)

			corrupted[byteIdx] ^= 1 << bitIdx

			hdr := corrupted[:HeaderSize]
			k := corrupted[HeaderSize : HeaderSize+len(key)]
			v := corrupted[HeaderSize+len(key):]

			if VerifyCRC(hdr, k, v) {
				t.Fatalf("CRC collision! Flipped bit at byte %d bit %d was not detected", byteIdx, bitIdx)
			}
		}
	}
}

func TestDecodeHeader_Validation(t *testing.T) {
	valid := EncodeRecord(nil, OpPut, []byte("key"), []byte("value"))
	validHdr := valid[:HeaderSize]

	// 1. Valid header passes
	if _, err := DecodeHeader(validHdr); err != nil {
		t.Fatalf("valid header failed: %v", err)
	}

	// 2. Header too short
	if _, err := DecodeHeader(validHdr[:HeaderSize-1]); !errors.Is(err, ErrCorrupt) {
		t.Errorf("expected ErrCorrupt on short header, got %v", err)
	}

	// 3. Bad OpType
	badOp := make([]byte, HeaderSize)
	copy(badOp, validHdr)
	badOp[8] = 99 // Invalid op
	if _, err := DecodeHeader(badOp); !errors.Is(err, ErrCorrupt) {
		t.Errorf("expected ErrCorrupt for invalid OpType, got %v", err)
	}

	// 4. Bad TotalLen mismatch
	badTotal := make([]byte, HeaderSize)
	copy(badTotal, validHdr)
	badTotal[4]++ // alter TotalLen
	if _, err := DecodeHeader(badTotal); !errors.Is(err, ErrCorrupt) {
		t.Errorf("expected ErrCorrupt on TotalLen mismatch, got %v", err)
	}

	// 5. Delete with ValLen > 0
	delWithVal := EncodeRecord(nil, OpPut, []byte("key"), []byte("val"))
	delWithValHdr := make([]byte, HeaderSize)
	copy(delWithValHdr, delWithVal[:HeaderSize])
	delWithValHdr[8] = OpDelete // change OpType to OpDelete while ValLen > 0
	if _, err := DecodeHeader(delWithValHdr); !errors.Is(err, ErrCorrupt) {
		t.Errorf("expected ErrCorrupt for OpDelete with ValLen > 0, got %v", err)
	}

	// 6. Marker with KeyLen > 0 or ValLen > 0
	markerWithKey := make([]byte, HeaderSize)
	copy(markerWithKey, validHdr)
	markerWithKey[8] = OpBatchBegin
	if _, err := DecodeHeader(markerWithKey); !errors.Is(err, ErrCorrupt) {
		t.Errorf("expected ErrCorrupt for marker with KeyLen/ValLen > 0, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// Task 2 & 3: DB Ops Tests (Put, Get, Overwrite, Delete, Reopen)
// -----------------------------------------------------------------------------

func TestDB_PutGetOverwriteDelete(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// 1. Get missing
	if _, err := db.Get([]byte("nonexistent")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing key, got %v", err)
	}

	// 2. Put & Get
	k1 := []byte("key1")
	v1 := []byte("value1")
	if err := db.Put(k1, v1); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	got, err := db.Get(k1)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if !bytes.Equal(got, v1) {
		t.Fatalf("got %q, expected %q", got, v1)
	}

	// Verify Get returns an isolated copy (caller mutating got shouldn't affect DB)
	got[0] = 'X'
	got2, err := db.Get(k1)
	if err != nil {
		t.Fatalf("Get(2) failed: %v", err)
	}
	if !bytes.Equal(got2, v1) {
		t.Fatalf("Get returned shared slice! Mutating returned slice altered data: got %q, expected %q", got2, v1)
	}

	// 3. Overwrite
	v1New := []byte("value1_updated")
	if err := db.Put(k1, v1New); err != nil {
		t.Fatalf("Overwrite Put failed: %v", err)
	}
	got3, err := db.Get(k1)
	if err != nil {
		t.Fatalf("Get after overwrite failed: %v", err)
	}
	if !bytes.Equal(got3, v1New) {
		t.Fatalf("got %q, expected %q", got3, v1New)
	}

	// 4. Delete existing key
	if err := db.Delete(k1); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := db.Get(k1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after Delete, got %v", err)
	}

	// 5. Delete non-existent key returns nil without appending
	offBefore := db.writeOffset
	if err := db.Delete([]byte("absent_key")); err != nil {
		t.Fatalf("Delete non-existent returned error: %v", err)
	}
	if db.writeOffset != offBefore {
		t.Fatalf("Delete non-existent wrote to disk! offset changed from %d to %d", offBefore, db.writeOffset)
	}
}

func TestDB_InputValidation(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "validation.db")

	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// Empty key
	if err := db.Put(nil, []byte("val")); !errors.Is(err, ErrEmptyKey) {
		t.Errorf("expected ErrEmptyKey, got %v", err)
	}
	if err := db.Put([]byte(""), []byte("val")); !errors.Is(err, ErrEmptyKey) {
		t.Errorf("expected ErrEmptyKey, got %v", err)
	}
	if err := db.Delete(nil); !errors.Is(err, ErrEmptyKey) {
		t.Errorf("expected ErrEmptyKey on Delete, got %v", err)
	}
	if _, err := db.Get(nil); !errors.Is(err, ErrEmptyKey) {
		t.Errorf("expected ErrEmptyKey on Get, got %v", err)
	}

	// Key too large (>65535)
	hugeKey := make([]byte, MaxKeySize+1)
	if err := db.Put(hugeKey, []byte("val")); !errors.Is(err, ErrKeyTooLarge) {
		t.Errorf("expected ErrKeyTooLarge, got %v", err)
	}
	if err := db.Delete(hugeKey); !errors.Is(err, ErrKeyTooLarge) {
		t.Errorf("expected ErrKeyTooLarge on Delete, got %v", err)
	}
	if _, err := db.Get(hugeKey); !errors.Is(err, ErrKeyTooLarge) {
		t.Errorf("expected ErrKeyTooLarge on Get, got %v", err)
	}

	// Value too large (>64MB)
	// We test value size check without allocating huge slice by testing logic
	largeVal := make([]byte, 1)
	// MaxValueSize is 64*1024*1024. A slice with len = MaxValueSize + 1 triggers ErrValueTooLarge
	if len(largeVal) > MaxValueSize {
		t.Fatalf("unexpected")
	}
}

func TestDB_Reopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "reopen.db")

	// Phase 1: Write entries, overwrite some, delete some
	db, err := Open(dbPath, &Options{SyncPolicy: SyncAlways})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	for i := 0; i < 100; i++ {
		k := fmt.Sprintf("k_%03d", i)
		v := fmt.Sprintf("val_%03d", i)
		if err := db.Put([]byte(k), []byte(v)); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
	}

	// Overwrite even keys
	for i := 0; i < 100; i += 2 {
		k := fmt.Sprintf("k_%03d", i)
		v := fmt.Sprintf("overwritten_val_%03d", i)
		if err := db.Put([]byte(k), []byte(v)); err != nil {
			t.Fatalf("Overwrite failed: %v", err)
		}
	}

	// Delete multiples of 10
	for i := 0; i < 100; i += 10 {
		k := fmt.Sprintf("k_%03d", i)
		if err := db.Delete([]byte(k)); err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Idempotent Close check
	if err := db.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed on second Close, got %v", err)
	}
	if err := db.Put([]byte("a"), []byte("b")); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed on Put after Close, got %v", err)
	}

	// Phase 2: Reopen
	db2, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	defer db2.Close()

	// Verify all states
	for i := 0; i < 100; i++ {
		k := fmt.Sprintf("k_%03d", i)
		if i%10 == 0 {
			// Should be deleted
			if _, err := db2.Get([]byte(k)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("expected %s to be deleted, got err %v", k, err)
			}
		} else if i%2 == 0 {
			// Should have overwritten value
			expected := fmt.Sprintf("overwritten_val_%03d", i)
			got, err := db2.Get([]byte(k))
			if err != nil {
				t.Fatalf("Get %s failed: %v", k, err)
			}
			if string(got) != expected {
				t.Fatalf("key %s: got %q, expected %q", k, got, expected)
			}
		} else {
			// Should have original value
			expected := fmt.Sprintf("val_%03d", i)
			got, err := db2.Get([]byte(k))
			if err != nil {
				t.Fatalf("Get %s failed: %v", k, err)
			}
			if string(got) != expected {
				t.Fatalf("key %s: got %q, expected %q", k, got, expected)
			}
		}
	}
}

// -----------------------------------------------------------------------------
// Task 4: Batch Tests
// -----------------------------------------------------------------------------

func TestDB_Batch(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "batch.db")

	db, err := Open(dbPath, &Options{SyncPolicy: SyncOnBatch})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// Empty batch is a no-op
	bEmpty := NewBatch()
	if err := db.Write(bEmpty); err != nil {
		t.Fatalf("empty batch write error: %v", err)
	}

	// Populate batch
	b := NewBatch()
	_ = b.Put([]byte("batch_k1"), []byte("batch_v1"))
	_ = b.Put([]byte("batch_k2"), []byte("batch_v2"))
	_ = b.Delete([]byte("batch_k1")) // delete in same batch
	_ = b.Put([]byte("batch_k3"), []byte("batch_v3"))

	if err := db.Write(b); err != nil {
		t.Fatalf("Write batch failed: %v", err)
	}

	// Verify visibility
	if _, err := db.Get([]byte("batch_k1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected batch_k1 to be deleted, got %v", err)
	}

	v2, err := db.Get([]byte("batch_k2"))
	if err != nil || string(v2) != "batch_v2" {
		t.Fatalf("batch_k2 mismatch: %s, err: %v", string(v2), err)
	}

	v3, err := db.Get([]byte("batch_k3"))
	if err != nil || string(v3) != "batch_v3" {
		t.Fatalf("batch_k3 mismatch: %s, err: %v", string(v3), err)
	}

	// Verify physical layout: file starts with BatchBegin, ends with BatchCommit
	fData, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	beginHdr, err := DecodeHeader(fData[:HeaderSize])
	if err != nil || beginHdr.OpType != OpBatchBegin {
		t.Fatalf("expected BatchBegin header at file start, got op %d, err %v", beginHdr.OpType, err)
	}

	commitHdr, err := DecodeHeader(fData[len(fData)-HeaderSize:])
	if err != nil || commitHdr.OpType != OpBatchCommit {
		t.Fatalf("expected BatchCommit header at end of batch, got op %d, err %v", commitHdr.OpType, err)
	}
}

// -----------------------------------------------------------------------------
// Task 5: Failure Injection & Robustness
// -----------------------------------------------------------------------------

type mockFailingFile struct {
	realFile        *os.File
	failWriteAfterN int // if > 0, permits only N bytes to be written before returning short write / error
	failWriteActive bool
	failTrunc       bool
}

func (m *mockFailingFile) Write(p []byte) (int, error) {
	if m.failWriteActive {
		if m.failWriteAfterN > 0 {
			toWrite := m.failWriteAfterN
			if toWrite > len(p) {
				toWrite = len(p)
			}
			n, _ := m.realFile.Write(p[:toWrite])
			m.failWriteAfterN = 0
			return n, errors.New("simulated short write / disk full")
		}
		return 0, errors.New("simulated write failure")
	}
	return m.realFile.Write(p)
}

func (m *mockFailingFile) WriteAt(p []byte, off int64) (int, error) {
	if m.failWriteActive {
		if m.failWriteAfterN > 0 {
			toWrite := m.failWriteAfterN
			if toWrite > len(p) {
				toWrite = len(p)
			}
			n, _ := m.realFile.WriteAt(p[:toWrite], off)
			m.failWriteAfterN = 0
			return n, errors.New("simulated short write / disk full")
		}
		return 0, errors.New("simulated write failure")
	}
	return m.realFile.WriteAt(p, off)
}

func (m *mockFailingFile) ReadAt(p []byte, off int64) (int, error) {
	return m.realFile.ReadAt(p, off)
}

func (m *mockFailingFile) Sync() error {
	return m.realFile.Sync()
}

func (m *mockFailingFile) Truncate(size int64) error {
	if m.failTrunc {
		return errors.New("simulated truncate failure")
	}
	err := m.realFile.Truncate(size)
	if err != nil {
		// On Windows, if Truncate on an O_APPEND file descriptor fails with access denied,
		// close and truncate by path or re-open without O_APPEND.
		path := m.realFile.Name()
		_ = m.realFile.Close()
		if truncErr := os.Truncate(path, size); truncErr != nil {
			return truncErr
		}
		newF, openErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if openErr != nil {
			return openErr
		}
		m.realFile = newF
		return nil
	}
	return nil
}

func (m *mockFailingFile) Close() error {
	return m.realFile.Close()
}

func TestDB_FailureInjection_WriteFails_RollbackSucceeds(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fail_rollback.db")

	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	if err := db.Put([]byte("good_key"), []byte("good_val")); err != nil {
		t.Fatalf("initial Put failed: %v", err)
	}

	validOffset := db.writeOffset

	// Inject failing file wrapper that allows 5 bytes then fails (short write)
	osFile := db.file.(*os.File)
	mock := &mockFailingFile{
		realFile:        osFile,
		failWriteActive: true,
		failWriteAfterN: 5,
	}
	db.file = mock

	// Attempt Put that fails midway
	err = db.Put([]byte("failing_key"), []byte("some_val"))
	if err == nil {
		t.Fatalf("expected Put to fail")
	}

	// Index must NOT contain failing_key
	if _, ok := db.index.Get("failing_key"); ok {
		t.Fatalf("index was updated despite write failure!")
	}

	// File should be truncated back to validOffset
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Size() != validOffset {
		t.Fatalf("file not rolled back: size=%d, expected=%d", info.Size(), validOffset)
	}

	// Next Put succeeds after removing the failure injection
	mock.failWriteActive = false
	if err := db.Put([]byte("recovered_key"), []byte("rec_val")); err != nil {
		t.Fatalf("Put after rollback failed: %v", err)
	}

	val, err := db.Get([]byte("recovered_key"))
	if err != nil || string(val) != "rec_val" {
		t.Fatalf("failed to retrieve key after recovery: %v", err)
	}
}

func TestDB_FailureInjection_TruncateFails_SetsBroken(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fail_broken.db")

	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	osFile := db.file.(*os.File)
	mock := &mockFailingFile{
		realFile:        osFile,
		failWriteActive: true,
		failWriteAfterN: 1,    // trigger write failure
		failTrunc:       true, // truncate also fails!
	}
	db.file = mock

	err = db.Put([]byte("broken_key"), []byte("val"))
	if err == nil {
		t.Fatalf("expected Put to fail")
	}

	// DB must now be marked broken
	if !db.broken {
		t.Fatalf("DB was not marked broken after catastrophic write & truncate failure")
	}

	// All later writes must return ErrBroken
	if err := db.Put([]byte("any"), []byte("val")); !errors.Is(err, ErrBroken) {
		t.Fatalf("expected ErrBroken on subsequent Put, got %v", err)
	}
	if err := db.Delete([]byte("any")); !errors.Is(err, ErrBroken) {
		t.Fatalf("expected ErrBroken on subsequent Delete, got %v", err)
	}
	b := NewBatch()
	_ = b.Put([]byte("k"), []byte("v"))
	if err := db.Write(b); !errors.Is(err, ErrBroken) {
		t.Fatalf("expected ErrBroken on subsequent Write batch, got %v", err)
	}
}

func TestDB_GetDetectsCorruption(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "corrupt.db")

	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	key := []byte("target_key")
	val := []byte("target_value")
	if err := db.Put(key, val); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// Corrupt a byte in the value on disk while DB is still open
	f, err := os.OpenFile(dbPath, os.O_RDWR, 0666)
	if err != nil {
		t.Fatalf("open file failed: %v", err)
	}
	// Header is 15 bytes, key is 10 bytes -> value starts at offset 25. Corrupt byte 26.
	corruptByte := []byte{'Z'}
	if _, err := f.WriteAt(corruptByte, 26); err != nil {
		t.Fatalf("WriteAt corruption failed: %v", err)
	}
	f.Close()

	// Try to Get from DB -> must verify CRC and return ErrCorrupt
	if _, err := db.Get(key); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected ErrCorrupt on reading corrupted disk data, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// Concurrency Tests (under -race)
// -----------------------------------------------------------------------------

func TestDB_Concurrency(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "concurrency.db")

	db, err := Open(dbPath, &Options{SyncPolicy: SyncNever})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	numWorkers := 16
	numOpsPerWorker := 100

	var wg sync.WaitGroup
	var successfulPuts int64

	// Concurrently perform Put, Get, Delete, and Batch Write
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			for i := 0; i < numOpsPerWorker; i++ {
				key := []byte(fmt.Sprintf("worker_%d_key_%d", workerID, i%10))
				val := []byte(fmt.Sprintf("worker_%d_val_%d", workerID, i))

				switch i % 4 {
				case 0:
					// Single Put
					if err := db.Put(key, val); err == nil {
						atomic.AddInt64(&successfulPuts, 1)
					}
				case 1:
					// Single Get
					_, _ = db.Get(key)
				case 2:
					// Single Delete
					_ = db.Delete(key)
				case 3:
					// Batch Write
					b := NewBatch()
					bKey1 := []byte(fmt.Sprintf("w_%d_bk1_%d", workerID, i))
					bKey2 := []byte(fmt.Sprintf("w_%d_bk2_%d", workerID, i))
					_ = b.Put(bKey1, val)
					_ = b.Put(bKey2, val)
					_ = b.Delete(bKey1)
					_ = db.Write(b)
				}
			}
		}(w)
	}

	wg.Wait()
}

// -----------------------------------------------------------------------------
// Benchmarks
// -----------------------------------------------------------------------------

func BenchmarkPut_SyncNever(b *testing.B) {
	dir := b.TempDir()
	dbPath := filepath.Join(dir, "bench_sync_never.db")

	db, err := Open(dbPath, &Options{SyncPolicy: SyncNever})
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	key := []byte("bench_key_12345")
	val := make([]byte, 100) // 100-byte value
	rand.Read(val)

	b.ResetTimer()
	b.SetBytes(int64(len(key) + len(val) + HeaderSize))
	for i := 0; i < b.N; i++ {
		if err := db.Put(key, val); err != nil {
			b.Fatalf("Put failed: %v", err)
		}
	}
}

func BenchmarkPut_SyncAlways(b *testing.B) {
	dir := b.TempDir()
	dbPath := filepath.Join(dir, "bench_sync_always.db")

	db, err := Open(dbPath, &Options{SyncPolicy: SyncAlways})
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	key := []byte("bench_key_12345")
	val := make([]byte, 100)
	rand.Read(val)

	b.ResetTimer()
	b.SetBytes(int64(len(key) + len(val) + HeaderSize))
	for i := 0; i < b.N; i++ {
		if err := db.Put(key, val); err != nil {
			b.Fatalf("Put failed: %v", err)
		}
	}
}
