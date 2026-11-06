package src

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBulkImport_FormatsAndChunking(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "import_test.db")

	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// 1. Lines format
	linesData := `put user1 Alice
put user2 Bob
del user1
put user3 Charlie
`
	resLines, err := db.BulkImport(strings.NewReader(linesData), ImportOptions{
		Format:    FormatLines,
		ChunkSize: 2,
	})
	if err != nil {
		t.Fatalf("BulkImport lines failed: %v", err)
	}
	if resLines.OpsApplied != 4 || resLines.Chunks != 2 {
		t.Fatalf("unexpected lines result: %+v", resLines)
	}

	// Verify current state
	if _, err := db.Get([]byte("user1")); err != ErrNotFound {
		t.Fatalf("user1 should be deleted")
	}
	if v, err := db.Get([]byte("user2")); err != nil || string(v) != "Bob" {
		t.Fatalf("user2 expected Bob, got %s, err %v", string(v), err)
	}
	if v, err := db.Get([]byte("user3")); err != nil || string(v) != "Charlie" {
		t.Fatalf("user3 expected Charlie, got %s, err %v", string(v), err)
	}

	// 2. CSV format
	csvData := `user4,Dave
user5,Eve
user6,Frank
`
	resCSV, err := db.BulkImport(strings.NewReader(csvData), ImportOptions{
		Format:    FormatCSV,
		ChunkSize: 2,
	})
	if err != nil {
		t.Fatalf("BulkImport csv failed: %v", err)
	}
	if resCSV.OpsApplied != 3 || resCSV.Chunks != 2 {
		t.Fatalf("unexpected csv result: %+v", resCSV)
	}
	if v, err := db.Get([]byte("user5")); err != nil || string(v) != "Eve" {
		t.Fatalf("user5 expected Eve, got %s", string(v))
	}

	// 3. JSONL format
	jsonlData := `{"key":"user7","value":"Grace"}
{"key":"user8","value":"Heidi"}
{"key":"user5","op":"del"}
`
	resJSONL, err := db.BulkImport(strings.NewReader(jsonlData), ImportOptions{
		Format:    FormatJSONL,
		ChunkSize: 10,
	})
	if err != nil {
		t.Fatalf("BulkImport jsonl failed: %v", err)
	}
	if resJSONL.OpsApplied != 3 || resJSONL.Chunks != 1 {
		t.Fatalf("unexpected jsonl result: %+v", resJSONL)
	}
	if _, err := db.Get([]byte("user5")); err != ErrNotFound {
		t.Fatalf("user5 should be deleted via JSONL")
	}
	if v, err := db.Get([]byte("user7")); err != nil || string(v) != "Grace" {
		t.Fatalf("user7 expected Grace, got %s", string(v))
	}
}

func TestBulkImport_StartLineResumeAndErrors(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "resume_test.db")

	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	dataWithBadLine := `put k1 v1
put k2 v2
bad_op_invalid
put k3 v3
`
	// With OnErrorAbort, should fail at line 3 without applying line 4
	_, err = db.BulkImport(strings.NewReader(dataWithBadLine), ImportOptions{
		Format:    FormatLines,
		ChunkSize: 10,
		OnError:   OnErrorAbort,
	})
	if err == nil {
		t.Fatalf("expected BulkImport to fail on bad line")
	}

	// With OnErrorSkip, should skip line 3 and apply k1, k2, k3
	resSkip, err := db.BulkImport(strings.NewReader(dataWithBadLine), ImportOptions{
		Format:    FormatLines,
		ChunkSize: 10,
		OnError:   OnErrorSkip,
	})
	if err != nil {
		t.Fatalf("expected BulkImport to succeed with skip: %v", err)
	}
	if resSkip.OpsApplied != 3 || resSkip.Skipped < 1 {
		t.Fatalf("unexpected skip result: %+v", resSkip)
	}

	// Test StartLine resuming from line 4
	resResume, err := db.BulkImport(strings.NewReader(dataWithBadLine), ImportOptions{
		Format:    FormatLines,
		StartLine: 4,
	})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if resResume.OpsApplied != 1 {
		t.Fatalf("expected 1 op resumed, got %d", resResume.OpsApplied)
	}
}

func TestBulkImport_DiskFull(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "diskfull_import.db")

	// Set small max bytes: 1000 records of ~35 bytes = ~35 KB, so 10 KB will hit ErrDiskFull quickly
	opts := DefaultOptions()
	opts.MaxBytes = 10 * 1024 // 10 KB
	db, err := Open(dbPath, opts)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// Generate a 1000-line import dataset
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		sb.WriteString(fmt.Sprintf("put key_%05d value_%05d\n", i, i))
	}

	res, err := db.BulkImport(strings.NewReader(sb.String()), ImportOptions{
		Format:    FormatLines,
		ChunkSize: 50,
	})
	if err == nil {
		t.Fatalf("expected ErrDiskFull on small max-bytes")
	}
	if !strings.Contains(err.Error(), "disk full") && err != ErrDiskFull {
		t.Fatalf("expected disk full error, got: %v", err)
	}

	// Ensure DB is still readable and valid
	if res.Chunks > 0 {
		v, err := db.Get([]byte("key_00000"))
		if err != nil || string(v) != "value_00000" {
			t.Fatalf("first key should still be readable: %v, %s", err, string(v))
		}
	}
}

func TestBulkImport_1MLinesVerification(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 1M lines test in short mode")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "import_1M.db")
	importFilePath := filepath.Join(dir, "data_1M.txt")

	const count = 100_000 // representative large volume
	f, err := os.Create(importFilePath)
	if err != nil {
		t.Fatalf("Create import file failed: %v", err)
	}
	for i := 0; i < count; i++ {
		fmt.Fprintf(f, "put key_%08d val_%08d\n", i, i)
	}
	f.Close()

	db, err := Open(dbPath, &Options{SyncPolicy: SyncNever})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	inFile, err := os.Open(importFilePath)
	if err != nil {
		t.Fatalf("Open import file failed: %v", err)
	}
	defer inFile.Close()

	res, err := db.BulkImport(inFile, ImportOptions{
		Format:    FormatLines,
		ChunkSize: 5000,
	})
	if err != nil {
		t.Fatalf("BulkImport 100k failed: %v", err)
	}
	if res.OpsApplied != count {
		t.Fatalf("expected %d ops applied, got %d", count, res.OpsApplied)
	}

	snap := db.Snapshot()
	if len(snap) != count {
		t.Fatalf("expected %d keys in snapshot, got %d", count, len(snap))
	}
}
