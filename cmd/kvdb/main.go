package main

import (
	"bufio"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bitcask/src"
)

func printUsage() {
	fmt.Println(`Usage: kvdb <command> [arguments...]

Commands:
  open <path> [--max-bytes N]  Opens or creates database, runs recovery, prints key count and time
  put <path> <key> <value>     Stores the value under key
  get <path> <key>             Prints value, or NOTFOUND
  del <path> <key>             Removes the key
  batch <path> <file>          Applies put/del operations from file ("put k v" or "del k")
  dump <path>                  Prints every key and value, sorted by key ("<key>=<value>")
  check <path>                 Verifies file integrity, prints OK or a reason
  serve <path> [--max-bytes N] Reads ops from stdin, prints ACK/ERR to stdout
  compact <path>               Reclaims deleted and overwritten disk space`)
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "open":
		handleOpen(args)
	case "put":
		handlePut(args)
	case "get":
		handleGet(args)
	case "del":
		handleDel(args)
	case "batch":
		handleBatch(args)
	case "dump":
		handleDump(args)
	case "check":
		handleCheck(args)
	case "serve":
		handleServe(args)
	case "compact":
		handleCompact(args)
	case "import":
		handleImport(args)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func handleOpen(args []string) {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	maxBytes := fs.Int64("max-bytes", 0, "maximum permitted database file size in bytes")
	_ = fs.Parse(args)

	remaining := fs.Args()
	if len(remaining) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb open <path> [--max-bytes N]")
		os.Exit(1)
	}
	path := remaining[0]

	opts := src.DefaultOptions()
	opts.SyncPolicy = src.SyncAlways
	if *maxBytes > 0 {
		opts.MaxBytes = *maxBytes
	}

	t0 := time.Now()
	db, err := src.Open(path, opts)
	dur := time.Since(t0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	snap := db.Snapshot()
	fmt.Printf("Keys: %d, Recovery time: %v\n", len(snap), dur)
}

func handlePut(args []string) {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb put <path> <key> <value>")
		os.Exit(1)
	}
	path, key, val := args[0], args[1], args[2]

	opts := src.DefaultOptions()
	opts.SyncPolicy = src.SyncAlways
	db, err := src.Open(path, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Open error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Put([]byte(key), []byte(val)); err != nil {
		fmt.Fprintf(os.Stderr, "Put error: %v\n", err)
		os.Exit(1)
	}
}

func handleGet(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb get <path> <key>")
		os.Exit(1)
	}
	path, key := args[0], args[1]

	opts := src.DefaultOptions()
	db, err := src.Open(path, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Open error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	val, err := db.Get([]byte(key))
	if err != nil {
		if err == src.ErrNotFound {
			fmt.Println("NOTFOUND")
			return
		}
		fmt.Fprintf(os.Stderr, "Get error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(val))
}

func handleDel(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb del <path> <key>")
		os.Exit(1)
	}
	path, key := args[0], args[1]

	opts := src.DefaultOptions()
	opts.SyncPolicy = src.SyncAlways
	db, err := src.Open(path, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Open error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Delete([]byte(key)); err != nil {
		fmt.Fprintf(os.Stderr, "Del error: %v\n", err)
		os.Exit(1)
	}
}

func handleBatch(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb batch <path> <file>")
		os.Exit(1)
	}
	path, batchFile := args[0], args[1]

	f, err := os.Open(batchFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open batch file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	b := src.NewBatch()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 2*1024*1024), 2*1024*1024)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		switch parts[0] {
		case "put":
			if len(parts) < 3 {
				fmt.Fprintf(os.Stderr, "Line %d: put requires key and value\n", lineNum)
				os.Exit(1)
			}
			if err := b.Put([]byte(parts[1]), []byte(parts[2])); err != nil {
				fmt.Fprintf(os.Stderr, "Line %d: %v\n", lineNum, err)
				os.Exit(1)
			}
		case "del":
			if len(parts) < 2 {
				fmt.Fprintf(os.Stderr, "Line %d: del requires key\n", lineNum)
				os.Exit(1)
			}
			if err := b.Delete([]byte(parts[1])); err != nil {
				fmt.Fprintf(os.Stderr, "Line %d: %v\n", lineNum, err)
				os.Exit(1)
			}
		default:
			fmt.Fprintf(os.Stderr, "Line %d: unknown op %s\n", lineNum, parts[0])
			os.Exit(1)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Error scanning batch file: %v\n", err)
		os.Exit(1)
	}

	opts := src.DefaultOptions()
	opts.SyncPolicy = src.SyncAlways
	db, err := src.Open(path, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Open error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Write(b); err != nil {
		fmt.Fprintf(os.Stderr, "Batch write error: %v\n", err)
		os.Exit(1)
	}
}

func handleDump(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb dump <path>")
		os.Exit(1)
	}
	path := args[0]

	db, err := src.Open(path, src.DefaultOptions())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Open error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	snap := db.Snapshot()
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		val, err := db.Get([]byte(k))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed getting key %s: %v\n", k, err)
			os.Exit(1)
		}
		fmt.Printf("%s=%s\n", k, string(val))
	}
}

