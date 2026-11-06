package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
	_ "embed"

	"bitcask/src"
)

//go:embed index.html
var indexHTML []byte

type server struct {
	mu     sync.Mutex
	db     *src.DB
	dbPath string
}

func newServer(dbPath string) (*server, error) {
	db, err := src.Open(dbPath, src.DefaultOptions())
	if err != nil {
		return nil, err
	}
	return &server{db: db, dbPath: dbPath}, nil
}

func (s *server) withDB(fn func(*src.DB) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(s.db)
}

func (s *server) reopenDB() (fileSizeBefore, fileSizeAfter int64, dur time.Duration, err error) {
	fileSizeBefore = s.db.FileSize()
	if cerr := s.db.Close(); cerr != nil && !errors.Is(cerr, src.ErrClosed) {
		err = fmt.Errorf("close failed: %w", cerr)
		return
	}
	t0 := time.Now()
	s.db, err = src.Open(s.dbPath, src.DefaultOptions())
	dur = time.Since(t0)
	if err != nil {
		return
	}
	fileSizeAfter = s.db.FileSize()
	return
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *server) handlePut(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, 400, "bad request: "+err.Error())
		return
	}
	if req.Key == "" {
		errJSON(w, 400, "key is required")
		return
	}
	if len(req.Key) > src.MaxKeySize {
		errJSON(w, 400, "key too large")
		return
	}
	if len(req.Value) > src.MaxValueSize {
		errJSON(w, 400, "value too large")
		return
	}

	var putErr error
	s.withDB(func(db *src.DB) error {
		putErr = db.Put([]byte(req.Key), []byte(req.Value))
		return nil
	})
	if putErr != nil {
		errJSON(w, 500, putErr.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "true"})
}

func (s *server) handleGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		errJSON(w, 400, "key is required")
		return
	}
	var val []byte
	var getErr error
	s.withDB(func(db *src.DB) error {
		val, getErr = db.Get([]byte(key))
		return nil
	})
	if errors.Is(getErr, src.ErrNotFound) {
		errJSON(w, 404, "not found")
		return
	}
	if getErr != nil {
		errJSON(w, 500, getErr.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"value": string(val)})
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, 400, "bad request: "+err.Error())
		return
	}
	if req.Key == "" {
		errJSON(w, 400, "key is required")
		return
	}
	var delErr error
	s.withDB(func(db *src.DB) error {
		delErr = db.Delete([]byte(req.Key))
		return nil
	})
	if delErr != nil {
		errJSON(w, 500, delErr.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "true"})
}

type batchOpReq struct {
	Op    string `json:"op"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (s *server) handleBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Ops []batchOpReq `json:"ops"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, 400, "bad request: "+err.Error())
		return
	}
	if len(req.Ops) == 0 {
		errJSON(w, 400, "no ops")
		return
	}

	b := src.NewBatch()
	for _, op := range req.Ops {
		if op.Key == "" {
			errJSON(w, 400, "each op needs a key")
			return
		}
		switch op.Op {
		case "put":
			if err := b.Put([]byte(op.Key), []byte(op.Value)); err != nil {
				errJSON(w, 400, "batch.Put: "+err.Error())
				return
			}
		case "delete":
			if err := b.Delete([]byte(op.Key)); err != nil {
				errJSON(w, 400, "batch.Delete: "+err.Error())
				return
			}
		default:
			errJSON(w, 400, "unknown op: "+op.Op)
			return
		}
	}
	var writeErr error
	s.withDB(func(db *src.DB) error {
		writeErr = db.Write(b)
		return nil
	})
	if writeErr != nil {
		errJSON(w, 500, writeErr.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "true"})
}

type indexEntry struct {
	Key    string `json:"key"`
	Offset int64  `json:"offset"`
}

