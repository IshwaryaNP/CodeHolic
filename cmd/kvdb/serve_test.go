package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"bitcask/src"
)

var (
	sharedKVDBBin     string
	sharedKVDBOnce    sync.Once
	sharedKVDBBuildErr error
)

func buildKVDBBinary(t *testing.T) string {
	t.Helper()
	sharedKVDBOnce.Do(func() {
		binDir, err := os.MkdirTemp("", "kvdb_shared_bin_*")
		if err != nil {
			sharedKVDBBuildErr = err
			return
		}
		binPath := filepath.Join(binDir, "kvdb_test.exe")
		cmd := exec.Command("go", "build", "-o", binPath, ".")
		if out, err := cmd.CombinedOutput(); err != nil {
			sharedKVDBBuildErr = fmt.Errorf("build kvdb failed: %v, out: %s", err, string(out))
			return
		}
		sharedKVDBBin = binPath
	})
	if sharedKVDBBuildErr != nil {
		t.Fatalf("buildKVDBBinary failed: %v", sharedKVDBBuildErr)
	}
	return sharedKVDBBin
}

// TestCLI_ServeProtocolAndAtomicity tests that:
// 1. serve responds to put, del, batch <n>, and compact.
// 2. ACK appears only after the data is visible via a second open.
// 3. Malformed lines produce "ERR <id> <reason>" and never crash.
// 4. Batch lines are read atomically before execution.
func TestCLI_ServeProtocolAndAtomicity(t *testing.T) {
	binPath := buildKVDBBinary(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "serve_test.db")

	cmd := exec.Command(binPath, "serve", dbPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd start: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	scanner := bufio.NewScanner(stdout)
	writer := bufio.NewWriter(stdin)

	send := func(line string) {
		_, _ = writer.WriteString(line + "\n")
		_ = writer.Flush()
	}

	readResp := func() string {
		if scanner.Scan() {
			return scanner.Text()
		}
		return ""
	}

	// 1. Put with hex encoding: key "k1" = 6b31, val "v1" = 7631
	k1Hex := hex.EncodeToString([]byte("k1"))
	v1Hex := hex.EncodeToString([]byte("v1"))
	send(fmt.Sprintf("op1 put %s %s", k1Hex, v1Hex))
	resp1 := readResp()
	if resp1 != "ACK op1" {
		t.Fatalf("Expected 'ACK op1', got %q", resp1)
	}

	// Verify durability via second open
	rdb, err := src.Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Second open failed: %v", err)
	}
	gotVal, err := rdb.Get([]byte("k1"))
	if err != nil || string(gotVal) != "v1" {
		t.Fatalf("Second open did not see durable k1: %v, %s", err, gotVal)
	}
	_ = rdb.Close()

	// 2. Batch <n>: op2 batch 3
	// put k2 v2
	// put k3 v3
	// del k1
	k2Hex := hex.EncodeToString([]byte("k2"))
	v2Hex := hex.EncodeToString([]byte("v2"))
	k3Hex := hex.EncodeToString([]byte("k3"))
	v3Hex := hex.EncodeToString([]byte("v3"))

	send("op2 batch 3")
	send(fmt.Sprintf("put %s %s", k2Hex, v2Hex))
	send(fmt.Sprintf("put %s %s", k3Hex, v3Hex))
	send(fmt.Sprintf("del %s", k1Hex))

	resp2 := readResp()
	if resp2 != "ACK op2" {
		t.Fatalf("Expected 'ACK op2', got %q", resp2)
	}

	// Second open confirms batch visibility
	rdb2, err := src.Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Second open 2 failed: %v", err)
	}
	if _, err := rdb2.Get([]byte("k1")); err != src.ErrNotFound {
		t.Fatalf("Expected k1 to be deleted in batch")
	}
	val2, err := rdb2.Get([]byte("k2"))
	if err != nil || string(val2) != "v2" {
		t.Fatalf("k2 mismatch: %v, %s", err, val2)
	}
	val3, err := rdb2.Get([]byte("k3"))
	if err != nil || string(val3) != "v3" {
		t.Fatalf("k3 mismatch: %v, %s", err, val3)
	}
	_ = rdb2.Close()

	// 3. Compact command
	send("op3 compact")
	resp3 := readResp()
	if resp3 != "ACK op3" {
		t.Fatalf("Expected 'ACK op3', got %q", resp3)
	}

	// 4. Malformed command tests
	send("op4 unknown_op arg1")
	resp4 := readResp()
	if !strings.HasPrefix(resp4, "ERR op4") {
		t.Fatalf("Expected ERR for unknown op, got %q", resp4)
	}

	send("op5 put incomplete")
	resp5 := readResp()
	if !strings.HasPrefix(resp5, "ERR op5") {
		t.Fatalf("Expected ERR for incomplete put, got %q", resp5)
	}

	send("op6 del")
	resp6 := readResp()
	if !strings.HasPrefix(resp6, "ERR op6") {
		t.Fatalf("Expected ERR for incomplete del, got %q", resp6)
	}

	send("opMissing")
	respMissing := readResp()
	if !strings.HasPrefix(respMissing, "ERR opMissing") {
		t.Fatalf("Expected ERR for line with missing op, got %q", respMissing)
	}

	// Empty line should be skipped cleanly
	send("")

	// Ensure server is still alive after malformed inputs
	send(fmt.Sprintf("op7 put %s %s", hex.EncodeToString([]byte("alive")), hex.EncodeToString([]byte("yes"))))
	resp7 := readResp()
	if resp7 != "ACK op7" {
		t.Fatalf("Server died or failed after malformed input: %q", resp7)
	}

	// 5. Large 1 MB value hex-encoded (2 MB + overhead) through serve
	oneMBVal := bytes.Repeat([]byte("X"), 1024*1024)
	hexVal := hex.EncodeToString(oneMBVal)
	send(fmt.Sprintf("opLarge put %s %s", hex.EncodeToString([]byte("large_k")), hexVal))
	respLarge := readResp()
	if respLarge != "ACK opLarge" {
		t.Fatalf("Expected 'ACK opLarge' for 1MB value, got %q", respLarge)
	}

	// Verify large value via independent reopen
	rdb3, err := src.Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Reopen for 1MB check failed: %v", err)
	}
	gotLarge, err := rdb3.Get([]byte("large_k"))
	if err != nil || len(gotLarge) != len(oneMBVal) || gotLarge[0] != 'X' {
		t.Fatalf("1MB value mismatch: len=%d, err=%v", len(gotLarge), err)
	}
	_ = rdb3.Close()
}

