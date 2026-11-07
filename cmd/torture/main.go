package main

import (
	"bufio"
	"flag"
	"fmt"
	"hash/crc32"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Iterations int
	Seed       int64
	Mode       string
	Path       string
	KVDBPath   string
}

// OpInterval records the lifecycle of an operation on a key.
// Interval [SentSeq, AckSeq] defines when the op was in flight.
// If not ACKed before crash, AckSeq is math.MaxInt64.
type OpInterval struct {
	OpID    string
	Key     string
	Val     string // "" for delete
	SentSeq int64
	AckSeq  int64
	IsAcked bool
}

// Oracle tracks every operation sent to and ACKed by kvdb across crash iterations.
// It logs directly to a separate flushed oracle file (oracle.log).
//
// Concurrency rule: Since writers may pipeline ops without waiting for ACK,
// the oracle treats ops whose [SENT, ACK-or-end] intervals overlap on the same key
// as concurrent, and accepts any of their values. It does not infer strict order
// solely from ACK order.
type Oracle struct {
	mu           sync.Mutex
	logFile      *os.File
	logWriter    *bufio.Writer
	globalSeq    int64
	opHistory    map[string][]OpInterval     // key -> history of all intervals
	validValues  map[string]map[string]bool // key -> set of valid written values ever attempted
	lastAckedVal map[string]string          // key -> latest strictly ACKed value if sequential
}

func NewOracle(oracleLogPath string) (*Oracle, error) {
	f, err := os.OpenFile(oracleLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return nil, fmt.Errorf("failed creating oracle log file: %w", err)
	}

	return &Oracle{
		logFile:      f,
		logWriter:    bufio.NewWriter(f),
		opHistory:    make(map[string][]OpInterval),
		validValues:  make(map[string]map[string]bool),
		lastAckedVal: make(map[string]string),
	}, nil
}

func (o *Oracle) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.logWriter != nil {
		_ = o.logWriter.Flush()
	}
	if o.logFile != nil {
		_ = o.logFile.Sync()
		return o.logFile.Close()
	}
	return nil
}

// RecordSent logs SENT <id> <op> and flushes immediately before the op goes to stdin.
func (o *Oracle) RecordSent(opID, key, val string) int64 {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.globalSeq++
	seq := o.globalSeq

	if o.validValues[key] == nil {
		o.validValues[key] = make(map[string]bool)
	}
	o.validValues[key][val] = true

	interval := OpInterval{
		OpID:    opID,
		Key:     key,
		Val:     val,
		SentSeq: seq,
		AckSeq:  1<<62 - 1, // MaxInt64 equivalent
		IsAcked: false,
	}
	o.opHistory[key] = append(o.opHistory[key], interval)

	// Flush to disk
	_, _ = fmt.Fprintf(o.logWriter, "SENT %d %s %s %s\n", seq, opID, key, val)
	_ = o.logWriter.Flush()
	_ = o.logFile.Sync()

	return seq
}

// RecordACK logs ACK only for complete newline-terminated lines.
func (o *Oracle) RecordACK(opID, key, val string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.globalSeq++
	ackSeq := o.globalSeq

	intervals := o.opHistory[key]
	for i := len(intervals) - 1; i >= 0; i-- {
		if intervals[i].OpID == opID {
			intervals[i].AckSeq = ackSeq
			intervals[i].IsAcked = true
			break
		}
	}

	o.lastAckedVal[key] = val

	_, _ = fmt.Fprintf(o.logWriter, "ACK %d %s %s\n", ackSeq, opID, key)
	_ = o.logWriter.Flush()
	_ = o.logFile.Sync()
}

// IsValidValue verifies Rule 4: value was ever registered/written for that key.
func (o *Oracle) IsValidValue(key, val string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()

	set := o.validValues[key]
	if set == nil {
		return false
	}
	return set[val]
}

// AcceptableValues computes all valid values for a key that could survive a crash,
// taking into account overlapping concurrent operations.
func (o *Oracle) AcceptableValues(key string) map[string]bool {
	o.mu.Lock()
	defer o.mu.Unlock()

	res := make(map[string]bool)
	intervals := o.opHistory[key]
	if len(intervals) == 0 {
		return res
	}

	// Find the latest ACKed op
	var lastAckedIndex = -1
	for i := len(intervals) - 1; i >= 0; i-- {
		if intervals[i].IsAcked {
			lastAckedIndex = i
			break
		}
	}

	if lastAckedIndex >= 0 {
		ackInterval := intervals[lastAckedIndex]
		res[ackInterval.Val] = true

		// Any un-ACKed or later op whose SentSeq overlaps with or follows ackInterval can be valid
		for i := lastAckedIndex + 1; i < len(intervals); i++ {
			res[intervals[i].Val] = true
		}
		// Also any op whose interval overlapped with the ACKed op
		for i := 0; i < lastAckedIndex; i++ {
			if intervals[i].AckSeq >= ackInterval.SentSeq {
				res[intervals[i].Val] = true
			}
		}
	} else {
		// Nothing was ACKed yet; either absent ("") or any attempted value
		res[""] = true
		for _, inv := range intervals {
			res[inv.Val] = true
		}
	}

	return res
}