type stateResponse struct {
	FileSize    int64        `json:"fileSize"`
	LiveKeys    int          `json:"liveKeys"`
	Index       []indexEntry `json:"index"`
	DeadBytes   int64        `json:"deadBytes"`
	DeadPercent float64      `json:"deadPercent"`
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()

	snap := db.Snapshot()
	fileSize := db.FileSize()

	var liveBytes int64
	if len(snap) > 0 {
		f, err := os.Open(db.DBPath())
		if err == nil {
			var hdr [src.HeaderSize]byte
			for _, off := range snap {
				if _, err2 := f.ReadAt(hdr[:], off); err2 == nil {
					h, err3 := src.DecodeHeader(hdr[:])
					if err3 == nil {
						liveBytes += int64(h.TotalLen)
					}
				}
			}
			_ = f.Close()
		}
	}

	deadBytes := fileSize - liveBytes
	if deadBytes < 0 {
		deadBytes = 0
	}
	var deadPct float64
	if fileSize > 0 {
		deadPct = float64(deadBytes) / float64(fileSize) * 100.0
	}

	entries := make([]indexEntry, 0, len(snap))
	for k, off := range snap {
		entries = append(entries, indexEntry{Key: k, Offset: off})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Offset < entries[j].Offset })

	writeJSON(w, 200, stateResponse{
		FileSize:    fileSize,
		LiveKeys:    len(snap),
		Index:       entries,
		DeadBytes:   deadBytes,
		DeadPercent: deadPct,
	})
}

type recordInfo struct {
	Offset       int64  `json:"offset"`
	Length       int64  `json:"length"`
	Op           byte   `json:"op"`
	Key          string `json:"key"`
	ValueSize    int64  `json:"valueSize"`
	ValuePreview string `json:"valuePreview"`
	CRCOk        bool   `json:"crcOk"`
	Live         bool   `json:"live"`
}

type recordsResponse struct {
	Records          []recordInfo `json:"records"`
	TailGarbageBytes int64        `json:"tailGarbageBytes"`
	TailOffset       int64        `json:"tailOffset"`
}

const maxRecords = 5000

func (s *server) handleRecords(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()

	snap := db.Snapshot()
	dbPath := db.DBPath()

	f, err := os.Open(dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, 200, recordsResponse{Records: []recordInfo{}})
			return
		}
		errJSON(w, 500, "open file: "+err.Error())
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		errJSON(w, 500, "stat: "+err.Error())
		return
	}
	fileSize := fi.Size()

	liveOffsets := make(map[int64]bool, len(snap))
	for _, off := range snap {
		liveOffsets[off] = true
	}

	reader := bufio.NewReaderSize(f, 1024*1024)
	records := make([]recordInfo, 0)
	var offset int64

	for offset < fileSize && len(records) < maxRecords {
		recordStart := offset

		if offset+int64(src.HeaderSize) > fileSize {
			break
		}

		var hdrBuf [src.HeaderSize]byte
		if _, err := io.ReadFull(reader, hdrBuf[:]); err != nil {
			break
		}

		hdr, err := src.DecodeHeader(hdrBuf[:])
		if err != nil {
			break
		}

		if offset+int64(hdr.TotalLen) > fileSize {
			break
		}

		keyValLen := int(hdr.TotalLen) - src.HeaderSize
		payload := make([]byte, keyValLen)
		if _, err := io.ReadFull(reader, payload); err != nil {
			break
		}

		keyBytes := payload[:hdr.KeyLen]
		valBytes := payload[hdr.KeyLen:]

		crcOk := src.VerifyCRC(hdrBuf[:], keyBytes, valBytes)
		if !crcOk {
			break
		}

		preview := ""
		if hdr.OpType == src.OpPut && len(valBytes) > 0 {
			n := 40
			if len(valBytes) < n {
				n = len(valBytes)
			}
			preview = string(valBytes[:n])
		}

		isLive := liveOffsets[recordStart]

		records = append(records, recordInfo{
			Offset:       recordStart,
			Length:       int64(hdr.TotalLen),
			Op:           hdr.OpType,
			Key:          string(keyBytes),
			ValueSize:    int64(hdr.ValLen),
			ValuePreview: preview,
			CRCOk:        crcOk,
			Live:         isLive,
		})

		offset += int64(hdr.TotalLen)
	}

	var tailGarbage int64
	var tailOffset int64
	if offset < fileSize {
		tailGarbage = fileSize - offset
		tailOffset = offset
	}

	writeJSON(w, 200, recordsResponse{
		Records:          records,
		TailGarbageBytes: tailGarbage,
		TailOffset:       tailOffset,
	})
}

