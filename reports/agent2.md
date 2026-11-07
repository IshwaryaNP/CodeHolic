# Agent 2 Audit: Serve Protocol & ACK Durability

## Summary
| Spec Requirement | Status | Evidence / Command Output |
|---|---|---|
| Protocol Dispatch (`put`, `del`, `batch <n>`, `compact`) | **PASS** | Valid hex and operations correctly return `ACK <id>` over stdout. |
| Empty Value Token (`-`) | **PASS** | Successfully handles `-` as empty byte slice `[]byte{}` for single ops & batch. |
| Binary Keys/Values (`0x00`, `0x0A`) | **PASS** | Binary payloads round-trip cleanly via hex tokens. |
| Error Handling (`ERR <id> <reason>`) | **PASS** | Malformed hex, unknown ops, missing parameters reject with formatted `ERR` lines. |
| Batch Atomicity on Error | **PASS** | Batch with invalid hex or premature EOF fails completely with `ERR` and commits 0 ops. |
| 1 MB Value (2 MB Hex String) | **PASS** | 4 MB scanner buffer handles 2 MB hex line seamlessly; DB check returns `OK`. |
| Stdout Flush & Concurrency Safety | **PASS** | `stdoutMu.Lock()` and `os.Stdout.Sync()` flush each line immediately. |
| ACK Durability (100 rounds with instant `Process.Kill`) | **PASS** | 100/100 rounds verified: immediately killed upon `ACK`, reopened and verified intact. Zero lost ACKs. |

---

## Detailed Test Logs & Execution Trace

### Test 1: Serve Protocol & Input Validation
Ran commands via stdin stream into `kvdb serve`:
```
1 put 6b657931 76616c31               -> ACK 1
2 put 656d707479 -                    -> ACK 2 (empty value test)
3 del 6b657931                        -> ACK 3
4 put 000a00 010203000a               -> ACK 4 (binary key/val with NULL & LF)
5 batch 2
  put 626174636831 76616c756531
  put 626174636832 -                  -> ACK 5
6 compact                             -> ACK 6
7 put ZZ 1234                         -> ERR 7 bad key hex: encoding/hex: invalid byte: U+005A 'Z'
8 put 1234 ZZ                         -> ERR 8 bad value hex: encoding/hex: invalid byte: U+005A 'Z'
9 put 1234                            -> ERR 9 put requires key and value
10 unknown                            -> ERR 10 unknown op unknown
11 batch 2
  put 6661696c6b6579 76616c
  put ZZ 1234                         -> ERR 11 bad batch key hex: encoding/hex: invalid byte: U+005A 'Z'
12 batch 3
  put 6661696c32 76616c               -> ERR 12 unexpected EOF while reading batch op lines
```
Database integrity check afterwards:
```
kvdb check: OK
kvdb dump:
\x00\n\x00=\x01\x02\x03\x00\n
batch1=value1
batch2=
empty=
```
Note: `failkey` and `fail2` are NOT in the database, verifying that failed batches applied 0 mutations.

### Test 2: Large 1 MB Value (2 MB Hex Line)
- Generated 1,048,576 byte buffer of repeated `0xAA` (2,097,152 hex characters).
- Streamed `100 put <hex_key> <hex_val>` into `kvdb serve`.
- Result: Received `ACK 100`.
- Validation: `kvdb check` returned `OK`.

### Test 3: ACK Durability Across Process Kills
- Executed 100 independent rounds.
- Each round:
  1. Spawned `kvdb serve <db>`.
  2. Issued `put` over stdin.
  3. Waited synchronously for `ACK <id>`.
  4. Immediately invoked Windows `Process.Kill()`.
  5. Opened DB with `kvdb get` to verify key exists and has expected value.
- **Outcome:** `Durability 100/100 rounds PASSED with zero lost ACKs!`

