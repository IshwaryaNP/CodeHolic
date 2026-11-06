# CodeHolic

A high-performance append-only key-value store (Bitcask-style) built in Go with standard library only.

## Features
- **Append-only log architecture** with 15-byte fixed headers and incremental CRC32 (IEEE) checksumming.
- **Zero-allocation serialization** for active writes.
- **Crash consistency**: automatic torn-tail recovery and uncommitted batch rollback.
- **Atomic batches**: multi-key transactions committed with `BatchBegin` and `BatchCommit`.
- **Online compaction**: space reclamation with atomic, cross-platform file replacement.
- **Interactive CLI**: embedded REPL for testing `put`, `get`, `del`, `compact`, and `reopen`.

## Quick Start
```bash
go run main.go
```

