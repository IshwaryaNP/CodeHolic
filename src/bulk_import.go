package src

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ImportFormat specifies the syntax of the input data stream.
type ImportFormat string

const (
	FormatLines ImportFormat = "lines"
	FormatCSV   ImportFormat = "csv"
	FormatJSONL ImportFormat = "jsonl"
)

// OnErrorAction specifies how parse or validation errors on individual lines are handled.
type OnErrorAction string

const (
	OnErrorAbort OnErrorAction = "abort"
	OnErrorSkip  OnErrorAction = "skip"
)

// ImportOptions configures a BulkImport run.
type ImportOptions struct {
	Format    ImportFormat
	ChunkSize int           // default 5000, max 10000
	StartLine int           // 1-based start line (skip lines before StartLine)
	OnError   OnErrorAction // default abort
	OnChunk   func(chunkIdx int, opsInChunk int, totalCommittedOps int, lastLine int)
}

// ImportResult contains execution statistics from a BulkImport run.
type ImportResult struct {
	LinesRead    int           `json:"lines_read"`
	OpsApplied   int           `json:"ops_applied"`
	Chunks       int           `json:"chunks"`
	Skipped      int           `json:"skipped"`
	LastLine     int           `json:"last_line"`
	ElapsedTime  time.Duration `json:"elapsed_time"`
	StoppedEarly bool          `json:"stopped_early"`
	Error        error         `json:"error,omitempty"`
}

