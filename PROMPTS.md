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
