# Agent 4 Audit: Compaction & Disk-Full (Scenarios Groups C3-C5, E1-E5)

## Summary
| Spec Requirement / Scenario | Description | Status | Evidence / Metrics |
|---|---|---|---|
| **Space Reclamation** | Manual compaction shrinks file with deleted/overwritten keys | **PASS** | File shrank from **22,355 bytes down to 155 bytes** (>99% reclamation). Live keys intact; `kvdb check` passed with `OK`. |
| **Scenario E1 / E2** | Single put or batch exceeding `--max-bytes` cap | **PASS** | Write rejected with `ERR 9 disk full: write exceeds maximum permitted database file size`. Size remained exactly `200 <= 200` bytes. |
| **Scenario E3** | Process kill at cap, reopen, verify ACKed data | **PASS** | Verified via test `TestPhase3_E3_ProcessKillAtCapReopen` and serve durability run. |
| **Scenario E4** | Compaction failure injection across all steps (`tmp_write`, `tmp_sync`, `rename`, `reopen`) | **PASS** | Pre-rename failures fall back to original file; authoritative swap on post-rename failure; index remains consistent. |
| **Scenario E5** | Repeated overwrites at cap succeed via auto-compaction | **PASS** | 50 consecutive single-key overwrites executed under 1000-byte cap. Auto-compaction reclaimed dead space on-the-fly; final size was `540 <= 1000` bytes. |
| **Max-Bytes Invariant** | File size on disk strictly `<= maxBytes` after disk full | **PASS** | Disk size verified on disk directly after rejection: never exceeds configured cap. |

---

## Detailed Execution Logs

### 1. Manual Compaction Space Reclamation
- Database populated with 500 puts and 500 deletes, followed by 5 live records.
- Pre-compaction size: **22,355 bytes**.
- Ran `kvdb compact <db>`.
- Post-compaction size: **155 bytes**.
- Verified all live records via `kvdb get` and validated database integrity via `kvdb check`:
  ```
  live_0 = live_val_0
  check: OK
  ```

### 2. Disk-Full Enforcement (`serve --max-bytes 200`)
- Streamed puts until capacity was reached:
  - Operations 1 through 8 succeeded with `ACK 1` .. `ACK 8`.
  - Operation 9 exceeded the 200-byte cap and was rejected:
    ```
    ERR 9 disk full: write exceeds maximum permitted database file size
    ```
- Verified file size on disk immediately after: **200 bytes** (exactly `<= maxBytes`).

### 3. Scenario E5: Auto-Compaction on Repeated Overwrites at Cap
- Database configured with `--max-bytes 1000`.
- Issued 50 consecutive updates to the same key (`key`) in `serve`.
- Without auto-compaction, 50 operations would have consumed over 1,500 bytes and failed with `ErrDiskFull`.
- **Result:** All 50/50 operations succeeded (`ACK 1` .. `ACK 50`). Auto-compaction automatically triggered to reclaim obsolete versions of the key.
- Final file size on disk: **540 bytes** (well below the 1,000-byte limit).
- Verified final value: `val_cycle_50`.

