package main

import (
	"bufio"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Iterations int
	Seed       int64
	Mode       string
	Path       string
	KVDBPath   string
}

// Oracle tracks the state of the database by recording every operation that was ACKed.
// It also tracks all operations that were attempted (to verify Rule 4: no unknown values ever appear).
type Oracle struct {
	mu           sync.Mutex
	acknowledged map[string]string          // key -> latest value committed
	history      map[string]map[string]bool // key -> set of valid written values
}

func NewOracle() *Oracle {
	return &Oracle{
		acknowledged: make(map[string]string),
		history:      make(map[string]map[string]bool),
	}
}

func (o *Oracle) RegisterAttempt(key, val string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	set := o.history[key]
	if set == nil {
		set = make(map[string]bool)
		o.history[key] = set
	}
	set[val] = true
}

func (o *Oracle) ApplyACK(key, val string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if val == "" {
		delete(o.acknowledged, key)
	} else {
		o.acknowledged[key] = val
	}
}

func (o *Oracle) IsValidValue(key, val string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	set := o.history[key]
	if set == nil {
		return false
	}
	return set[val]
}

func (o *Oracle) Snapshot() map[string]string {
	o.mu.Lock()
	defer o.mu.Unlock()
	cp := make(map[string]string, len(o.acknowledged))
	for k, v := range o.acknowledged {
		cp[k] = v
	}
	return cp
}

