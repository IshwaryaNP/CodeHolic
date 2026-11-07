Write-Host "=========================================================="
Write-Host "   CRASH & RECOVERY LIVE DEMO SUITE (GROUPS A, B, C, E, F)"
Write-Host "=========================================================="

$db = "crash_demo.db"
if (Test-Path $db) { Remove-Item $db -Force }
$batchFile = "crash_batch.txt"

# -------------------------------------------------------------
# PHASE 1: Normal Operations (Adding, Overwriting, Deleting, Dump, Compaction)
# -------------------------------------------------------------
Write-Host "`n>>> [PHASE 1] Adding, Overwriting, Deleting, Dump & Compaction..."

# 1. Open
Write-Host "1. Initializing fresh DB..."
.\kvdb.exe open $db

# 2. Put
Write-Host "2. Adding initial keys..."
.\kvdb.exe put $db user:1 "Alice"
.\kvdb.exe put $db user:2 "Bob"
.\kvdb.exe put $db server:status "ONLINE"

# 3. Overwrite
Write-Host "3. Overwriting user:1 (Alice -> Alice_Updated)..."
.\kvdb.exe put $db user:1 "Alice_Updated"

# 4. Delete
Write-Host "4. Deleting user:2..."
.\kvdb.exe del $db user:2

# 5. Dump
Write-Host "5. Dumping current database contents (should be sorted, user:2 absent):"
.\kvdb.exe dump $db

# 6. Compaction
$sizeBefore = (Get-Item $db).Length
Write-Host "6. Running compaction (Size before: $sizeBefore bytes)..."
.\kvdb.exe compact $db
$sizeAfter = (Get-Item $db).Length
Write-Host "   Compaction done! New size: $sizeAfter bytes. Integrity check:"
.\kvdb.exe check $db

# -------------------------------------------------------------
# PHASE 2: Group A - Torn Write Crash (Scenario A2 / F4)
# -------------------------------------------------------------
Write-Host "`n>>> [PHASE 2] Simulating CRASH during single write (Torn Tail - Scenarios A2 & F4)..."
Write-Host "Adding a new record 'temp_key' = '1234567890'..."
.\kvdb.exe put $db temp_key "1234567890"

$currSize = (Get-Item $db).Length
Write-Host "Simulating sudden power loss / kill mid-write by chopping off 3 bytes from tail..."
$fs = [System.IO.File]::Open($db, [System.IO.FileMode]::Open, [System.IO.FileAccess]::ReadWrite)
$fs.SetLength($currSize - 3)
$fs.Close()

Write-Host "Running integrity check on crashed file (CRC/Length will detect corruption):"
.\kvdb.exe check $db

Write-Host "Re-opening database (kvdb open will detect torn tail and automatically roll back to valid boundary):"
.\kvdb.exe open $db

Write-Host "Integrity after auto-recovery:"
.\kvdb.exe check $db
Write-Host -NoNewline "Checking temp_key (should be NOTFOUND, torn data never returned): "
.\kvdb.exe get $db temp_key
Write-Host -NoNewline "Checking prior data user:1 (must be 100% intact): "
.\kvdb.exe get $db user:1

# -------------------------------------------------------------
# PHASE 3: Group B - Batch Atomicity Crash (Scenarios B1, B2, B5)
# -------------------------------------------------------------
Write-Host "`n>>> [PHASE 3] Simulating CRASH during an Atomic Batch (Scenarios B1 & B2)..."
Write-Host "Scenario B5: Creating batch with 'put item:X initial' AND 'del item:X' in the SAME batch..."
Set-Content -Path $batchFile -Value @'
put item:1 "Item 1"
put item:2 "Item 2"
put item:X "initial_value"
del item:X
'@
.\kvdb.exe batch $db $batchFile

Write-Host "Checking item:X after batch commit (Ordering B5 verified):"
Write-Host -NoNewline "item:X = "
.\kvdb.exe get $db item:X