type restartResponse struct {
	FileSizeBefore int64   `json:"fileSizeBefore"`
	FileSizeAfter  int64   `json:"fileSizeAfter"`
	TruncatedBytes int64   `json:"truncatedBytes"`
	KeysAfter      int     `json:"keysAfter"`
	DurationMs     float64 `json:"durationMs"`
}

func (s *server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	before, after, dur, err := s.reopenDB()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}

	writeJSON(w, 200, restartResponse{
		FileSizeBefore: before,
		FileSizeAfter:  after,
		TruncatedBytes: before - after,
		KeysAfter:      len(s.db.Snapshot()),
		DurationMs:     float64(dur.Microseconds()) / 1000.0,
	})
}

func (s *server) handleCompact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.mu.Lock()
	db := s.db
	before := db.FileSize()
	snap := db.Snapshot()
	dbPath := db.DBPath()
	s.mu.Unlock()

	// Count dead records
	countDead := func() int {
		liveOffsets := make(map[int64]bool, len(snap))
		for _, off := range snap {
			liveOffsets[off] = true
		}
		f, err := os.Open(dbPath)
		if err != nil {
			return 0
		}
		defer f.Close()
		reader := bufio.NewReaderSize(f, 512*1024)
		fi, err := f.Stat()
		if err != nil {
			return 0
		}
		fileSize := fi.Size()
		dead := 0
		var offset int64
		var hdr [src.HeaderSize]byte
		for offset < fileSize {
			if _, err := io.ReadFull(reader, hdr[:]); err != nil {
				break
			}
			h, err := src.DecodeHeader(hdr[:])
			if err != nil {
				break
			}
			kv := int(h.TotalLen) - src.HeaderSize
			payload := make([]byte, kv)
			if _, err := io.ReadFull(reader, payload); err != nil {
				break
			}
			if !src.VerifyCRC(hdr[:], payload[:h.KeyLen], payload[h.KeyLen:]) {
				break
			}
			if h.OpType == src.OpPut && !liveOffsets[offset] {
				dead++
			}
			offset += int64(h.TotalLen)
		}
		return dead
	}
	deadBefore := countDead()

	var compactErr error
	s.withDB(func(d *src.DB) error {
		compactErr = d.Compact()
		return nil
	})
	if compactErr != nil {
		errJSON(w, 500, compactErr.Error())
		return
	}

	s.mu.Lock()
	after := s.db.FileSize()
	s.mu.Unlock()

	writeJSON(w, 200, map[string]any{
		"fileSizeBefore":     before,
		"fileSizeAfter":      after,
		"deadRecordsRemoved": deadBefore,
	})
}

func (s *server) handleCrashTorn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Bytes int `json:"bytes"`
	}
	req.Bytes = 20
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Bytes < 1 {
		req.Bytes = 20
	}
	if req.Bytes > 65536 {
		req.Bytes = 65536
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	before := s.db.FileSize()

	if err := s.db.Close(); err != nil && !errors.Is(err, src.ErrClosed) {
		errJSON(w, 500, "close: "+err.Error())
		return
	}

	f, err := os.OpenFile(s.dbPath, os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		errJSON(w, 500, "open for append: "+err.Error())
		return
	}
	garbage := make([]byte, req.Bytes)
	for i := range garbage {
		garbage[i] = 0xFF
	}
	_, _ = f.Write(garbage)
	_ = f.Close()

	t0 := time.Now()
	s.db, err = src.Open(s.dbPath, src.DefaultOptions())
	dur := time.Since(t0)
	if err != nil {
		errJSON(w, 500, "reopen: "+err.Error())
		return
	}

	after := s.db.FileSize()
	writeJSON(w, 200, restartResponse{
		FileSizeBefore: before + int64(req.Bytes),
		FileSizeAfter:  after,
		TruncatedBytes: (before + int64(req.Bytes)) - after,
		KeysAfter:      len(s.db.Snapshot()),
		DurationMs:     float64(dur.Microseconds()) / 1000.0,
	})
}