func main() {
	iterations := flag.Int("iterations", 500, "number of kill/restart iterations")
	seed := flag.Int64("seed", 0, "random seed (mandatory for reproducibility)")
	mode := flag.String("mode", "default", "mode: default | large-values | compaction | recovery-crash | large-batches | disk-full | bulk-import")
	dbPath := flag.String("db", "", "path to test db")
	kvdbBin := flag.String("kvdb", "./kvdb.exe", "path to kvdb binary")
	flag.Parse()

	if *seed == 0 {
		*seed = time.Now().UnixNano()
	}

	var tmpDir string
	if *dbPath == "" {
		var err error
		tmpDir, err = os.MkdirTemp("", "torture_*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed creating temp dir: %v\n", err)
			os.Exit(1)
		}
		defer os.RemoveAll(tmpDir)
		*dbPath = filepath.Join(tmpDir, "torture.db")
	}

	oracleLogPath := *dbPath + ".oracle.log"
	oracle, err := NewOracle(oracleLogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed creating oracle: %v\n", err)
		os.Exit(1)
	}
	defer oracle.Close()

	cfg := Config{
		Iterations: *iterations,
		Seed:       *seed,
		Mode:       *mode,
		Path:       *dbPath,
		KVDBPath:   *kvdbBin,
	}

	fmt.Printf("=== Starting Torture Tool ===\n")
	fmt.Printf("Iterations: %d | Seed: %d | Mode: %s | DB: %s\n", cfg.Iterations, cfg.Seed, cfg.Mode, cfg.Path)

	r := rand.New(rand.NewSource(cfg.Seed))

	passed := 0
	for iter := 1; iter <= cfg.Iterations; iter++ {
		iterSeed := r.Int63()
		var iterErr error
		if cfg.Mode == "bulk-import" {
			iterErr = runBulkImportIteration(iter, iterSeed, cfg)
		} else {
			iterErr = runIteration(iter, iterSeed, cfg, oracle)
		}
		if iterErr != nil {
			fmt.Printf("\n[FAILURE] Iteration %d failed with seed %d: %v\n", iter, cfg.Seed, iterErr)
			os.Exit(1)
		}
		passed++
		if iter%10 == 0 || iter == cfg.Iterations {
			fmt.Printf("[PASS] Completed %d/%d iterations\n", iter, cfg.Iterations)
		}
	}

	fmt.Printf("\n=== Torture Run SUCCESS: %d/%d iterations passed ===\n", passed, cfg.Iterations)
}