// TestCLI_ServeBatchEOFTruncation asserts that for a batch truncated by EOF (e.g. batch 5 with only 2 lines),
// NOTHING from it is applied to the database.
func TestCLI_ServeBatchEOFTruncation(t *testing.T) {
	binPath := buildKVDBBinary(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "serve_batch_eof.db")

	cmd := exec.Command(binPath, "serve", dbPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd start: %v", err)
	}

	scanner := bufio.NewScanner(stdout)
	writer := bufio.NewWriter(stdin)

	// Send an initial baseline key
	_, _ = writer.WriteString("opInit put " + hex.EncodeToString([]byte("baseline")) + " " + hex.EncodeToString([]byte("val0")) + "\n")
	_ = writer.Flush()
	if scanner.Scan() {
		if scanner.Text() != "ACK opInit" {
			t.Fatalf("Expected ACK opInit, got %s", scanner.Text())
		}
	}

	// Send a batch header declaring 5 ops, but supply only 2 lines then close stdin (EOF)
	_, _ = writer.WriteString("opTrunc batch 5\n")
	_, _ = writer.WriteString("put " + hex.EncodeToString([]byte("should_not_exist_1")) + " " + hex.EncodeToString([]byte("val1")) + "\n")
	_, _ = writer.WriteString("put " + hex.EncodeToString([]byte("should_not_exist_2")) + " " + hex.EncodeToString([]byte("val2")) + "\n")
	_ = writer.Flush()
	_ = stdin.Close() // EOF

	// Must produce an ERR on stdout
	gotErr := false
	for scanner.Scan() {
		txt := scanner.Text()
		if strings.HasPrefix(txt, "ERR opTrunc") {
			gotErr = true
		}
	}
	_ = cmd.Wait()

	if !gotErr {
		t.Fatalf("Expected ERR opTrunc on unexpected EOF")
	}

	// Verify nothing from the truncated batch was applied
	rdb, err := src.Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer rdb.Close()

	if rdb.Snapshot()["should_not_exist_1"] != 0 {
		t.Fatalf("should_not_exist_1 found in database!")
	}
	if rdb.Snapshot()["should_not_exist_2"] != 0 {
		t.Fatalf("should_not_exist_2 found in database!")
	}
	vBase, err := rdb.Get([]byte("baseline"))
	if err != nil || string(vBase) != "val0" {
		t.Fatalf("baseline key corrupt: %v, %s", err, vBase)
	}
}