// BulkImport streams key/value records from reader and writes them in atomic chunks.
// It never loads the entire file into memory. Each chunk is committed via db.Write(batch)
// which performs ONE buffer encoding, ONE Write, and ONE Sync.
func (db *DB) BulkImport(reader io.Reader, opts ImportOptions) (ImportResult, error) {
	start := time.Now()
	res := ImportResult{}

	chunkLimit := opts.ChunkSize
	if chunkLimit <= 0 {
		chunkLimit = 5000
	}
	if chunkLimit > 10000 {
		chunkLimit = 10000
	}

	onError := opts.OnError
	if onError == "" {
		onError = OnErrorAbort
	}

	format := opts.Format
	if format == "" {
		format = FormatLines
	}

	startLine := opts.StartLine
	if startLine < 1 {
		startLine = 1
	}

	type pendingOp struct {
		isDel bool
		key   []byte
		val   []byte
		line  int
	}

	chunkOps := make([]pendingOp, 0, chunkLimit)
	commitChunk := func() error {
		if len(chunkOps) == 0 {
			return nil
		}
		b := NewBatch()
		for _, op := range chunkOps {
			if op.isDel {
				if err := b.Delete(op.key); err != nil {
					return err
				}
			} else {
				if err := b.Put(op.key, op.val); err != nil {
					return err
				}
			}
		}

		if err := db.Write(b); err != nil {
			return err
		}

		res.Chunks++
		res.OpsApplied += len(chunkOps)
		lastLineInChunk := chunkOps[len(chunkOps)-1].line
		res.LastLine = lastLineInChunk
		if opts.OnChunk != nil {
			opts.OnChunk(res.Chunks, len(chunkOps), res.OpsApplied, res.LastLine)
		}
		chunkOps = chunkOps[:0]
		return nil
	}

	bufReader := bufio.NewReaderSize(reader, 1024*1024)
	currentLine := 0

	switch format {
	case FormatLines:
		scanner := bufio.NewScanner(bufReader)
		// 2MB buffer for large values
		scanner.Buffer(make([]byte, 2*1024*1024), 2*1024*1024)

		for scanner.Scan() {
			currentLine++
			res.LinesRead++
			if currentLine < startLine {
				res.Skipped++
				continue
			}

			lineText := scanner.Text()
			lineText = strings.TrimRight(lineText, "\r\n")
			if strings.TrimSpace(lineText) == "" {
				res.Skipped++
				continue
			}

			// Format: "put <key> <value>" or "del <key>"
			parts := strings.SplitN(lineText, " ", 3)
			cmd := parts[0]

			var isDel bool
			var k, v []byte

			if cmd == "put" {
				if len(parts) < 3 {
					if onError == OnErrorAbort {
						res.ElapsedTime = time.Since(start)
						err := fmt.Errorf("line %d: malformed put command", currentLine)
						res.Error = err
						return res, err
					}
					res.Skipped++
					continue
				}
				isDel = false
				k = []byte(parts[1])
				v = []byte(parts[2])
			} else if cmd == "del" {
				if len(parts) < 2 {
					if onError == OnErrorAbort {
						res.ElapsedTime = time.Since(start)
						err := fmt.Errorf("line %d: malformed del command", currentLine)
						res.Error = err
						return res, err
					}
					res.Skipped++
					continue
				}
				isDel = true
				k = []byte(parts[1])
			} else {
				if onError == OnErrorAbort {
					res.ElapsedTime = time.Since(start)
					err := fmt.Errorf("line %d: unknown command %q", currentLine, cmd)
					res.Error = err
					return res, err
				}
				res.Skipped++
				continue
			}

			// Validate limits per line
			if len(k) == 0 || len(k) > SpecMaxKeySize || (!isDel && len(v) > SpecMaxValueSize) {
				if onError == OnErrorAbort {
					res.ElapsedTime = time.Since(start)
					err := fmt.Errorf("line %d: key or value size exceeds limits (key=%d, val=%d)", currentLine, len(k), len(v))
					res.Error = err
					return res, err
				}
				res.Skipped++
				continue
			}

			chunkOps = append(chunkOps, pendingOp{isDel: isDel, key: k, val: v, line: currentLine})
			if len(chunkOps) >= chunkLimit {
				if err := commitChunk(); err != nil {
					res.ElapsedTime = time.Since(start)
					res.StoppedEarly = true
					res.Error = err
					return res, err
				}
			}
		}
		if err := scanner.Err(); err != nil {
			res.ElapsedTime = time.Since(start)
			res.Error = err
			return res, err
		}

	case FormatCSV:
		csvReader := csv.NewReader(bufReader)
		csvReader.FieldsPerRecord = -1 // Allow variable or check explicitly

		for {
			record, err := csvReader.Read()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				currentLine++
				res.LinesRead++
				if onError == OnErrorAbort {
					res.ElapsedTime = time.Since(start)
					err = fmt.Errorf("line %d: csv parse error: %w", currentLine, err)
					res.Error = err
					return res, err
				}
				res.Skipped++
				continue
			}

			currentLine++
			res.LinesRead++
			if currentLine < startLine {
				res.Skipped++
				continue
			}

			// Format: key,value
			if len(record) < 2 {
				if onError == OnErrorAbort {
					res.ElapsedTime = time.Since(start)
					err := fmt.Errorf("line %d: csv row requires at least 2 fields, got %d", currentLine, len(record))
					res.Error = err
					return res, err
				}
				res.Skipped++
				continue
			}

			k := []byte(record[0])
			v := []byte(record[1])

			if len(k) == 0 || len(k) > SpecMaxKeySize || len(v) > SpecMaxValueSize {
				if onError == OnErrorAbort {
					res.ElapsedTime = time.Since(start)
					err := fmt.Errorf("line %d: key or value size exceeds limits (key=%d, val=%d)", currentLine, len(k), len(v))
					res.Error = err
					return res, err
				}
				res.Skipped++
				continue
			}

			chunkOps = append(chunkOps, pendingOp{isDel: false, key: k, val: v, line: currentLine})
			if len(chunkOps) >= chunkLimit {
				if err := commitChunk(); err != nil {
					res.ElapsedTime = time.Since(start)
					res.StoppedEarly = true
					res.Error = err
					return res, err
				}
			}
		}

	case FormatJSONL:
		scanner := bufio.NewScanner(bufReader)
		scanner.Buffer(make([]byte, 2*1024*1024), 2*1024*1024)

		type jsonlRecord struct {
			Key   string `json:"key"`
			Value string `json:"value"`
			Op    string `json:"op"`
		}

		for scanner.Scan() {
			currentLine++
			res.LinesRead++
			if currentLine < startLine {
				res.Skipped++
				continue
			}

			lineBytes := scanner.Bytes()
			if len(strings.TrimSpace(string(lineBytes))) == 0 {
				res.Skipped++
				continue
			}

			var item jsonlRecord
			if err := json.Unmarshal(lineBytes, &item); err != nil {
				if onError == OnErrorAbort {
					res.ElapsedTime = time.Since(start)
					err := fmt.Errorf("line %d: json unmarshal error: %w", currentLine, err)
					res.Error = err
					return res, err
				}
				res.Skipped++
				continue
			}

			isDel := item.Op == "del"
			k := []byte(item.Key)
			v := []byte(item.Value)

			if len(k) == 0 || len(k) > SpecMaxKeySize || (!isDel && len(v) > SpecMaxValueSize) {
				if onError == OnErrorAbort {
					res.ElapsedTime = time.Since(start)
					err := fmt.Errorf("line %d: key or value size exceeds limits (key=%d, val=%d)", currentLine, len(k), len(v))
					res.Error = err
					return res, err
				}
				res.Skipped++
				continue
			}

			chunkOps = append(chunkOps, pendingOp{isDel: isDel, key: k, val: v, line: currentLine})
			if len(chunkOps) >= chunkLimit {
				if err := commitChunk(); err != nil {
					res.ElapsedTime = time.Since(start)
					res.StoppedEarly = true
					res.Error = err
					return res, err
				}
			}
		}
		if err := scanner.Err(); err != nil {
			res.ElapsedTime = time.Since(start)
			res.Error = err
			return res, err
		}
	}

	// Commit any trailing ops in the final chunk
	if len(chunkOps) > 0 {
		if err := commitChunk(); err != nil {
			res.ElapsedTime = time.Since(start)
			res.StoppedEarly = true
			res.Error = err
			return res, err
		}
	}

	res.ElapsedTime = time.Since(start)
	return res, nil
}
