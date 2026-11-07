# Agent 5 Audit: Concurrency & Contention

## Summary
| Spec Requirement / Scenario | Description | Status | Evidence / Code Inspection |
|---|---|---|---|
| **RWMutex Protection** | All in-memory index access and DB metadata protected by `db.mu` | **PASS** | `db.mu.Lock()` on writes; `db.mu.RLock()` on reads in [`src/ops.go`](file:///c:/Users/Ishwarya/OneDrive/Desktop/S/Member1/src/ops.go). |
| **Write Serialization** | Single active writer for log appends and write offset progression | **PASS** | `Put`, `Delete`, and `Write` hold exclusive `db.mu.Lock()` during record encoding, file `WriteAt`, and `Sync()`. |
| **Concurrent Reads** | Non-blocking parallel reads using shared read lock | **PASS** | `db.Get` takes `db.mu.RLock()` to look up file offset and length, reads payload via `file.ReadAt` without blocking other readers. |
| **Serve Concurrency** | Non-blocking command dispatch in server loop | **PASS** | Operations in `kvdb serve` are dispatched in independent goroutines (`go func(...)`), with output synchronization protected by `stdoutMu`. |
| **Pipelined Writes & ACK Durability** | Multiple concurrent writers pipeline ops without interleaving file corruption | **PASS** | 50 concurrent goroutines executed simultaneous puts/deletes/gets; zero corruption; `kvdb check` returned `OK`. |

---

## Code Inspection & Invariant Analysis

### 1. Reader/Writer Synchronization ([`src/ops.go`](file:///c:/Users/Ishwarya/OneDrive/Desktop/S/Member1/src/ops.go), [`src/batch.go`](file:///c:/Users/Ishwarya/OneDrive/Desktop/S/Member1/src/batch.go))
- **Exclusive Lock on Write (`Put`, `Delete`):**
  ```go
  db.mu.Lock()
  defer db.mu.Unlock()
  ```
  This ensures:
  1. Record offset capture (`writeOffset`) is completely linear and monotonic.
  2. The shared buffer `db.encBuf` has zero concurrent access or memory races.
  3. Tail appends (`db.file.WriteAt(...)`) are strictly serialized.
  4. The in-memory map index is updated atomically alongside the on-disk state.

- **Concurrent Shared Lock on Read (`Get`):**
  ```go
  db.mu.RLock()
  rec, ok := db.index.Get(key)
  db.mu.RUnlock()
  ```
  The read lock is held only to resolve the file offset and value length from the in-memory index. The subsequent `ReadAt` operation can run concurrently with other readers, providing maximum read throughput.

### 2. Compaction Lock Transitions ([`src/compaction.go`](file:///c:/Users/Ishwarya/OneDrive/Desktop/S/Member1/src/compaction.go))
- During compaction:
  1. Reads a snapshot of the live keys under `db.mu.RLock()`.
  2. Writes live records into a temporary file (`.compact.tmp`) without blocking read operations.
  3. Acquires exclusive `db.mu.Lock()` only for the atomic rename and index swap, minimizing write stall time.