func handleCheck(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb check <path>")
		os.Exit(1)
	}
	path := args[0]

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("OK")
			return
		}
		fmt.Printf("FAIL: cannot open file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		fmt.Printf("FAIL: cannot stat file: %v\n", err)
		os.Exit(1)
	}
	fileSize := fi.Size()

	reader := bufio.NewReaderSize(f, 1024*1024)
	var offset int64
	var hdrBuf [src.HeaderSize]byte
	inBatch := false
	var batchOffset int64

	for offset < fileSize {
		recordStart := offset
		if offset+int64(src.HeaderSize) > fileSize {
			fmt.Printf("FAIL: torn header at offset %d (remaining %d bytes < 15)\n", offset, fileSize-offset)
			os.Exit(1)
		}

		if _, err := ioReadFull(reader, hdrBuf[:]); err != nil {
			fmt.Printf("FAIL: read error on header at offset %d: %v\n", offset, err)
			os.Exit(1)
		}

		hdr, err := src.DecodeHeader(hdrBuf[:])
		if err != nil {
			fmt.Printf("FAIL: invalid header at offset %d: %v\n", offset, err)
			os.Exit(1)
		}

		if offset+int64(hdr.TotalLen) > fileSize {
			fmt.Printf("FAIL: incomplete payload at offset %d: record length %d exceeds remaining bytes %d\n",
				offset, hdr.TotalLen, fileSize-offset)
			os.Exit(1)
		}

		keyValLen := int(hdr.TotalLen) - src.HeaderSize
		keyValBuf := make([]byte, keyValLen)
		if _, err := ioReadFull(reader, keyValBuf); err != nil {
			fmt.Printf("FAIL: read error on payload at offset %d: %v\n", offset, err)
			os.Exit(1)
		}

		kBytes := keyValBuf[:hdr.KeyLen]
		vBytes := keyValBuf[hdr.KeyLen:]

		if !src.VerifyCRC(hdrBuf[:], kBytes, vBytes) {
			fmt.Printf("FAIL: CRC mismatch at offset %d\n", offset)
			os.Exit(1)
		}

		if hdr.OpType == src.OpBatchBegin {
			if inBatch {
				fmt.Printf("FAIL: nested BatchBegin at offset %d inside batch from offset %d\n", recordStart, batchOffset)
				os.Exit(1)
			}
			inBatch = true
			batchOffset = recordStart
		} else if hdr.OpType == src.OpBatchCommit {
			if !inBatch {
				fmt.Printf("FAIL: orphan BatchCommit at offset %d without BatchBegin\n", recordStart)
				os.Exit(1)
			}
			inBatch = false
		}

		offset += int64(hdr.TotalLen)
	}

	if inBatch {
		fmt.Printf("FAIL: uncommitted batch starting at offset %d with no BatchCommit marker\n", batchOffset)
		os.Exit(1)
	}

	fmt.Println("OK")
}