// TestCLI_ServeConcurrentOps tests sending concurrent requests through serve stdin/stdout.
func TestCLI_ServeConcurrentOps(t *testing.T) {
	binPath := buildKVDBBinary(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "serve_concurrent.db")

	cmd := exec.Command(binPath, "serve", dbPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd start: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	scanner := bufio.NewScanner(stdout)
	writer := bufio.NewWriter(stdin)

	var writeMu sync.Mutex
	sendLine := func(l string) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_, _ = writer.WriteString(l + "\n")
		_ = writer.Flush()
	}

	totalOps := 50
	expectedACKs := make(map[string]bool)
	for i := 0; i < totalOps; i++ {
		opID := fmt.Sprintf("c_%03d", i)
		expectedACKs[opID] = true
	}

	// Concurrent dispatcher sending ops
	var wg sync.WaitGroup
	for i := 0; i < totalOps; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			opID := fmt.Sprintf("c_%03d", idx)
			k := hex.EncodeToString([]byte(fmt.Sprintf("ck_%03d", idx)))
			v := hex.EncodeToString([]byte(fmt.Sprintf("cv_%03d", idx)))
			sendLine(fmt.Sprintf("%s put %s %s", opID, k, v))
		}(i)
	}
	wg.Wait()

	// Read all ACKs
	receivedACKs := make(map[string]bool)
	for len(receivedACKs) < totalOps {
		if !scanner.Scan() {
			t.Fatalf("Scanner finished prematurely with %d of %d ACKs", len(receivedACKs), totalOps)
		}
		txt := scanner.Text()
		if strings.HasPrefix(txt, "ACK ") {
			receivedACKs[strings.TrimPrefix(txt, "ACK ")] = true
		} else {
			t.Fatalf("Unexpected line received: %s", txt)
		}
	}

	for opID := range expectedACKs {
		if !receivedACKs[opID] {
			t.Fatalf("Missing ACK for %s", opID)
		}
	}
}

