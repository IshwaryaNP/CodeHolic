# How I Would Break This (BREAKING.md)

## 1. Crash Points Table

| # | Exact Location / Function | State on Disk if Process Dies Here | What `open()` / `Recover()` Does About It |
|---|---------------------------|-----------------------------------|-------------------------------------------|
| 1 | `src/ops.go:39` (during `file.Write` of single Put) | Partial record bytes written to disk or OS buffer cache (torn write). | `DecodeHeader` or length check detects incomplete record; file truncated back to previous record boundary. |
| 2 | `src/ops.go:49` (after `file.Write`, before `file.Sync`) | Full record bytes exist in OS page cache but not yet forced to physical platters/NAND. | If lost on power cut/kernel death, treated as unwritten. If flushed before process death, CRC passes and record is recovered. |
| 3 | `src/ops.go:55` (after `file.Sync`, before `index.Set` & return) | Complete, valid record with CRC32 is durable on disk. Process dies before caller receives ACK. | Recovery scans record, validates CRC, and updates index. Key is present after restart (durable promise preserved). |
| 4 | `src/batch.go:160` (during `file.Write` of contiguous batch) | Incomplete batch bytes on disk (contains `BatchBegin` and some records, but no `BatchCommit`). | Recovery flags `inBatch=true`, buffers mutations in RAM, reaches EOF without `BatchCommit`, and truncates file back to `batchBeginOffset`. |
| 5 | `src/batch.go:171` (after `file.Write`, before `file.Sync` of batch) | Batch written to cache. If killed, commit marker may not be durable. | If commit marker did not reach disk, entire batch is rolled back to `batchBeginOffset`. If reached, fully recovered. |
| 6 | `src/compaction.go:52` (creating `<path>.tmp`) | Zero-byte or partially written `<path>.tmp` file created on disk. | Recovery removes `<path>.tmp` before opening authoritative database file. |
| 7 | `src/compaction.go:135` (streaming live records to `.tmp`) | Active file has all original data; `.tmp` has partial subset of compacted records. | Cleanup removes `<path>.tmp`; original authoritative `.db` remains active and uncorrupted. |
| 8 | `src/compaction.go:157` (syncing `.tmp` file) | Complete compacted data written to `.tmp`, but sync incomplete. | Temporary file discarded on recovery; active `.db` unaffected. |
| 9 | `src/compaction.go:167` (after closing `db.file`, before `os.Rename`) | Both `.db` and `.tmp` are closed. Data is complete in `.tmp`, old data in `.db`. | On startup without rename, `.db` remains authoritative; stale `.tmp` cleaned up. |
| 10 | `src/compaction.go:183` (during `os.Rename(tmp, path)`) | Filesystem atomic directory swap in progress. | Filesystems (NTFS/ext4/APFS) guarantee rename is atomic; either old `.db` or new `.db` exists. |
| 11 | `src/recovery.go:187` (during `Truncate(validEnd)`) | Truncate system call interrupted mid-operation. | Next recovery re-runs sequential scan from byte 0, detects truncated tail again, and issues `Truncate(validEnd)` idempotently. |
| 12 | `src/recovery.go:190` (after `Truncate`, before `file.Sync`) | Truncation metadata modified in buffer cache but not synced. | Next recovery simply re-evaluates length and completes truncation. |

---

## 2. The Ordering Argument

Our architecture relies on an uncompromising write-ordering discipline:

1. **Sequential Append**: Records are strictly serialized via a global mutex (`db.mu`). In batches, `BatchBegin`, all operations, and `BatchCommit` are encoded into a single contiguous slice before writing.
2. **Synchronous fsync Before ACK**: We never return success or emit an `ACK` token until `db.file.Sync()` (`FlushFileBuffers` on Windows, `fsync` on POSIX) returns successfully. A crash prior to `Sync()` means the caller received no ACK, and any partially written data will be rejected by the IEEE CRC32 check on recovery.
3. **Compaction Invariant**:
   - Compaction streams live records to a separate `<path>.tmp` file.
   - The `.tmp` file is explicitly synced via `tmpFile.Sync()` and closed before replacing the active database.
   - The active file handle is closed (essential on Windows to release file locks).
   - Atomic replacement is performed via `os.Rename(tmpPath, dbPath)`.
   - On POSIX systems, `syncDir()` syncs the parent directory descriptor to guarantee directory entry persistence.
   - If a crash occurs at any moment before `os.Rename` succeeds, the startup recovery routine removes `<path>.tmp`, preserving the original file as authoritative.

