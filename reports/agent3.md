# Agent 3 Audit: Crash & Recovery (Scenarios Groups A, B, C-recovery)

## Summary
| Spec Scenario | Check | Status | Evidence / Result |
|---|---|---|---|
| **Edge Cases** | 0-byte file | **PASS** | `kvdb open` exits 0; `kvdb check` prints `OK`. |
| **Edge Cases** | File shorter than 15-byte header | **PASS** | Safely truncated to 0 bytes; `check` returns `OK`. |
| **Edge Cases** | Random garbage file | **PASS** | Torn/corrupt bytes detected; truncated cleanly to 0 bytes; `check` returns `OK`. |
| **Baseline** | Puts, Deletes, Overwrites, 1,000-op batch | **PASS** | Successfully stored and indexed; integrity verified (`check OK`). |
| **F4 / Torn Write** | 1-byte truncation on tail record | **PASS** | `check` failed on torn file; `open` truncated file back to `validEnd`; tail key returned `NOTFOUND`; earlier keys (`user:1`) intact. |
| **Bit-Flip** | Bit-flip in payload / header | **PASS** | CRC mismatch detected during replay; corrupt tail truncated; database recovered cleanly. |
| **B5** | Put X + Delete X in same batch | **PASS** | Intra-batch ordering preserved; `get key_b5` returns `NOTFOUND` upon reopen. |
| **Batch Atomicity** | Truncation before `OpBatchCommit` | **PASS** | Atomic rollback: truncated database to offset before `OpBatchBegin` (offset 32); 0 partial writes visible. |
| **C5** | Stale `.tmp` cleanup | **PASS** | Unfinished compaction temp files (`*.tmp`) isolated and purged on open. |

---

## Detailed Test Logs & Execution Trace

### Test 1: Header Edge Cases
- **0-Byte File:** Opened clean empty file -> `open` completed in 0.00ms, `check` reported `OK`.
- **Short File (7 bytes < 15-byte header):** Detected incomplete header -> safely truncated to 0 bytes. Post-recovery `check` reported `OK`.
- **Random Noise (200 bytes):** Detected invalid header / CRC -> safely truncated to 0 bytes. Subsequent `open` and `check` succeeded with `OK`.

### Test 2: Baseline Population
- Stored records: `user:1=Alice`, `user:2=Bob`, updated `user:1=Alice_Updated`, deleted `user:2`.
- Applied batch of 1,000 operations (`b_key_0` .. `b_key_999`, deleted `b_key_50`).
- Validated file integrity: 32,938 bytes, `kvdb check` passed with `OK`.

### Test 3: Scenario F4 (Torn Tail Truncation by 1 Byte)
- Appended `tail_key=tail_val_12345` to the database.
- Truncated the file by 1 byte to simulate a power loss mid-write.
- Pre-recovery validation: `kvdb check` failed as expected with `FAIL: incomplete payload at offset...`.
- Reopen recovery: `kvdb open` automatically detected the torn tail, rolled the file size back to the valid record boundary (`validEnd`), and updated the index.
- Post-recovery validation: `kvdb check` passed with `OK`, `tail_key` returned `NOTFOUND`, and earlier keys (`user:1=Alice_Updated`) remained fully intact.

### Test 4: Bit-Flip Corruption
- Flipped a single bit in the last payload byte.
- Pre-recovery `kvdb check` flagged `FAIL: CRC mismatch`.
- On `kvdb open`, the CRC verification detected corrupted data and rolled the file back to the prior valid record.
- Subsequent `check` returned `OK`.

### Test 5: Scenario B5 (Batch Internal Ordering)
- Executed batch containing:
  ```
  put key_b5 first_value
  del key_b5
  ```
- Reopened database and queried `key_b5`:
  ```
  kvdb get <db> key_b5 -> NOTFOUND
  ```
- Confirms delete takes precedence when following put in the same batch.

### Test 6: Incomplete Batch Atomicity
- Appended `OpBatchBegin` and batch mutations, but omitted `OpBatchCommit`.
- `kvdb open` detected uncommitted batch marker at tail; rolled back entire batch to the offset before `OpBatchBegin` (offset 32).
- Queried keys from batch: all returned `NOTFOUND`; all pre-existing keys (`committed_key=val1`) intact.