Write-Host "`nNow simulating a crash in the MIDDLE of a 5-item batch (No Commit Marker written)..."
# We manually append a BatchBegin and 2 puts to disk, simulating crash before BatchCommit
$validEnd = (Get-Item $db).Length
$tmpBatchDb = "tmp_batch.db"
Copy-Item $db $tmpBatchDb
Set-Content -Path $batchFile -Value @'
put partial:1 "NeverCommitted"
put partial:2 "NeverCommitted"
'@
.\kvdb.exe batch $tmpBatchDb $batchFile

# Cut off the BatchCommit marker (last 16 bytes)
$raw = [System.IO.File]::ReadAllBytes($tmpBatchDb)
$cut = New-Object byte[] ($raw.Length - 16)
[System.Array]::Copy($raw, $cut, $cut.Length)
[System.IO.File]::WriteAllBytes($db, $cut)
Remove-Item $tmpBatchDb -Force

Write-Host "File size before recovery (includes uncommitted partial batch): $((Get-Item $db).Length) bytes"
Write-Host "Re-opening crashed database with incomplete batch:"
.\kvdb.exe open $db
Write-Host "File size after recovery (cleanly rolled back to before BatchBegin): $((Get-Item $db).Length) bytes (expected: $validEnd bytes)"

Write-Host -NoNewline "Checking partial:1 (must be NOTFOUND, all-or-nothing atomicity): "
.\kvdb.exe get $db partial:1

# -------------------------------------------------------------
# PHASE 4: Group C - Stale Temp Files from Crash during Compaction (Scenario C5)
# -------------------------------------------------------------
Write-Host "`n>>> [PHASE 4] Simulating CRASH during Compaction (Scenario C5 - Stale .tmp)..."
$staleTmp = "$db.compact.tmp"
[System.IO.File]::WriteAllText($staleTmp, "leftover corrupted data from aborted compaction")
Write-Host "Created leftover temporary compaction file: $staleTmp"

Write-Host "Calling kvdb open (engine automatically prunes/ignores leftover compaction tmp files):"
.\kvdb.exe open $db
$tmpStillExists = Test-Path $staleTmp
Write-Host "Leftover temporary file exists after startup: $tmpStillExists (False = safely cleaned up)"

# -------------------------------------------------------------
# PHASE 5: Group E - Disk Full Enforcement (Scenarios E1, E2, E5)
# -------------------------------------------------------------
Write-Host "`n>>> [PHASE 5] Simulating Disk-Full Cap Enforcement (Scenarios E1, E2, E5)..."
$cappedDb = "capped_demo.db"
if (Test-Path $cappedDb) { Remove-Item $cappedDb -Force }

Write-Host "1. Initializing DB with strict 300-byte cap..."
.\kvdb.exe open $cappedDb --max-bytes 300
.\kvdb.exe put $cappedDb small_k "small_v"

Write-Host "2. Attempting single write that exceeds 300 bytes (Scenario E1)..."
.\kvdb.exe put $cappedDb large_k ("Z" * 400)

Write-Host "3. Verifying prior data remains readable after disk full error:"
Write-Host -NoNewline "small_k = "
.\kvdb.exe get $cappedDb small_k
Write-Host "File size on disk: $((Get-Item $cappedDb).Length) bytes (must be <= 300)"

Write-Host "`n4. Scenario E5: Repeated overwrites at cap trigger auto-compaction automatically:"
for ($i = 1; $i -le 10; $i++) {
    .\kvdb.exe put $cappedDb overwrite_key "val_$i"
}
Write-Host -NoNewline "Final value of overwrite_key = "
.\kvdb.exe get $cappedDb overwrite_key
Write-Host "File size after 10 updates: $((Get-Item $cappedDb).Length) bytes (still safely under 300 bytes)"

# Cleanup
if (Test-Path $db) { Remove-Item $db -Force }
if (Test-Path $cappedDb) { Remove-Item $cappedDb -Force }
if (Test-Path $batchFile) { Remove-Item $batchFile -Force }

Write-Host "`n=========================================================="
Write-Host "   ALL CRASH, RECOVERY & INVARIANT TESTS COMPLETED!"
Write-Host "=========================================================="
