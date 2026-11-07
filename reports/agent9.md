# Agent 9 Audit: Demo Readiness & Crash-Scenario Questions

## Demo Workflow Script
The project includes a ready-to-run interactive demo script covering all 8 CLI operations, high-performance concurrency in `serve`, atomic batch operations, and crash recovery.

### Step 1: Initialize Database & Verify Clean Recovery
```powershell
.\kvdb.exe open demo_run.db
# Expected output: Keys: 0, Recovery time: <X>ms
```

### Step 2: Key-Value Storage, Query, and Delete
```powershell
.\kvdb.exe put demo_run.db user:42 "Arthur Dent"
.\kvdb.exe get demo_run.db user:42
# Output: Arthur Dent

.\kvdb.exe del demo_run.db user:42
.\kvdb.exe get demo_run.db user:42
# Output: NOTFOUND
```

### Step 3: Atomic Batch Execution
Create a batch without UTF-8 BOM:
```powershell
Set-Content -Path batch.txt -Value @'
put account:1 1000
put account:2 2500
del account:old
'@

.\kvdb.exe batch demo_run.db batch.txt
.\kvdb.exe dump demo_run.db
```

### Step 4: High-Performance Concurrent Serve
```powershell
# In PowerShell:
$p = Start-Process -FilePath .\kvdb.exe -ArgumentList "serve demo_run.db" -NoNewWindow -PassThru -RedirectStandardInput input.txt -RedirectStandardOutput output.txt
```

### Step 5: Space Reclamation via Compaction
```powershell
.\kvdb.exe compact demo_run.db
.\kvdb.exe check demo_run.db
# Output: OK
```

---

## 10 "Crash-Here" Scenarios & On-Disk State Analysis

| # | Exact Crash Point | In-Flight Operation | On-Disk State After Crash | State After Next `kvdb open` Reopen |
|---|---|---|---|---|
| **1** | Mid-payload write in `Put` (torn write) | Single `put k v` | File has incomplete trailing bytes; CRC check fails. | File truncated to `validEnd`; partial record rolled back; old value intact. |
| **2** | After write, before `fsync` (`SyncAlways`) | Single `put k v` | OS buffer cache may discard unflushed page on hard reboot. | Tail truncated to last durable fsynced offset. No ACK was sent, so no contract violated. |
| **3** | Right after `ACK` printed to stdout | Single `put k v` | `SyncAlways` fsynced before `printACK`; bytes durable on disk. | Reopen restores the key immediately; 100% durable. |
| **4** | Mid-batch write (between ops) | Batch with 500 ops | `OpBatchBegin` written, but no `OpBatchCommit`. | Replay detects uncommitted batch; rolls back file size to offset before `BatchBegin`. 0 partial ops visible. |
| **5** | After `OpBatchCommit` write, before `fsync` | Batch | Commit marker present on disk or partially written. | If CRC passes on commit marker, batch commits; if torn, entire batch rolls back. |
| **6** | During compaction: writing temp file | `compact` | Primary `.db` untouched; `.compact.tmp` partially written. | Startup scans and removes `.compact.tmp`; primary database unaffected. |
| **7** | During compaction: after atomic rename, before dir sync | `compact` | New compacted file authoritative; old file replaced. | Reopen opens new compacted file; index rebuilt; 0 data lost. |
| **8** | At cap: single put exceeding `max-bytes` | Single `put` | `db.writeOffset + len > maxBytes` pre-checked before disk write. | No bytes written to disk; file size strictly `<= maxBytes`. |
| **9** | Replay of 0-byte database file | `open` | 0 bytes on disk. | Replay terminates cleanly with 0 keys; file remains 0 bytes; `check` returns `OK`. |
| **10**| Replay of corrupted file (< 15 bytes) | `open` | Incomplete header (< 15 bytes) on disk. | Replay detects invalid/torn header; truncates file to 0 bytes; DB ready for fresh writes. |

---

## TTL / Expiry Extension Design Document
To extend the engine to support expiring keys (TTL) without breaking append-only Bitcask invariants:
1. **Header Layout Extension:**
   - Add uint64 `ExpiresAtUnixNano` to the record header (0 = never expire). Total header expands from 15 to 23 bytes.
2. **In-Memory Index:**
   - In-memory struct stores `ExpiresAt` alongside `Offset` and `ValLen`.
   - On `Get(key)`: if `time.Now().UnixNano() > entry.ExpiresAt`, return `ErrNotFound`.
3. **Replay Phase:**
   - During startup recovery scan, skip indexing any record whose `ExpiresAt != 0` and is in the past.
4. **Compaction:**
   - In `compactLocked()`, filter out records where `ExpiresAt != 0 && time.Now().UnixNano() > ExpiresAt`, reclaiming disk space automatically.

