package src

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// BenchmarkRecovery1MillionKeys measures the time it takes for Open() / Recover()
// to scan, validate, rebuild the index, and verify invariants over a 1,000,000 key
// dataset containing overwrites, deletes, a torn tail, and an uncommitted/killed batch.
// Spec constraint: recovery must complete in < 2 seconds.
func BenchmarkRecovery1MillionKeys(b *testing.B) {
	dir := b.TempDir()
	dbPath := filepath.Join(dir, "bench_1m.db")

	f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		b.Fatalf("Failed creating bench db: %v", err)
	}

	b.Logf("Generating 1,000,000-record dataset (overwrites, deletes, batches)...")

	// Pre-generate records using buffered streaming to disk
	buf := make([]byte, 0, 1024*1024)
	flushThreshold := 512 * 1024

	r := rand.New(rand.NewSource(42))
	totalRecords := 1_000_000

	for i := 0; i < totalRecords; i++ {
		key := []byte(fmt.Sprintf("user_%06d", r.Intn(400_000))) // Overwrite collisions
		if r.Float32() < 0.10 {
			// 10% deletes
			buf = EncodeRecord(buf, OpDelete, key, nil)
		} else {
			val := []byte(fmt.Sprintf("v_%06d", i))
			buf = EncodeRecord(buf, OpPut, key, val)
		}

		if len(buf) >= flushThreshold {
			if _, err := f.Write(buf); err != nil {
				b.Fatalf("Write failed: %v", err)
			}
			buf = buf[:0]
		}
	}

	// Append a killed 10,000-op batch (truncated halfway to test batch rollback)
	buf = EncodeRecord(buf, OpBatchBegin, nil, nil)
	for i := 0; i < 5000; i++ {
		k := []byte(fmt.Sprintf("killed_batch_k_%d", i))
		v := []byte(fmt.Sprintf("killed_batch_v_%d", i))
		buf = EncodeRecord(buf, OpPut, k, v)
	}

	// Append a torn tail (10 garbage bytes simulating a torn write at crash)
	buf = append(buf, []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06}...)

	if len(buf) > 0 {
		if _, err := f.Write(buf); err != nil {
			b.Fatalf("Final write failed: %v", err)
		}
	}
	_ = f.Sync()
	_ = f.Close()

	info, err := os.Stat(dbPath)
	if err != nil {
		b.Fatalf("os.Stat failed: %v", err)
	}
	b.Logf("Dataset created: %d bytes (%.2f MB)", info.Size(), float64(info.Size())/(1024*1024))

	b.ResetTimer()

	// Benchmark recovery
	start := time.Now()
	db, err := Open(dbPath, DefaultOptions())
	elapsed := time.Since(start)

	if err != nil {
		b.Fatalf("Open failed on 1M key dataset: %v", err)
	}
	defer db.Close()

	b.StopTimer()

	b.Logf("=== 1M Key Recovery Results ===")
	b.Logf("Elapsed Time: %v", elapsed)
	b.Logf("Key Count in Index: %d", db.index.Len())
	b.Logf("Platform: %s/%s | CPUs: %d", runtime.GOOS, runtime.GOARCH, runtime.NumCPU())

	if elapsed > 2*time.Second {
		b.Fatalf("Recovery exceeded 2.0 second limit: %v", elapsed)
	}
}

// TestBenchmarkRecovery1MillionKeysWrapper runs the benchmark under 'go test'
func TestBenchmarkRecovery1MillionKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 1M recovery test in short mode")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test_1m.db")

	f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		t.Fatalf("Failed creating test db: %v", err)
	}

	t.Logf("Generating 1,000,000-record dataset...")

	buf := make([]byte, 0, 1024*1024)
	flushThreshold := 512 * 1024

	r := rand.New(rand.NewSource(42))
	totalRecords := 1_000_000

	for i := 0; i < totalRecords; i++ {
		key := []byte(fmt.Sprintf("user_%06d", r.Intn(400_000)))
		if r.Float32() < 0.10 {
			buf = EncodeRecord(buf, OpDelete, key, nil)
		} else {
			val := []byte(fmt.Sprintf("v_%06d", i))
			buf = EncodeRecord(buf, OpPut, key, val)
		}

		if len(buf) >= flushThreshold {
			if _, err := f.Write(buf); err != nil {
				t.Fatalf("Write failed: %v", err)
			}
			buf = buf[:0]
		}
	}

	// Append killed batch + torn tail
	buf = EncodeRecord(buf, OpBatchBegin, nil, nil)
	for i := 0; i < 5000; i++ {
		k := []byte(fmt.Sprintf("killed_batch_k_%d", i))
		v := []byte(fmt.Sprintf("killed_batch_v_%d", i))
		buf = EncodeRecord(buf, OpPut, k, v)
	}
	buf = append(buf, []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03}...)

	if len(buf) > 0 {
		if _, err := f.Write(buf); err != nil {
			t.Fatalf("Final write failed: %v", err)
		}
	}
	_ = f.Sync()
	_ = f.Close()

	info, _ := os.Stat(dbPath)
	t.Logf("Dataset ready: %.2f MB", float64(info.Size())/(1024*1024))

	start := time.Now()
	db, err := Open(dbPath, DefaultOptions())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	t.Logf("=== Phase 5 Performance Result ===")
	t.Logf("1,000,000 records recovery time: %v (Spec requirement: < 2.0s)", elapsed)
	t.Logf("Indexed live keys: %d", db.index.Len())
	t.Logf("Hardware: %s/%s, CPUs: %d", runtime.GOOS, runtime.GOARCH, runtime.NumCPU())

	if elapsed > 2*time.Second {
		t.Fatalf("Recovery took %v, which exceeds the 2.0s threshold", elapsed)
	}
}

