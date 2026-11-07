# CodeHolic: Crash-Safe Key-Value Store

An append-only, Bitcask-style embedded key-value storage engine in Go that survives abrupt process termination (`kill -9`) at any instruction, recovers in under 1 second, and provides verifiable crash invariants.

---

## 1. How to Build

A single build command compiles both the engine CLI (`kvdb`) and the torture testing tool (`torture`):

```bash
go build -o kvdb.exe ./cmd/kvdb && go build -o torture.exe ./cmd/torture
```

On Linux / macOS:
```bash
go build -o kvdb ./cmd/kvdb && go build -o torture ./cmd/torture
```

---

## 2. Command-Line Interface (`kvdb`)

All 8 mandatory CLI commands are supported:

```bash
# 1. Open or create database, run recovery, and print recovery time
./kvdb open ./data.db [--max-bytes N]

# 2. Put key-value pair
./kvdb put ./data.db user:101 '{"name":"Alice"}'

# 3. Get key (prints value, or NOTFOUND)
./kvdb get ./data.db user:101

# 4. Delete key
./kvdb del ./data.db user:101

# 5. Atomic batch execution from a text file (one op per line: "put k v" or "del k")
./kvdb batch ./data.db ./batch_ops.txt

# 6. Dump all keys and values, sorted by key
./kvdb dump ./data.db

# 7. Check file integrity (prints OK or failure reason)
./kvdb check ./data.db

# 8. Long-running serve mode (reads ops from stdin, prints ACK <op-id> after fsync)
./kvdb serve ./data.db [--max-bytes N]
```

> **Concurrency & Ordering in `serve`**: Operations read from `stdin` in `serve` are dispatched concurrently into individual worker goroutines. Ordering between single operations is only guaranteed by waiting for the newline-terminated `ACK <id>` before issuing dependent operations. Atomic batches (`batch <n>` or file-based) remain strictly atomic and sequentially framed.
>
> **Compaction Temp Files**: The compaction temporary file (`<path>.tmp`) does **NOT** count against `--max-bytes`. `--max-bytes` strictly bounds only the primary active data log.

---

## 3. On-Disk Binary Format

The authoritative database resides entirely within a single append-only `.db` file.

### Fixed Record Layout (Little-Endian)
```
+---------------+----------------+---------------+---------------+---------------+---------------+---------------+
|  CRC32 (4B)   | TotalLen (4B)  |  OpType (1B)  |  KeyLen (2B)  |  ValLen (4B)  |  Key (NB)     |  Value (MB)   |
+---------------+----------------+---------------+---------------+---------------+---------------+---------------+
0               4                8               9               11              15              15+KeyLen       TotalLen
```

- **Header Size**: 15 bytes.
- **CRC32 (IEEE)**: Computed incrementally over `header[4:15] + key + value`.
- **TotalLen**: Total byte length of the record = `15 + KeyLen + ValLen`.
- **OpType**:
  - `1`: `OpPut`
  - `2`: `OpDelete` (`ValLen = 0`)
  - `3`: `OpBatchBegin` (`KeyLen = 0`, `ValLen = 0`)
  - `4`: `OpBatchCommit` (`KeyLen = 0`, `ValLen = 0`)

---

## 4. Recovery Algorithm (Plain English)

When `open(path)` is called:
1. **Remove Stale Artifacts**: If a `<path>.tmp` file exists from an interrupted compaction, it is removed immediately.
2. **Sequential Scan**: A 1MB buffered reader streams records starting at byte `0`.
3. **Record Validation**:
   - Reads the 15-byte header. If fewer than 15 bytes remain, it is treated as an incomplete torn tail.
   - Decodes and bounds-checks `TotalLen`, `KeyLen`, and `ValLen`.
   - Reads the key and value payload bytes.
   - Verifies the IEEE CRC32 checksum. If checksum fails, scanning halts immediately.
4. **Batch Atomicity**:
   - On `OpBatchBegin`, recovery records the begin offset and buffers pending mutations in memory.
   - On `OpBatchCommit`, all buffered batch mutations are committed to the index, and `validEnd` advances past the commit marker.
   - If the log ends without an `OpBatchCommit`, all buffered operations are discarded, and the log is truncated back to `batchBeginOffset`.
5. **Torn Tail Truncation**:
   - If `validEnd < fileSize`, the file is truncated back to `validEnd` using `file.Truncate()` and flushed with `file.Sync()`.

---

## 5. Running the Torture Tool

The torture tool runs concurrent writers against `kvdb serve`, sends random SIGKILL signals, restarts, runs `check` and `dump`, and compares against an independent oracle.

```bash
# Standard 500-iteration random kill run
./torture --iterations 500 --seed 12345

# Targeted modes (all verified with 100+ iterations)
./torture --iterations 100 --seed 2002 --mode large-values
./torture --iterations 100 --seed 3003 --mode large-batches
./torture --iterations 100 --seed 4004 --mode recovery-crash
./torture --iterations 100 --seed 5005 --mode compaction
./torture --iterations 100 --seed 6006 --mode disk-full
./torture --iterations 100 --seed 7007 --mode bulk-import
```

---

## 6. Bulk Import Usage & Formats

Use `kvdb import <path> <file>` to stream data into the store in atomic batches.

```bash
kvdb import <path> <file> [--format lines|csv|jsonl] [--chunk N] [--max-bytes N] [--on-error skip|abort] [--start-line L]
```

### Sample Files

#### Lines Format (`sample.lines`)
```text
put user:1 Alice
put user:2 Bob
del user:1
put user:3 Charlie
```

#### CSV Format (`sample.csv`)
```csv
user:4,David
user:5,Eve
user:6,"Frank, Jr."
```

#### JSONL Format (`sample.jsonl`)
```json
{"key":"user:7","value":"Grace"}
{"key":"user:8","value":"Heidi"}
{"key":"user:4","op":"del"}
```

---

## 7. Interactive Web Visualizer

To visually inspect the append-only log, RAM index, bulk import streaming, compaction, and crash recoveries in your browser:

```bash
go run ./cmd/ui -addr 127.0.0.1:8080
```
Open **[http://127.0.0.1:8080](http://127.0.0.1:8080)**.
