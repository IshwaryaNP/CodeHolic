package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"bitcask/src"
)

func newTestServer(t *testing.T) (*server, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	srv, err := newServer(dbPath)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return srv, func() { srv.db.Close() }
}

func doRequest(t *testing.T, srv *server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("json marshal: %v", err)
		}
		reqBody = bytes.NewReader(b)
	} else {
		reqBody = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/put", srv.handlePut)
	mux.HandleFunc("/api/get", srv.handleGet)
	mux.HandleFunc("/api/delete", srv.handleDelete)
	mux.HandleFunc("/api/batch", srv.handleBatch)
	mux.HandleFunc("/api/state", srv.handleState)
	mux.HandleFunc("/api/records", srv.handleRecords)
	mux.HandleFunc("/api/compact", srv.handleCompact)
	mux.HandleFunc("/api/restart", srv.handleRestart)
	mux.HandleFunc("/api/crash/torn", srv.handleCrashTorn)
	mux.HandleFunc("/api/crash/uncommitted-batch", srv.handleCrashUncommittedBatch)
	mux.ServeHTTP(rr, req)
	return rr
}

func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&m); err != nil {
		t.Fatalf("decode JSON: %v (body: %s)", err, rr.Body.String())
	}
	return m
}

func TestPutGetDelete(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	// PUT
	rr := doRequest(t, srv, "POST", "/api/put", map[string]string{"key": "k1", "value": "hello"})
	if rr.Code != 200 {
		t.Fatalf("put: got %d, body: %s", rr.Code, rr.Body)
	}

	// GET
	req := httptest.NewRequest("GET", "/api/get?key=k1", nil)
	rr2 := httptest.NewRecorder()
	srv.handleGet(rr2, req)
	if rr2.Code != 200 {
		t.Fatalf("get: got %d", rr2.Code)
	}
	m := decodeJSON(t, rr2)
	if m["value"] != "hello" {
		t.Fatalf("get value: got %v", m["value"])
	}

	// DELETE
	rr3 := doRequest(t, srv, "POST", "/api/delete", map[string]string{"key": "k1"})
	if rr3.Code != 200 {
		t.Fatalf("delete: got %d", rr3.Code)
	}

	// GET after delete -> 404
	req4 := httptest.NewRequest("GET", "/api/get?key=k1", nil)
	rr4 := httptest.NewRecorder()
	srv.handleGet(rr4, req4)
	if rr4.Code != 404 {
		t.Fatalf("get after delete: expected 404, got %d", rr4.Code)
	}
}

func TestBatch(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	rr := doRequest(t, srv, "POST", "/api/batch", map[string]any{
		"ops": []map[string]string{
			{"op": "put", "key": "bk1", "value": "bv1"},
			{"op": "put", "key": "bk2", "value": "bv2"},
		},
	})
	if rr.Code != 200 {
		t.Fatalf("batch: got %d, body: %s", rr.Code, rr.Body)
	}

	for _, kv := range [][2]string{{"bk1", "bv1"}, {"bk2", "bv2"}} {
		req := httptest.NewRequest("GET", "/api/get?key="+kv[0], nil)
		rr2 := httptest.NewRecorder()
		srv.handleGet(rr2, req)
		if rr2.Code != 200 {
			t.Fatalf("get %s: got %d", kv[0], rr2.Code)
		}
		m := decodeJSON(t, rr2)
		if m["value"] != kv[1] {
			t.Fatalf("batch get %s: got %v", kv[0], m["value"])
		}
	}
}

func TestStateDeadBytes(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	// First put: HeaderSize (15) + len("k") (1) + len("first") (5) = 21 bytes
	doRequest(t, srv, "POST", "/api/put", map[string]string{"key": "k", "value": "first"})
	firstSize := int64(src.HeaderSize + 1 + 5)

	// Overwrite: makes the first record dead
	doRequest(t, srv, "POST", "/api/put", map[string]string{"key": "k", "value": "second"})

	// State
	req := httptest.NewRequest("GET", "/api/state", nil)
	rr := httptest.NewRecorder()
	srv.handleState(rr, req)
	if rr.Code != 200 {
		t.Fatalf("state: got %d", rr.Code)
	}
	m := decodeJSON(t, rr)
	deadBytes := int64(m["deadBytes"].(float64))
	if deadBytes != firstSize {
		t.Fatalf("deadBytes: want %d, got %d", firstSize, deadBytes)
	}
}