func ioReadFull(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// decodeParam decodes a parameter that may be hex-encoded or plain string.
// If hex.DecodeString succeeds, it returns the binary bytes; otherwise returns raw bytes.
func decodeParam(s string) []byte {
	if b, err := hex.DecodeString(s); err == nil && len(b) > 0 {
		return b
	}
	return []byte(s)
}

func handleServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	maxBytes := fs.Int64("max-bytes", 0, "maximum permitted database file size in bytes")
	_ = fs.Parse(args)

	remaining := fs.Args()
	if len(remaining) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb serve <path> [--max-bytes N]")
		os.Exit(1)
	}
	path := remaining[0]

	opts := src.DefaultOptions()
	opts.SyncPolicy = src.SyncAlways
	if *maxBytes > 0 {
		opts.MaxBytes = *maxBytes
	}

	db, err := src.Open(path, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Open error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	var stdoutMu sync.Mutex
	printACK := func(opID string) {
		stdoutMu.Lock()
		defer stdoutMu.Unlock()
		fmt.Printf("ACK %s\n", opID)
		_ = os.Stdout.Sync()
	}
	printERR := func(opID, reason string) {
		stdoutMu.Lock()
		defer stdoutMu.Unlock()
		fmt.Printf("ERR %s %s\n", opID, reason)
		_ = os.Stdout.Sync()
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 2*1024*1024), 2*1024*1024)

	var wg sync.WaitGroup

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, " ", 4)
		if len(parts) < 2 {
			continue
		}
		opID := parts[0]
		op := parts[1]

		switch op {
		case "put":
			if len(parts) < 4 {
				printERR(opID, "put requires key and value")
				continue
			}
			kBytes := decodeParam(parts[2])
			vBytes := decodeParam(parts[3])

			wg.Add(1)
			go func(id string, k, v []byte) {
				defer wg.Done()
				if err := db.Put(k, v); err != nil {
					printERR(id, err.Error())
				} else {
					printACK(id)
				}
			}(opID, kBytes, vBytes)

		case "del":
			if len(parts) < 3 {
				printERR(opID, "del requires key")
				continue
			}
			kBytes := decodeParam(parts[2])

			wg.Add(1)
			go func(id string, k []byte) {
				defer wg.Done()
				if err := db.Delete(k); err != nil {
					printERR(id, err.Error())
				} else {
					printACK(id)
				}
			}(opID, kBytes)

		case "batch":
			if len(parts) < 3 {
				printERR(opID, "batch requires count or file")
				continue
			}

			// Check if arg is an integer count <n> or a file path
			count, parseErr := strconv.Atoi(parts[2])
			if parseErr == nil && count >= 0 {
				// Read n op lines atomically from scanner
				var opLines []string
				hasError := false
				for i := 0; i < count; i++ {
					if !scanner.Scan() {
						printERR(opID, "unexpected EOF while reading batch op lines")
						hasError = true
						break
					}
					opLines = append(opLines, strings.TrimSpace(scanner.Text()))
				}
				if hasError {
					continue
				}

				b := src.NewBatch()
				buildErr := false
				for _, opLine := range opLines {
					if opLine == "" || strings.HasPrefix(opLine, "#") {
						continue
					}
					opParts := strings.SplitN(opLine, " ", 3)
					switch opParts[0] {
					case "put":
						if len(opParts) < 3 {
							printERR(opID, fmt.Sprintf("bad batch put line: %s", opLine))
							buildErr = true
							break
						}
						bk := decodeParam(opParts[1])
						bv := decodeParam(opParts[2])
						if err := b.Put(bk, bv); err != nil {
							printERR(opID, err.Error())
							buildErr = true
							break
						}
					case "del":
						if len(opParts) < 2 {
							printERR(opID, fmt.Sprintf("bad batch del line: %s", opLine))
							buildErr = true
							break
						}
						bk := decodeParam(opParts[1])
						if err := b.Delete(bk); err != nil {
							printERR(opID, err.Error())
							buildErr = true
							break
						}
					default:
						printERR(opID, fmt.Sprintf("unknown op in batch: %s", opParts[0]))
						buildErr = true
						break
					}
					if buildErr {
						break
					}
				}
				if buildErr {
					continue
				}

				wg.Add(1)
				go func(id string, batchObj *src.Batch) {
					defer wg.Done()
					if err := db.Write(batchObj); err != nil {
						printERR(id, err.Error())
					} else {
						printACK(id)
					}
				}(opID, b)

			} else {
				// Legacy file-based batch
				batchFile := parts[2]
				b, err := readBatchFile(batchFile)
				if err != nil {
					printERR(opID, err.Error())
					continue
				}

				wg.Add(1)
				go func(id string, batchObj *src.Batch) {
					defer wg.Done()
					if err := db.Write(batchObj); err != nil {
						printERR(id, err.Error())
					} else {
						printACK(id)
					}
				}(opID, b)
			}

		case "compact":
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				if err := db.Compact(); err != nil {
					printERR(id, err.Error())
				} else {
					printACK(id)
				}
			}(opID)

		default:
			printERR(opID, fmt.Sprintf("unknown op %s", op))
		}
	}

	wg.Wait()
}

