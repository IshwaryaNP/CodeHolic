# AI Prompt Log (PROMPTS.md)

This document contains a chronological record of prompts and technical inquiries directed to AI tools during the 24-hour engineering hackathon.

---

### Prompt 1: 2026-11-06 ~21:00 (Claude 3.5 Sonnet / Antigravity)
> "ROLE: Implement Member 1 of an append-only key-value store (Bitcask-style) in Go standard library. We need the record format with fixed 15-byte header, CRC32 covering header[4:15] + key + value, sequential append Put/Get/Delete, and atomic batch writer with BatchBegin and BatchCommit markers. Ensure zero-allocation serialization on writes."

### Prompt 2: 2026-11-06 ~22:15 (Claude 3.5 Sonnet / Antigravity)
> "ROLE: Implement Member 2 of the Go Bitcask key-value store. Provide the thread-safe Index, sequential recovery that reconstructs index from 1M keys under 2 seconds, rolls back uncommitted batches, truncates torn tails, and performs online compaction with atomic file replacement."

### Prompt 3: 2026-11-06 ~22:50 (Claude 3.5 Sonnet / Antigravity)
> "During compaction on Windows, calling os.Rename on an open file fails with 'The process cannot access the file because it is being used by another process'. Also os.Truncate fails with Access Denied when opened with O_APPEND. How should we order file closing and reopening to be safe across Windows and Linux?"

### Prompt 4: 2026-11-06 ~23:20 (Claude 3.5 Sonnet / Antigravity)
> "When running 'go test -race' on Windows with CGO enabled, Windows Defender intermittently blocks the compiled test executable generated in %TEMP%. How can we compile and run the race detection test binary reliably from the local workspace directory?"

### Prompt 5: 2026-11-06 ~23:30 (GPT-OSS 120B / Gemini Flash)
> "Build a small local web UI that visualizes this append-only key-value store with vanilla HTML/CSS/JS embedded using go:embed. Include 3 panels: Operate (Put/Get/Del/Batch), Index (RAM), and File (disk records with CRC checks and dead space markers). Add endpoints to simulate torn writes and uncommitted batch crashes."

### Prompt 6: 2026-11-07 ~00:15 (Gemini 3.8 Flash / Antigravity)
> "Implement the mandatory hackathon CLI wrapper (`kvdb`) with commands: open, put, get, del, batch, dump, check, and serve (with protocol ACK <op-id>). Then build the torture tool deliverable that executes concurrent writers, random kills via SIGKILL, keeps an independent oracle, and tests targeted modes (large-values, compaction, recovery-crash)."

### Prompt 7: 2026-11-07 ~00:35 (Gemini 3.8 Flash / Antigravity)
> "In the torture tool, two concurrent writers caused a false-positive oracle mismatch because one worker's in-flight un-ACKed write was committing after another worker's ACKed write. How should the torture oracle structure in-flight tracking so it strictly enforces the invariant: Rule 1 (every ACKed write survives) and Rule 4 (no unknown values appear)?"


### Prompt 8: 2026-11-07 ~01:12 (Gemini 3.8 Flash / Antigravity)
> "You are working on an existing Go (stdlib only) Bitcask-style KV store in this repo (src/, cmd/kvdb, cmd/torture, cmd/ui, docs: README/ARCHITECTURE/BREAKING/PROMPTS.md). It must satisfy the 'Kill It and Prove It' spec: after kill -9 at any instant, every ACKed op is present, batches are all-or-nothing, no garbage values, open() always succeeds, recovery <2s at 1M keys. First READ the code, then work through the steps below in order. Do not rewrite working parts. After each step run `go test -race ./...` and make a separate commit with a meaningful message. Do not use any third-party libraries.
1. DISK FULL: add max_bytes to Open() and `kvdb open|serve --max-bytes N`. Before any append, if size+record > cap, return a clear ErrDiskFull without writing. Reserve headroom so deletes and compaction still work at the cap (E5 must recover without manual intervention). Batches check total size up front (E2).
2. WRITE ROLLBACK: on any write/sync error in a live process, Truncate the file back to the pre-write offset and fsync, so no orphan BatchBegin or partial record remains. Add a test that injects a write error mid-batch.
3. BATCH: write BatchBegin + ops + BatchCommit as ONE buffer, ONE Write, ONE Sync. Update the index only after the sync, holding the write lock, so readers see before-or-after, never in between (D4). Enforce limits: key 1-1024 B, value <= 1 MB, batch <= 10,000 ops. Ops in a batch apply in order (put X then del X => X absent); document this for B5.
4. RECOVERY HARDENING: reject records whose lengths exceed limits or the remaining file size before allocating. Truncate + fsync the torn tail. Make recovery idempotent. Delete stale .tmp files. Re-benchmark 1M-key recovery (<2s).
5. COMPACTION: block writers during the swap. Order: fsync tmp, close, rename, then fsync the parent directory (skip on Windows, note it in docs). On any failure (incl. disk full) delete the tmp and keep the old file valid. Expose `compact` in `kvdb serve` (prints ACK). Test that the file shrinks after 1M puts + 1M deletes.
6. BULK IMPORT (new feature):
   a. CLI: `kvdb import <path> <file> [--format lines|csv|jsonl] [--chunk N] [--max-bytes N]`
   b. Library: add db.BulkImport(reader, opts) used by the CLI and the UI.
   c. serve mode: support `import <file>` on stdin that emits one ACK per chunk.
   d. Web UI (cmd/ui): add an 'Import data' panel with drag-and-drop / file picker, format dropdown, chunk-size input, a progress bar fed by streaming progress (SSE or polling), and a final result summary. Use multipart upload streaming, no whole-file buffering, enforce a sane upload size limit.
   e. Tests: import 1M lines and verify counts via dump; kill the importer mid-run (in a test or via torture) and verify that only whole chunks are visible; import into a small --max-bytes DB and verify the clean error.
   f. Torture: add `--mode bulk-import`. It generates a file with a seeded RNG, runs `kvdb import`, SIGKILLs at a random ms, then reopens and checks via the oracle that the visible keys are exactly a prefix of whole chunks, check says OK, and resuming with --start-line finishes with the full dataset.
7. TORTURE TOOL: ensure modes: default, large-values (1MB), large-batches (10k ops), recovery-crash, compaction, disk-full, bulk-import. Requirements: >= 2 concurrent writers, an independent oracle file (log 'sent' before and 'acked' after each op), random-ms SIGKILL/TerminateProcess during operations, batch all-or-nothing check, unknown-value check, --seed reproducibility, one-line summary per iteration, stop on first failure with expected vs found and op IDs. Run 100+ iterations per mode with seeds not used before, and fix every engine bug found.
8. BUILD: one documented build command (script) that produces ./kvdb and ./torture (kvdb.exe/torture.exe on Windows). Verify from a clean clone. Verify all 8 required CLI commands (open, put, get, del, batch, dump, check, serve) match the spec output (NOTFOUND, ACK <op-id> only after fsync, etc.). `import` and `compact` are extras.
9. DOCS: update ARCHITECTURE.md (all 32 rows A1-F4, each with the real file/function and the torture mode+seed that verifies it, or 'not verified'), BREAKING.md (every crash point must match the actual code, plus the failed attempts found in step 7), README (on-disk format, recovery algorithm, import usage with a sample file for each format). Append every prompt I send you to PROMPTS.md verbatim."