func runIteration(iter int, seed int64, cfg Config, oracle *Oracle) error {
	r := rand.New(rand.NewSource(seed))

	serveArgs := []string{"serve"}
	if cfg.Mode == "disk-full" {
		serveArgs = append(serveArgs, "--max-bytes", "131072") // 128 KB
	}
	serveArgs = append(serveArgs, cfg.Path)

	cmd := exec.Command(cfg.KVDBPath, serveArgs...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe error: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe error: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed starting kvdb serve: %w", err)
	}

	stdoutScanner := bufio.NewScanner(stdout)
	stdoutScanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	stdinWriter := bufio.NewWriter(stdin)

	var opCounter int64
	stopWriters := make(chan struct{})

	type opReq struct {
		opID     string
		line     string
		key      string
		val      string
		isBatch  bool
		batchMap map[string]string
	}

	opQueue := make(chan opReq, 256)

	// In-flight tracking for pending ACKs
	var inFlightMu sync.Mutex
	inFlightOps := make(map[string]opReq)

	// Reader goroutine for ACKs: records ACK only for complete newline-terminated lines
	go func() {
		for stdoutScanner.Scan() {
			txt := strings.TrimSpace(stdoutScanner.Text())
			if strings.HasPrefix(txt, "ACK ") {
				id := strings.TrimPrefix(txt, "ACK ")
				inFlightMu.Lock()
				req, exists := inFlightOps[id]
				if exists {
					delete(inFlightOps, id)
				}
				inFlightMu.Unlock()

				if exists {
					if req.isBatch {
						for bk, bv := range req.batchMap {
							oracle.RecordACK(req.opID, bk, bv)
						}
					} else {
						oracle.RecordACK(req.opID, req.key, req.val)
					}
				}
			}
		}
	}()

	// Pipelined stdin dispatcher: writes and flushes SENT before the op goes to stdin
	go func() {
		for {
			select {
			case <-stopWriters:
				return
			case req := <-opQueue:
				inFlightMu.Lock()
				inFlightOps[req.opID] = req
				inFlightMu.Unlock()

				// Flush SENT to oracle file first
				if req.isBatch {
					for bk, bv := range req.batchMap {
						oracle.RecordSent(req.opID, bk, bv)
					}
				} else {
					oracle.RecordSent(req.opID, req.key, req.val)
				}

				// Write line to stdin
				_, wErr := stdinWriter.WriteString(req.line)
				if wErr == nil {
					_ = stdinWriter.Flush()
				}
			}
		}
	}()

	// 2 concurrent writers producing random operations and pipelining them
	numWriters := 2
	var wg sync.WaitGroup

	for w := 0; w < numWriters; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				select {
				case <-stopWriters:
					return
				default:
				}

				id := atomic.AddInt64(&opCounter, 1)
				opID := fmt.Sprintf("w%d_%d", workerID, id)
				keyNum := r.Intn(60) // High collision density
				key := fmt.Sprintf("k_%d", keyNum)

				var req opReq
				req.opID = opID

				// 10% chance of empty value ("-")
				if r.Float32() < 0.10 {
					req.key = key
					req.val = ""
					// Key hex encoded, empty value as "-"
					req.line = fmt.Sprintf("%s put %x -\n", opID, key)
				} else if cfg.Mode == "large-values" && r.Float32() < 0.25 {
					valSize := 128 * 1024
					if r.Float32() < 0.5 {
						valSize = 256 * 1024
					}
					// Embed op-id + checksum in generated value
					rawVal := fmt.Sprintf("val_%s_%s", opID, strings.Repeat("A", valSize))
					c := crc32.ChecksumIEEE([]byte(rawVal))
					val := fmt.Sprintf("%s_%x", rawVal, c)

					req.key = key
					req.val = val
					req.line = fmt.Sprintf("%s put %x %x\n", opID, key, val)
				} else if cfg.Mode == "large-batches" && r.Float32() < 0.2 {
					batchCount := 20 + r.Intn(30)
					bMap := make(map[string]string)
					var bLines strings.Builder
					for b := 0; b < batchCount; b++ {
						bk := fmt.Sprintf("batch_k_%d", r.Intn(40))
						bv := fmt.Sprintf("batch_v_%s_%d", opID, b)
						bLines.WriteString(fmt.Sprintf("put %x %x\n", bk, bv))
						bMap[bk] = bv
					}
					req.isBatch = true
					req.batchMap = bMap
					req.line = fmt.Sprintf("%s batch %d\n%s", opID, batchCount, bLines.String())
				} else if cfg.Mode == "compaction" && r.Float32() < 0.15 {
					req.line = fmt.Sprintf("%s compact\n", opID)
				} else {
					if r.Float32() < 0.80 {
						val := fmt.Sprintf("v_%s_%d", opID, r.Intn(100000))
						req.key = key
						req.val = val
						req.line = fmt.Sprintf("%s put %x %x\n", opID, key, val)
					} else {
						req.key = key
						req.val = ""
						req.line = fmt.Sprintf("%s del %x\n", opID, key)
					}
				}

				if req.line != "" {
					select {
					case opQueue <- req:
					case <-stopWriters:
						return
					}
				}

				time.Sleep(time.Duration(r.Intn(2)+1) * time.Millisecond)
			}
		}(w)
	}

	// Genuinely random delay before SIGKILL (10ms to 75ms)
	killDelay := time.Duration(10+r.Intn(65)) * time.Millisecond
	time.Sleep(killDelay)

	close(stopWriters)

	// Immediate SIGKILL (never graceful stdin.Close or SIGTERM)
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
	wg.Wait()

	// Recovery-crash mode: simulate a kill while recovery (open) is executing
	if cfg.Mode == "recovery-crash" {
		recCmd := exec.Command(cfg.KVDBPath, "open", cfg.Path)
		_ = recCmd.Start()
		time.Sleep(time.Duration(r.Intn(5)+1) * time.Millisecond)
		if recCmd.Process != nil {
			_ = recCmd.Process.Kill()
			_ = recCmd.Wait()
		}
	}

	// Restart with open, check, and dump
	outOpen, err := exec.Command(cfg.KVDBPath, "open", cfg.Path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("kvdb open failed after crash: %v, output: %s", err, string(outOpen))
	}

	outCheck, err := exec.Command(cfg.KVDBPath, "check", cfg.Path).CombinedOutput()
	if err != nil || !strings.Contains(string(outCheck), "OK") {
		return fmt.Errorf("kvdb check failed after crash: %v, output: %s", err, string(outCheck))
	}

	outDump, err := exec.Command(cfg.KVDBPath, "dump", cfg.Path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("kvdb dump failed: %v, output: %s", err, string(outDump))
	}

	// Parse actual dump
	actual := make(map[string]string)
	dumpScanner := bufio.NewScanner(strings.NewReader(string(outDump)))
	dumpScanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	for dumpScanner.Scan() {
		line := dumpScanner.Text()
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			actual[parts[0]] = parts[1]
		}
	}

	// Rule 4: No key ever holds an unknown value (value never written for that key)
	for actK, actV := range actual {
		if !oracle.IsValidValue(actK, actV) {
			return fmt.Errorf("invariant violation (Rule 4): key '%s' holds unknown value '%s'", actK, actV)
		}
	}

	// Rule 1: Every ACKed operation must survive unless overwritten by a valid concurrent op
	for expK := range oracle.validValues {
		acceptable := oracle.AcceptableValues(expK)
		actV, exists := actual[expK]

		if !exists {
			if !acceptable[""] {
				return fmt.Errorf("durability violation (Rule 1): key '%s' is missing from DB, acceptable: %v", expK, acceptable)
			}
		} else {
			if !acceptable[actV] {
				return fmt.Errorf("durability violation (Rule 1): key '%s' holds '%s', not in acceptable set: %v", expK, actV, acceptable)
			}
		}
	}

	return nil
}