func main() {
	iterations := flag.Int("iterations", 500, "number of kill/restart iterations")
	seed := flag.Int64("seed", 0, "random seed (mandatory for reproducibility)")
	mode := flag.String("mode", "default", "mode: default | large-values | compaction | recovery-crash | large-batches")
	dbPath := flag.String("db", "", "path to test db")
	kvdbBin := flag.String("kvdb", "./kvdb.exe", "path to kvdb binary")
	flag.Parse()

	if *seed == 0 {
		*seed = time.Now().UnixNano()
	}

	if *dbPath == "" {
		tmpDir, err := os.MkdirTemp("", "torture_*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed creating temp dir: %v\n", err)
			os.Exit(1)
		}
		defer os.RemoveAll(tmpDir)
		*dbPath = filepath.Join(tmpDir, "torture.db")
	}

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
	oracle := NewOracle()

	passed := 0
	for iter := 1; iter <= cfg.Iterations; iter++ {
		iterSeed := r.Int63()
		err := runIteration(iter, iterSeed, cfg, oracle)
		if err != nil {
			fmt.Printf("\n[FAILURE] Iteration %d failed with seed %d: %v\n", iter, cfg.Seed, err)
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

	cmd := exec.Command(cfg.KVDBPath, "serve", cfg.Path)
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
	stdinWriter := bufio.NewWriter(stdin)

	// Global order tracking
	var seqMu sync.Mutex
	var opSeq int64

	// inFlight tracks operations sent to kvdb that have not yet had their ACK processed
	var inFlightMu sync.Mutex
	inFlight := make(map[string]string) // key -> value attempted

	stopWriters := make(chan struct{})

	type opReq struct {
		opID     string
		line     string
		key      string
		val      string
		isBatch  bool
		batchMap map[string]string
		resp     chan bool
	}

	opQueue := make(chan opReq, 64)

	// Serialized stdin dispatcher: sends to kvdb, reads ACK, then confirms
	go func() {
		for {
			select {
			case <-stopWriters:
				return
			case req := <-opQueue:
				inFlightMu.Lock()
				if req.isBatch {
					for bk, bv := range req.batchMap {
						inFlight[bk] = bv
					}
				} else {
					inFlight[req.key] = req.val
				}
				inFlightMu.Unlock()

				_, wErr := stdinWriter.WriteString(req.line)
				if wErr == nil {
					_ = stdinWriter.Flush()
				}
				if wErr != nil {
					req.resp <- false
					return
				}

				if stdoutScanner.Scan() {
					txt := stdoutScanner.Text()
					if txt == "ACK "+req.opID {
						inFlightMu.Lock()
						if req.isBatch {
							for bk, bv := range req.batchMap {
								delete(inFlight, bk)
								oracle.ApplyACK(bk, bv)
							}
						} else {
							delete(inFlight, req.key)
							oracle.ApplyACK(req.key, req.val)
						}
						inFlightMu.Unlock()
						req.resp <- true
					} else {
						req.resp <- false
					}
				} else {
					req.resp <- false
					return
				}
			}
		}
	}()

	// 2 concurrent writers producing random operations
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

				seqMu.Lock()
				opSeq++
				id := opSeq
				seqMu.Unlock()

				opID := fmt.Sprintf("w%d_%d", workerID, id)
				keyNum := r.Intn(100)
				key := fmt.Sprintf("k_%d", keyNum)

				var req opReq
				req.opID = opID
				req.resp = make(chan bool, 1)

				if cfg.Mode == "large-values" && r.Float32() < 0.25 {
					valSize := 256 * 1024
					if r.Float32() < 0.5 {
						valSize = 512 * 1024
					}
					val := strings.Repeat(fmt.Sprintf("%d", r.Intn(10)), valSize)
					req.line = fmt.Sprintf("%s put %s %s\n", opID, key, val)
					req.key = key
					req.val = val
					oracle.RegisterAttempt(key, val)
				} else if cfg.Mode == "large-batches" && r.Float32() < 0.2 {
					batchOpsCount := 50 + r.Intn(50)
					bMap := make(map[string]string)
					bFile, err := os.CreateTemp("", "batch_*.txt")
					if err == nil {
						for b := 0; b < batchOpsCount; b++ {
							bk := fmt.Sprintf("bk_%d", r.Intn(50))
							bv := fmt.Sprintf("bv_%d", r.Intn(10000))
							fmt.Fprintf(bFile, "put %s %s\n", bk, bv)
							bMap[bk] = bv
							oracle.RegisterAttempt(bk, bv)
						}
						bFile.Close()
						req.line = fmt.Sprintf("%s batch %s\n", opID, bFile.Name())
						req.isBatch = true
						req.batchMap = bMap
						defer os.Remove(bFile.Name())
					}
				} else if cfg.Mode == "compaction" && r.Float32() < 0.2 {
					req.line = fmt.Sprintf("%s compact\n", opID)
				} else {
					if r.Float32() < 0.85 {
						val := fmt.Sprintf("val_%d_%d", workerID, r.Intn(100000))
						req.line = fmt.Sprintf("%s put %s %s\n", opID, key, val)
						req.key = key
						req.val = val
						oracle.RegisterAttempt(key, val)
					} else {
						req.line = fmt.Sprintf("%s del %s\n", opID, key)
						req.key = key
						req.val = ""
					}
				}

				if req.line != "" {
					select {
					case opQueue <- req:
						select {
						case <-req.resp:
						case <-time.After(250 * time.Millisecond):
						}
					case <-stopWriters:
						return
					}
				}

				time.Sleep(time.Duration(r.Intn(3)+1) * time.Millisecond)
			}
		}(w)
	}

	// Genuinely random delay before SIGKILL
	killDelay := time.Duration(15+r.Intn(60)) * time.Millisecond
	time.Sleep(killDelay)

	close(stopWriters)

	// Immediate SIGKILL
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
	wg.Wait()

	// Recovery-crash mode: simulate a kill while recovery (open) is executing
	if cfg.Mode == "recovery-crash" {
		recCmd := exec.Command(cfg.KVDBPath, "open", cfg.Path)
		_ = recCmd.Start()
		time.Sleep(time.Duration(r.Intn(8)+1) * time.Millisecond)
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
	dumpScanner.Buffer(make([]byte, 2*1024*1024), 2*1024*1024)
	for dumpScanner.Scan() {
		line := dumpScanner.Text()
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			actual[parts[0]] = parts[1]
		}
	}

	// Invariant Checks:
	// Rule 4: No key ever holds a value that was never written to it
	for actK, actV := range actual {
		if !oracle.IsValidValue(actK, actV) {
			return fmt.Errorf("invariant violation (Rule 4): key '%s' holds unknown value '%s'", actK, actV)
		}
	}

	// Rule 1: Every ACKed operation must survive unless overwritten by an in-flight operation
	inFlightMu.Lock()
	inFlightCopy := make(map[string]string, len(inFlight))
	for k, v := range inFlight {
		inFlightCopy[k] = v
	}
	inFlightMu.Unlock()

	acked := oracle.Snapshot()
	for expK, expV := range acked {
		actV, ok := actual[expK]
		if !ok {
			if inflightVal, hasInFlight := inFlightCopy[expK]; !hasInFlight || inflightVal != "" {
				return fmt.Errorf("durability violation (Rule 1): ACKed key '%s' is missing from database", expK)
			}
		} else if actV != expV {
			if inflightVal, hasInFlight := inFlightCopy[expK]; !hasInFlight || inflightVal != actV {
				return fmt.Errorf("durability violation (Rule 1): ACKed key '%s' holds '%s', expected '%s'", expK, actV, expV)
			}
		}
	}

	return nil
}