// TestCLI_OutputFormats verifies all 8 commands: open, put, get, del, batch, dump, check, serve
func TestCLI_OutputFormats(t *testing.T) {
	binPath := buildKVDBBinary(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cli_formats.db")

	// 1. open: "Keys: %d, Recovery time: %v"
	outOpen, err := exec.Command(binPath, "open", dbPath).CombinedOutput()
	if err != nil {
		t.Fatalf("open failed: %v, out: %s", err, string(outOpen))
	}
	if !strings.Contains(string(outOpen), "Keys: 0, Recovery time:") {
		t.Fatalf("open format mismatch: %s", string(outOpen))
	}

	// 2. put
	outPut, err := exec.Command(binPath, "put", dbPath, "z_key", "z_val").CombinedOutput()
	if err != nil {
		t.Fatalf("put failed: %v, out: %s", err, string(outPut))
	}
	outPut2, err := exec.Command(binPath, "put", dbPath, "a_key", "a_val").CombinedOutput()
	if err != nil {
		t.Fatalf("put 2 failed: %v, out: %s", err, string(outPut2))
	}

	// 3. get: prints value or NOTFOUND
	outGet, err := exec.Command(binPath, "get", dbPath, "z_key").CombinedOutput()
	if err != nil || strings.TrimSpace(string(outGet)) != "z_val" {
		t.Fatalf("get failed: %v, out: %s", err, string(outGet))
	}
	outGetNF, err := exec.Command(binPath, "get", dbPath, "nonexistent").CombinedOutput()
	if err != nil || strings.TrimSpace(string(outGetNF)) != "NOTFOUND" {
		t.Fatalf("get NOTFOUND mismatch: %v, out: %s", err, string(outGetNF))
	}

	// 4. del
	outDel, err := exec.Command(binPath, "del", dbPath, "z_key").CombinedOutput()
	if err != nil {
		t.Fatalf("del failed: %v, out: %s", err, string(outDel))
	}
	outGetDel, err := exec.Command(binPath, "get", dbPath, "z_key").CombinedOutput()
	if err != nil || strings.TrimSpace(string(outGetDel)) != "NOTFOUND" {
		t.Fatalf("get after del mismatch: %v, out: %s", err, string(outGetDel))
	}

	// 5. batch <path> <file>
	batchFile := filepath.Join(dir, "b.txt")
	_ = os.WriteFile(batchFile, []byte("put b_k b_v\nput c_k c_v\n"), 0666)
	outBatch, err := exec.Command(binPath, "batch", dbPath, batchFile).CombinedOutput()
	if err != nil {
		t.Fatalf("batch failed: %v, out: %s", err, string(outBatch))
	}

	// 6. dump: sorted by key, "<key>=<value>"
	outDump, err := exec.Command(binPath, "dump", dbPath).CombinedOutput()
	if err != nil {
		t.Fatalf("dump failed: %v, out: %s", err, string(outDump))
	}
	lines := strings.Split(strings.TrimSpace(string(outDump)), "\n")
	expectedLines := []string{"a_key=a_val", "b_k=b_v", "c_k=c_v"}
	if len(lines) != len(expectedLines) {
		t.Fatalf("dump line count mismatch: got %d, expected %d", len(lines), len(expectedLines))
	}
	for i := range lines {
		if strings.TrimSpace(lines[i]) != expectedLines[i] {
			t.Fatalf("dump line %d mismatch: got %s, expected %s", i, lines[i], expectedLines[i])
		}
	}

	// 7. check: prints OK or reason
	outCheck, err := exec.Command(binPath, "check", dbPath).CombinedOutput()
	if err != nil || strings.TrimSpace(string(outCheck)) != "OK" {
		t.Fatalf("check failed: %v, out: %s", err, string(outCheck))
	}
}

// TestCLI_ServeEmptyValue_BadHexBatch_1MBRoundTrip tests the Phase 3 user requests:
// 1. Empty values: "-" token means empty value in put and batch; round-trips through serve and reopen.
// 2. Batch with bad hex applies nothing.
// 3. 1 MB value (2 MB hex line) round-trips.
func TestCLI_ServeEmptyValue_BadHexBatch_1MBRoundTrip(t *testing.T) {
	binPath := buildKVDBBinary(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "serve_protocol_test.db")

	cmd := exec.Command(binPath, "serve", dbPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd start: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	writer := bufio.NewWriter(stdin)

	send := func(line string) {
		_, _ = writer.WriteString(line + "\n")
		_ = writer.Flush()
	}

	recv := func() string {
		if scanner.Scan() {
			return scanner.Text()
		}
		return ""
	}

	// 1. Test empty value via "-"
	send("op_empty put 656d7074795f6b6579 -")
	resp := recv()
	if resp != "ACK op_empty" {
		t.Fatalf("Expected 'ACK op_empty', got '%s'", resp)
	}

	// 2. Test batch with "-" empty value and test batch with bad hex applying nothing
	// First: batch with empty value
	send("op_b1 batch 1")
	send("put 625f656d707479 -")
	resp = recv()
	if resp != "ACK op_b1" {
		t.Fatalf("Expected 'ACK op_b1', got '%s'", resp)
	}

	// Next: batch with bad hex token. It must return ERR and apply nothing.
	send("op_bad_b batch 2")
	send("put 676f6f645f6b 6162")
	send("put 6261645f6b !!!not_hex!!!")
	resp = recv()
	if !strings.HasPrefix(resp, "ERR op_bad_b") {
		t.Fatalf("Expected 'ERR op_bad_b', got '%s'", resp)
	}

	// 3. Test 1 MB value (2 MB hex line)
	bigBytes := bytes.Repeat([]byte{0x42}, 1024*1024) // 1 MB
	bigHex := hex.EncodeToString(bigBytes)             // 2 MB hex string
	send(fmt.Sprintf("op_1mb put 6b6579316d62 %s", bigHex))
	resp = recv()
	if resp != "ACK op_1mb" {
		t.Fatalf("Expected 'ACK op_1mb', got '%s'", resp)
	}

	// Close serve and reopen directly to verify durability and content
	_ = stdin.Close()
	_ = cmd.Wait()

	db, err := src.Open(dbPath, src.DefaultOptions())
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	defer db.Close()

	// Verify empty value
	vEmpty, err := db.Get([]byte("empty_key"))
	if err != nil || len(vEmpty) != 0 {
		t.Fatalf("empty_key value mismatch: err=%v, len=%d", err, len(vEmpty))
	}
	vBEmpty, err := db.Get([]byte("b_empty"))
	if err != nil || len(vBEmpty) != 0 {
		t.Fatalf("b_empty value mismatch: err=%v, len=%d", err, len(vBEmpty))
	}

	// Verify bad hex batch applied NOTHING (good_k must not exist)
	_, err = db.Get([]byte("good_k"))
	if !errors.Is(err, src.ErrNotFound) {
		t.Fatalf("good_k should be NOTFOUND because batch failed, got: %v", err)
	}

	// Verify 1 MB value
	v1mb, err := db.Get([]byte("key1mb"))
	if err != nil || len(v1mb) != 1024*1024 || v1mb[0] != 0x42 {
		t.Fatalf("1MB value mismatch: err=%v, len=%d", err, len(v1mb))
	}
}