func (s *server) handleCrashUncommittedBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	before := s.db.FileSize()

	if err := s.db.Close(); err != nil && !errors.Is(err, src.ErrClosed) {
		errJSON(w, 500, "close: "+err.Error())
		return
	}

	f, err := os.OpenFile(s.dbPath, os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		errJSON(w, 500, "open for append: "+err.Error())
		return
	}

	var buf []byte
	buf = src.EncodeRecord(buf, src.OpBatchBegin, nil, nil)
	buf = src.EncodeRecord(buf, src.OpPut, []byte("crash1"), []byte("v1"))
	buf = src.EncodeRecord(buf, src.OpPut, []byte("crash2"), []byte("v2"))
	buf = src.EncodeRecord(buf, src.OpPut, []byte("crash3"), []byte("v3"))
	_, _ = f.Write(buf)
	_ = f.Close()

	t0 := time.Now()
	s.db, err = src.Open(s.dbPath, src.DefaultOptions())
	dur := time.Since(t0)
	if err != nil {
		errJSON(w, 500, "reopen: "+err.Error())
		return
	}

	after := s.db.FileSize()
	writeJSON(w, 200, restartResponse{
		FileSizeBefore: before + int64(len(buf)),
		FileSizeAfter:  after,
		TruncatedBytes: (before + int64(len(buf))) - after,
		KeysAfter:      len(s.db.Snapshot()),
		DurationMs:     float64(dur.Microseconds()) / 1000.0,
	})
}

func (s *server) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Sane upload limit: 128MB
	const maxUploadSize = 128 * 1024 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	// Stream multipart form without loading entire file into memory
	mr, err := r.MultipartReader()
	if err != nil {
		errJSON(w, 400, "expected multipart form: "+err.Error())
		return
	}

	format := src.FormatLines
	chunkSize := 5000
	onError := src.OnErrorAbort

	var filePart io.Reader

	for {
		part, err := mr.NextPart()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			errJSON(w, 400, "error reading form part: "+err.Error())
			return
		}

		formName := part.FormName()
		if formName == "format" {
			data, _ := io.ReadAll(part)
			format = src.ImportFormat(string(data))
		} else if formName == "chunk" {
			data, _ := io.ReadAll(part)
			var n int
			_, _ = fmt.Sscanf(string(data), "%d", &n)
			if n > 0 {
				chunkSize = n
			}
		} else if formName == "on_error" {
			data, _ := io.ReadAll(part)
			onError = src.OnErrorAction(string(data))
		} else if formName == "file" {
			filePart = part
			break
		}
	}

	if filePart == nil {
		errJSON(w, 400, "missing file part in multipart form")
		return
	}

	// Prepare SSE response for streaming progress
	flusher, ok := w.(http.Flusher)
	if !ok {
		errJSON(w, 500, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	s.mu.Lock()
	defer s.mu.Unlock()

	opts := src.ImportOptions{
		Format:    format,
		ChunkSize: chunkSize,
		OnError:   onError,
		OnChunk: func(chunkIdx, opsInChunk, totalCommittedOps, lastLine int) {
			progressData, _ := json.Marshal(map[string]any{
				"type":        "progress",
				"chunk":       chunkIdx,
				"opsInChunk":  opsInChunk,
				"totalOps":    totalCommittedOps,
				"lastLine":    lastLine,
			})
			fmt.Fprintf(w, "data: %s\n\n", string(progressData))
			flusher.Flush()
		},
	}

	res, importErr := s.db.BulkImport(filePart, opts)
	finalData, _ := json.Marshal(map[string]any{
		"type":         "complete",
		"lines_read":   res.LinesRead,
		"ops_applied":  res.OpsApplied,
		"chunks":       res.Chunks,
		"skipped":      res.Skipped,
		"last_line":    res.LastLine,
		"elapsed_ms":   res.ElapsedTime.Milliseconds(),
		"stopped_early": res.StoppedEarly,
		"error":        func() string { if importErr != nil { return importErr.Error() }; return "" }(),
	})
	fmt.Fprintf(w, "data: %s\n\n", string(finalData))
	flusher.Flush()
}

func main() {
	dbPath := flag.String("db", "./demo.db", "path to bitcask database file")
	addr := flag.String("addr", "127.0.0.1:8080", "address to bind")
	flag.Parse()

	srv, err := newServer(*dbPath)
	if err != nil {
		log.Fatalf("failed to open database %s: %v", *dbPath, err)
	}
	defer srv.db.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
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
	mux.HandleFunc("/api/import", srv.handleImport)

	log.Printf("Bitcask UI server listening on http://%s (db: %s)", *addr, *dbPath)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