---

## 3. Known Weaknesses

1. **Recovery is $O(\text{log size})$ without Hint Files**:
   Although our 1MB buffered I/O scans 1,000,000 records in ~0.64 seconds, a database with tens of millions of records or gigabytes of uncompacted stale logs will eventually exceed the 2-second startup limit. Production Bitcask architectures address this by generating `.hint` files during compaction containing only `[Header][Key][Offset]`.
2. **In-Memory RAM Footprint for Key Index**:
   All active keys must reside in RAM. With 1,000,000 keys of average length 32 bytes, the in-memory Go hash map consumes approximately 80–120MB of memory. While well within limits for modern laptops, it prevents scaling to hundreds of millions of keys on memory-constrained systems.
3. **Windows Directory Metadata Fsync**:
   On Windows, directory handles cannot be opened with `os.OpenFile` for `fsync` (`FlushFileBuffers`), resulting in an `Access Denied` error. While NTFS journaling guarantees file metadata consistency across power loss, directory metadata updates cannot be explicitly flushed via standard user-space APIs.

---

## 4. What We Tried That Failed

- **Direct Truncate on `O_APPEND` Files (Windows)**:
  During early failure-injection testing, calling `Truncate()` on files opened with `os.O_APPEND` failed on Windows with `Access is denied`. We discovered that Windows locks file pointers for append-only descriptors. In production recovery, the file is opened without `O_APPEND` during the initial scan/truncate phase, and reopened with `O_APPEND` only after the log tail is clean.
- **Race Detection with Binaries in `%TEMP%`**:
  Windows Defender aggressively blocks or delays binaries compiled with `-race` when executed from the user temp directory (`%TEMP%`), causing tests to fail with access errors. We resolved this by compiling test and torture binaries directly into the workspace folder.
- **Interleaved Stdin/Stdout Buffering in Serve Mode**:
  Initial versions of `kvdb serve` did not explicitly call `os.Stdout.Sync()` after emitting `ACK <op-id>`. When concurrent writers sent high-throughput requests, ACKs were buffered in the runtime output buffer and never flushed to the torture tool before process termination, causing the torture oracle to report false durability failures. Adding explicit stdout flushes eliminated this bug.
- **Concurrent Writer In-Flight Overwrite Oracle Mismatch**:
  With 2+ concurrent torture writers updating the same key concurrently, an in-flight un-ACKed write from Worker A could land on disk after Worker B's write had already been ACKed. The initial torture oracle tracked in-flight state as a single scalar, causing false-positive verification errors on killed processes. Refactoring the oracle to track a per-key multi-value candidate set solved this without compromising Rule 1 (every ACKed write survives) or Rule 4 (no unknown values ever appear).
- **Batch Atomicity Key Count Check on Finished Bulk Imports**:
  In torture `--mode bulk-import`, when an import finished all records in the file before the random kill timer elapsed, checking `len(actual) % chunkSize == 0` failed if the total operations in the test file were not an exact multiple of the chunk size. Checking that the visible set is either an exact multiple of `chunkSize` or matches the entire expected dataset resolved this cleanly.

---

## 5. If We Had 24 More Hours

1. **Bitcask Hint Files (`.hint`)**:
   Implement companion hint files generated during compaction. On startup, recovery could load the hint file directly without reading the log file payloads, reducing 1M-key startup recovery from 640ms to under 50ms.
2. **Segmented / Multi-File Log Rotation**:
   Split the append-only log into immutable segments (e.g. 64MB each), keeping only the latest active segment open for writes. This allows background compaction of older segments without holding a global lock on the active append log.
3. **Direct I/O (`O_DIRECT` / `FILE_FLAG_NO_BUFFERING`)**:
   Bypass OS page cache overhead for writes to provide predictable fsync latency and prevent double-buffering between Go memory and kernel memory.