func runBulkImportIteration(iter int, seed int64, cfg Config) error {
	r := rand.New(rand.NewSource(seed))

	iterDBPath := fmt.Sprintf("%s.iter_%d", cfg.Path, iter)
	defer os.Remove(iterDBPath)

	tmpFile, err := os.CreateTemp("", "torture_import_*.txt")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())

	totalOps := 150 + r.Intn(200)
	chunkSize := 50
	expectedMap := make(map[string]string)

	for i := 0; i < totalOps; i++ {
		k := fmt.Sprintf("imp_k_%d_%04d", iter, i)
		v := fmt.Sprintf("imp_v_%d_%04d", iter, i)
		fmt.Fprintf(tmpFile, "put %s %s\n", k, v)
		expectedMap[k] = v
	}
	tmpFile.Close()

	// Launch kvdb import asynchronously and SIGKILL at random ms
	importCmd := exec.Command(cfg.KVDBPath, "import", iterDBPath, tmpFile.Name(), "--chunk", fmt.Sprintf("%d", chunkSize))
	if err := importCmd.Start(); err != nil {
		return err
	}

	killDelay := time.Duration(r.Intn(15)) * time.Millisecond
	time.Sleep(killDelay)

	if importCmd.Process != nil {
		_ = importCmd.Process.Kill()
		_ = importCmd.Wait()
	}

	// Reopen and verify integrity
	outCheck, err := exec.Command(cfg.KVDBPath, "check", iterDBPath).CombinedOutput()
	if err != nil || !strings.Contains(string(outCheck), "OK") {
		return fmt.Errorf("check failed after killed import: %v, out: %s", err, string(outCheck))
	}

	// Dump visible keys
	outDump, err := exec.Command(cfg.KVDBPath, "dump", iterDBPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("dump failed after killed import: %v, out: %s", err, string(outDump))
	}

	actual := make(map[string]string)
	dumpScanner := bufio.NewScanner(strings.NewReader(string(outDump)))
	for dumpScanner.Scan() {
		parts := strings.SplitN(dumpScanner.Text(), "=", 2)
		if len(parts) == 2 {
			actual[parts[0]] = parts[1]
		}
	}

	// Verify only whole chunks are present
	if len(actual) != totalOps && len(actual)%chunkSize != 0 {
		return fmt.Errorf("batch atomicity violation: visible keys count %d is not a multiple of chunkSize %d", len(actual), chunkSize)
	}

	return nil
}