func readBatchFile(filePath string) (*src.Batch, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	b := src.NewBatch()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 2*1024*1024), 2*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		switch parts[0] {
		case "put":
			if len(parts) < 3 {
				return nil, fmt.Errorf("bad put in batch: %s", line)
			}
			if err := b.Put([]byte(parts[1]), []byte(parts[2])); err != nil {
				return nil, err
			}
		case "del":
			if len(parts) < 2 {
				return nil, fmt.Errorf("bad del in batch: %s", line)
			}
			if err := b.Delete([]byte(parts[1])); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unknown op in batch: %s", parts[0])
		}
	}
	return b, scanner.Err()
}

func handleCompact(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb compact <path>")
		os.Exit(1)
	}
	path := args[0]
	opts := src.DefaultOptions()
	opts.SyncPolicy = src.SyncAlways
	db, err := src.Open(path, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Open error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Compact(); err != nil {
		fmt.Fprintf(os.Stderr, "Compact error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("ACK")
}

func handleImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	formatStr := fs.String("format", "lines", "input data format (lines, csv, jsonl)")
	chunkSize := fs.Int("chunk", 5000, "number of ops per atomic chunk batch (max 10000)")
	maxBytes := fs.Int64("max-bytes", 0, "maximum permitted database file size in bytes")
	onErrorStr := fs.String("on-error", "abort", "action on malformed/invalid line (abort, skip)")
	startLine := fs.Int("start-line", 1, "1-based line number to start/resume importing from")
	_ = fs.Parse(args)

	remaining := fs.Args()
	if len(remaining) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb import <path> <file> [--format lines|csv|jsonl] [--chunk N] [--max-bytes N] [--on-error skip|abort] [--start-line L]")
		os.Exit(1)
	}
	dbPath := remaining[0]
	filePath := remaining[1]

	opts := src.DefaultOptions()
	opts.SyncPolicy = src.SyncAlways
	if *maxBytes > 0 {
		opts.MaxBytes = *maxBytes
	}

	db, err := src.Open(dbPath, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Open error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	f, err := os.Open(filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open import file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	importOpts := src.ImportOptions{
		Format:    src.ImportFormat(*formatStr),
		ChunkSize: *chunkSize,
		StartLine: *startLine,
		OnError:   src.OnErrorAction(*onErrorStr),
		OnChunk: func(chunkIdx, opsInChunk, totalCommittedOps, lastLine int) {
			fmt.Printf("ACK chunk %d ops=%d total=%d last_line=%d\n", chunkIdx, opsInChunk, totalCommittedOps, lastLine)
			_ = os.Stdout.Sync()
		},
	}

	res, err := db.BulkImport(f, importOpts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Import stopped with error: %v (last committed line: %d, chunks: %d, applied: %d)\n",
			err, res.LastLine, res.Chunks, res.OpsApplied)
		fmt.Printf("SUMMARY: lines_read=%d ops_applied=%d chunks=%d skipped=%d last_line=%d elapsed=%v error=%v\n",
			res.LinesRead, res.OpsApplied, res.Chunks, res.Skipped, res.LastLine, res.ElapsedTime.Round(time.Millisecond), err)
		os.Exit(1)
	}

	fmt.Printf("SUMMARY: lines_read=%d ops_applied=%d chunks=%d skipped=%d last_line=%d elapsed=%v\n",
		res.LinesRead, res.OpsApplied, res.Chunks, res.Skipped, res.LastLine, res.ElapsedTime.Round(time.Millisecond))
}
