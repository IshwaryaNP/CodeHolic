package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
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
  serve <path>                 Reads ops from stdin ("put k v", "del k", "batch <file>"), prints ACK <id>`)
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

	db, err := src.Open(path, src.DefaultOptions())
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

	db, err := src.Open(path, src.DefaultOptions())
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

	db, err := src.Open(path, src.DefaultOptions())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Open error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Delete([]byte(key)); err != nil {
		fmt.Fprintf(os.Stderr, "Delete error: %v\n", err)
		os.Exit(1)
	}
}

func handleBatch(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb batch <path> <file>")
		os.Exit(1)
	}
	path, filePath := args[0], args[1]

	f, err := os.Open(filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot read batch file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	b := src.NewBatch()
	scanner := bufio.NewScanner(f)
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

	db, err := src.Open(path, src.DefaultOptions())
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
			fmt.Printf("FAIL: error reading header at offset %d: %v\n", offset, err)
			os.Exit(1)
		}

		hdr, err := src.DecodeHeader(hdrBuf[:])
		if err != nil {
			fmt.Printf("FAIL: corrupt header at offset %d: %v\n", offset, err)
			os.Exit(1)
		}

		if offset+int64(hdr.TotalLen) > fileSize {
			fmt.Printf("FAIL: incomplete record at offset %d: record needs %d bytes, file has %d\n", offset, hdr.TotalLen, fileSize-offset)
			os.Exit(1)
		}

		payload := make([]byte, int(hdr.TotalLen)-src.HeaderSize)
		if _, err := ioReadFull(reader, payload); err != nil {
			fmt.Printf("FAIL: incomplete payload at offset %d: %v\n", offset, err)
			os.Exit(1)
		}

		key := payload[:hdr.KeyLen]
		val := payload[hdr.KeyLen:]

		if !src.VerifyCRC(hdrBuf[:], key, val) {
			fmt.Printf("FAIL: CRC mismatch at offset %d\n", offset)
			os.Exit(1)
		}

		if hdr.OpType == src.OpBatchBegin {
			if inBatch {
				fmt.Printf("FAIL: nested BatchBegin at offset %d\n", offset)
				os.Exit(1)
			}
			inBatch = true
			batchOffset = recordStart
		} else if hdr.OpType == src.OpBatchCommit {
			if !inBatch {
				fmt.Printf("FAIL: orphan BatchCommit at offset %d without BatchBegin\n", offset)
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
	if *maxBytes > 0 {
		opts.MaxBytes = *maxBytes
	}

	db, err := src.Open(path, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Open error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// Notify ready
	scanner := bufio.NewScanner(os.Stdin)
	// Larger buffer for up to 1MB values
	scanner.Buffer(make([]byte, 2*1024*1024), 2*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Protocol:
		// <op-id> put <key> <val>
		// <op-id> del <key>
		// <op-id> batch <file>
		// <op-id> compact
		parts := strings.SplitN(line, " ", 4)
		if len(parts) < 2 {
			continue
		}
		opID := parts[0]
		op := parts[1]

		switch op {
		case "put":
			if len(parts) < 4 {
				fmt.Fprintf(os.Stderr, "ERR %s put requires key and value\n", opID)
				continue
			}
			k := parts[2]
			v := parts[3]
			if err := db.Put([]byte(k), []byte(v)); err != nil {
				fmt.Fprintf(os.Stderr, "ERR %s put: %v\n", opID, err)
			} else {
				fmt.Printf("ACK %s\n", opID)
				os.Stdout.Sync()
			}

		case "del":
			if len(parts) < 3 {
				fmt.Fprintf(os.Stderr, "ERR %s del requires key\n", opID)
				continue
			}
			k := parts[2]
			if err := db.Delete([]byte(k)); err != nil {
				fmt.Fprintf(os.Stderr, "ERR %s del: %v\n", opID, err)
			} else {
				fmt.Printf("ACK %s\n", opID)
				os.Stdout.Sync()
			}

		case "batch":
			if len(parts) < 3 {
				fmt.Fprintf(os.Stderr, "ERR %s batch requires file path\n", opID)
				continue
			}
			batchFile := parts[2]
			b, err := readBatchFile(batchFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERR %s batch read: %v\n", opID, err)
				continue
			}
			if err := db.Write(b); err != nil {
				fmt.Fprintf(os.Stderr, "ERR %s batch write: %v\n", opID, err)
			} else {
				fmt.Printf("ACK %s\n", opID)
				os.Stdout.Sync()
			}

		case "compact":
			if err := db.Compact(); err != nil {
				fmt.Fprintf(os.Stderr, "ERR %s compact: %v\n", opID, err)
			} else {
				fmt.Printf("ACK %s\n", opID)
				os.Stdout.Sync()
			}

		default:
			fmt.Fprintf(os.Stderr, "ERR %s unknown op %s\n", opID, op)
		}
	}
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
