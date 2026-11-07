package src

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindows_OpenFile_Truncate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trunc_test.db")

	// Open with os.O_RDWR|os.O_CREATE (no O_APPEND), exactly as db.go now does
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	defer f.Close()

	data := []byte("hello world 1234567890")
	if _, err := f.WriteAt(data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// Call Truncate on real *os.File
	truncErr := f.Truncate(5)
	if truncErr != nil {
		t.Fatalf("Truncate on os.O_RDWR failed on Windows: %v", truncErr)
	}

	info, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Size() != 5 {
		t.Fatalf("Expected size 5 after Truncate, got %d", info.Size())
	}
	t.Logf("SUCCESS: os.OpenFile(O_RDWR).Truncate() succeeds on this Windows machine! size=%d", info.Size())
}