func TestRecordsLiveDead(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	doRequest(t, srv, "POST", "/api/put", map[string]string{"key": "k", "value": "v1"})
	doRequest(t, srv, "POST", "/api/put", map[string]string{"key": "k", "value": "v2"})

	req := httptest.NewRequest("GET", "/api/records", nil)
	rr := httptest.NewRecorder()
	srv.handleRecords(rr, req)
	if rr.Code != 200 {
		t.Fatalf("records: got %d", rr.Code)
	}

	var resp recordsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Records) != 2 {
		t.Fatalf("want 2 records, got %d", len(resp.Records))
	}
	if resp.Records[0].Live {
		t.Fatalf("first record should be dead")
	}
	if !resp.Records[1].Live {
		t.Fatalf("second record should be live")
	}
}

func TestTornWriteSimulation(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	doRequest(t, srv, "POST", "/api/put", map[string]string{"key": "k", "value": "v"})

	const garbageN = 20
	rr := doRequest(t, srv, "POST", "/api/crash/torn", map[string]int{"bytes": garbageN})
	if rr.Code != 200 {
		t.Fatalf("torn: got %d, body: %s", rr.Code, rr.Body)
	}

	m := decodeJSON(t, rr)
	truncated := int64(m["truncatedBytes"].(float64))
	if truncated != garbageN {
		t.Fatalf("truncatedBytes=%d, want %d", truncated, garbageN)
	}
}

func TestUncommittedBatchSimulation(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	doRequest(t, srv, "POST", "/api/put", map[string]string{"key": "existing", "value": "yes"})

	rr := doRequest(t, srv, "POST", "/api/crash/uncommitted-batch", nil)
	if rr.Code != 200 {
		t.Fatalf("uncommitted batch: got %d, body: %s", rr.Code, rr.Body)
	}

	// crash1, crash2, crash3 must not exist
	for _, k := range []string{"crash1", "crash2", "crash3"} {
		req := httptest.NewRequest("GET", "/api/get?key="+k, nil)
		rr2 := httptest.NewRecorder()
		srv.handleGet(rr2, req)
		if rr2.Code != 404 {
			t.Fatalf("key %s should not exist after uncommitted batch, got %d", k, rr2.Code)
		}
	}

	// existing key must still be present
	req := httptest.NewRequest("GET", "/api/get?key=existing", nil)
	rr3 := httptest.NewRecorder()
	srv.handleGet(rr3, req)
	if rr3.Code != 200 {
		t.Fatalf("existing key gone: got %d", rr3.Code)
	}
}

func TestRestartPreservesData(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	keys := []string{"alpha", "beta", "gamma"}
	for _, k := range keys {
		doRequest(t, srv, "POST", "/api/put", map[string]string{"key": k, "value": k + "-val"})
	}

	rr := doRequest(t, srv, "POST", "/api/restart", nil)
	if rr.Code != 200 {
		t.Fatalf("restart: got %d, body: %s", rr.Code, rr.Body)
	}

	m := decodeJSON(t, rr)
	keysAfter := int(m["keysAfter"].(float64))
	if keysAfter != len(keys) {
		t.Fatalf("keysAfter: want %d, got %d", len(keys), keysAfter)
	}

	for _, k := range keys {
		req := httptest.NewRequest("GET", "/api/get?key="+k, nil)
		rr2 := httptest.NewRecorder()
		srv.handleGet(rr2, req)
		if rr2.Code != 200 {
			t.Fatalf("key %s missing after restart: got %d", k, rr2.Code)
		}
		mv := decodeJSON(t, rr2)
		if mv["value"] != k+"-val" {
			t.Fatalf("key %s wrong value: %v", k, mv["value"])
		}
	}
}

func TestInputValidation(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	// Empty key
	rr := doRequest(t, srv, "POST", "/api/put", map[string]string{"key": "", "value": "v"})
	if rr.Code != 400 {
		t.Fatalf("empty key: want 400, got %d", rr.Code)
	}

	// Missing key in delete
	rr2 := doRequest(t, srv, "POST", "/api/delete", map[string]string{"key": ""})
	if rr2.Code != 400 {
		t.Fatalf("empty key delete: want 400, got %d", rr2.Code)
	}

	// Unknown batch op
	rr3 := doRequest(t, srv, "POST", "/api/batch", map[string]any{
		"ops": []map[string]string{{"op": "invalid", "key": "k"}},
	})
	if rr3.Code != 400 {
		t.Fatalf("invalid batch op: want 400, got %d", rr3.Code)
	}
}

