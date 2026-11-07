$ErrorActionPreference = 'Stop'
$tempDir = Join-Path $env:TEMP ('agent4_audit_' + [System.Guid]::NewGuid().ToString().Substring(0,8))
New-Item -ItemType Directory -Path $tempDir -Force | Out-Null
$kvdb = (Resolve-Path '.\kvdb.exe').Path

Write-Host "Temp dir: $tempDir"

function Run-Kvdb([string[]]$cmdArgs) {
    $p = Start-Process -FilePath $kvdb -ArgumentList $cmdArgs -NoNewWindow -Wait -PassThru -RedirectStandardOutput (Join-Path $tempDir 'out.tmp') -RedirectStandardError (Join-Path $tempDir 'err.tmp')
    $out = Get-Content (Join-Path $tempDir 'out.tmp') -Raw -ErrorAction SilentlyContinue
    $err = Get-Content (Join-Path $tempDir 'err.tmp') -Raw -ErrorAction SilentlyContinue
    return [PSCustomObject]@{ ExitCode = $p.ExitCode; Out = $out; Err = $err }
}

$report = @()

# -------------------------------------------------------------
# Test 1: Space reclamation via Compaction (10,000 puts + 10,000 deletes)
# -------------------------------------------------------------
Write-Host "=== TEST 1: Space Reclamation via Compaction ==="
$dbShrink = Join-Path $tempDir 'shrink.db'
$batchPop = Join-Path $tempDir 'pop.txt'

$sb = [System.Text.StringBuilder]::new()
for ($i = 0; $i -lt 5000; $i++) {
    [void]$sb.AppendLine("put k_$i v_$i")
}
for ($i = 0; $i -lt 5000; $i++) {
    [void]$sb.AppendLine("del k_$i")
}
# Keep only 10 live keys
for ($i = 0; $i -lt 10; $i++) {
    [void]$sb.AppendLine("put live_$i live_val_$i")
}
[System.IO.File]::WriteAllText($batchPop, $sb.ToString())

Run-Kvdb @('batch', $dbShrink, $batchPop) | Out-Null
$sizeBefore = (Get-Item $dbShrink).Length
Write-Host "Size before compaction: $sizeBefore bytes"

$rCompact = Run-Kvdb @('compact', $dbShrink)
$sizeAfter = (Get-Item $dbShrink).Length
Write-Host "Size after compaction: $sizeAfter bytes"

$cShrink = Run-Kvdb @('check', $dbShrink)
$getLive = Run-Kvdb @('get', $dbShrink, 'live_0')

if ($sizeAfter -lt ($sizeBefore / 10) -and $cShrink.Out -match 'OK' -and $getLive.Out -match 'live_val_0') {
    $report += "Space Reclamation: PASS (File shrank from $sizeBefore to $sizeAfter bytes, live data intact, check OK)"
} else {
    $report += "Space Reclamation: FAIL (sizeBefore=$sizeBefore, sizeAfter=$sizeAfter)"
}

# -------------------------------------------------------------
# Test 2: Scenario E1 - Single Put exceeding max-bytes cap
# -------------------------------------------------------------
Write-Host "=== TEST 2: Scenario E1 - Single Put at Cap ==="
$dbE1 = Join-Path $tempDir 'e1.db'
# Create DB with 1000 max bytes
$rOpenE1 = Run-Kvdb @('open', $dbE1, '--max-bytes', '500')
# Populate with valid data
Run-Kvdb @('put', $dbE1, 'key1', 'val1') | Out-Null
$initialSize = (Get-Item $dbE1).Length

# Attempt to write a single record that exceeds cap
$rOver = Run-Kvdb @('put', $dbE1, 'huge_key', ('X' * 600))
$sizeAfterE1 = (Get-Item $dbE1).Length
$getK1 = Run-Kvdb @('get', $dbE1, 'key1')
$getHuge = Run-Kvdb @('get', $dbE1, 'huge_key')

if ($rOver.ExitCode -ne 0 -and $sizeAfterE1 -eq $initialSize -and $getK1.Out -match 'val1' -and $getHuge.Out -match 'NOTFOUND') {
    $report += "E1 Single Put at Cap: PASS (rejected with error, no bytes written, old data intact)"
} else {
    $report += "E1 Single Put at Cap: FAIL (exit=$($rOver.ExitCode), size=$sizeAfterE1)"
}

# -------------------------------------------------------------
# Test 3: Scenario E2 - Batch exceeds max-bytes cap
# -------------------------------------------------------------
Write-Host "=== TEST 3: Scenario E2 - Batch Exceeds Cap ==="
$dbE2 = Join-Path $tempDir 'e2.db'
Run-Kvdb @('open', $dbE2, '--max-bytes', '1000') | Out-Null
Run-Kvdb @('put', $dbE2, 'init_key', 'init_val') | Out-Null
$e2SizeBefore = (Get-Item $dbE2).Length

$batchE2 = Join-Path $tempDir 'batch_over.txt'
@"
put b1 v1
put b2 $( 'Y' * 800 )
put b3 v3
"@ | Set-Content $batchE2

$rBatchE2 = Run-Kvdb @('batch', $dbE2, $batchE2)
$e2SizeAfter = (Get-Item $dbE2).Length
$getB1 = Run-Kvdb @('get', $dbE2, 'b1')
$getInit = Run-Kvdb @('get', $dbE2, 'init_key')

if ($rBatchE2.ExitCode -ne 0 -and $e2SizeAfter -eq $e2SizeBefore -and $getB1.Out -match 'NOTFOUND' -and $getInit.Out -match 'init_val') {
    $report += "E2 Batch Exceeds Cap: PASS (rejected, zero partial ops written, size unchanged at $e2SizeAfter bytes)"
} else {
    $report += "E2 Batch Exceeds Cap: FAIL"
}

# -------------------------------------------------------------
# Test 4: Scenario E5 - Repeated overwrites succeed via Auto-Compaction at cap
# -------------------------------------------------------------
Write-Host "=== TEST 4: Scenario E5 - Auto-compaction on repeated overwrites at Cap ==="
$dbE5 = Join-Path $tempDir 'e5.db'
$capBytes = 1500
Run-Kvdb @('open', $dbE5, '--max-bytes', "$capBytes") | Out-Null

# Repeatedly overwrite a single key 50 times in serve mode
$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName = $kvdb
$psi.Arguments = "serve `"$dbE5`" --max-bytes $capBytes"
$psi.UseShellExecute = $false
$psi.RedirectStandardInput = $true
$psi.RedirectStandardOutput = $true
$psi.CreateNoWindow = $true

$proc = [System.Diagnostics.Process]::Start($psi)
$sw = $proc.StandardInput
$sr = $proc.StandardOutput

$e5Success = 0
for ($i = 1; $i -le 50; $i++) {
    $v = "val_cycle_$i"
    $vHex = [System.BitConverter]::ToString([System.Text.Encoding]::UTF8.GetBytes($v)).Replace('-','').ToLower()
    $sw.WriteLine("$i put 6b6579 $vHex") # key="key"
    $ack = $sr.ReadLine()
    if ($ack -match "^ACK $i") {
        $e5Success++
    }
}
$sw.Close()
$proc.WaitForExit(5000)

$e5FinalSize = (Get-Item $dbE5).Length
$getFinalE5 = Run-Kvdb @('get', $dbE5, 'key')
$cE5 = Run-Kvdb @('check', $dbE5)

if ($e5Success -eq 50 -and $e5FinalSize -le $capBytes -and $getFinalE5.Out -match 'val_cycle_50' -and $cE5.Out -match 'OK') {
    $report += "E5 Auto-Compaction Overwrites: PASS (50/50 overwrites succeeded under $capBytes byte cap, final size $e5FinalSize <= $capBytes)"
} else {
    $report += "E5 Auto-Compaction Overwrites: FAIL (success=$e5Success, size=$e5FinalSize, expected <= $capBytes)"
}

# -------------------------------------------------------------
# Test 5: Serve prints ERR <id> disk full
# -------------------------------------------------------------
Write-Host "=== TEST 5: Serve prints ERR <id> disk full ==="
$dbDiskFull = Join-Path $tempDir 'serve_df.db'
Run-Kvdb @('open', $dbDiskFull, '--max-bytes', '400') | Out-Null

$psiDF = New-Object System.Diagnostics.ProcessStartInfo
$psiDF.FileName = $kvdb
$psiDF.Arguments = "serve `"$dbDiskFull`" --max-bytes 400"
$psiDF.UseShellExecute = $false
$psiDF.RedirectStandardInput = $true
$psiDF.RedirectStandardOutput = $true
$psiDF.CreateNoWindow = $true

$procDF = [System.Diagnostics.Process]::Start($psiDF)
$swDF = $procDF.StandardInput
$srDF = $procDF.StandardOutput

# Put normal op
$swDF.WriteLine('1 put 6b31 7631')
$ack1 = $srDF.ReadLine()

# Hopeless write exceeding cap
$bigHex = 'ff' * 500
$swDF.WriteLine("2 put 6865617679 $bigHex")
$errDF = $srDF.ReadLine()

$swDF.Close()
$procDF.WaitForExit(5000)

if ($ack1 -match '^ACK 1' -and $errDF -match 'ERR 2 disk full') {
    $report += "Serve Disk Full: PASS (output: '$errDF')"
} else {
    $report += "Serve Disk Full: FAIL (got '$errDF')"
}

# -------------------------------------------------------------
# Test 6: Verify file size <= max-bytes on disk after every test
# -------------------------------------------------------------
Write-Host "=== TEST 6: File size <= max-bytes assertion ==="
$dbSizesOk = $true
if ((Get-Item $dbE1).Length -gt 500) { $dbSizesOk = $false }
if ((Get-Item $dbE2).Length -gt 1000) { $dbSizesOk = $false }
if ((Get-Item $dbE5).Length -gt 1500) { $dbSizesOk = $false }
if ((Get-Item $dbDiskFull).Length -gt 400) { $dbSizesOk = $false }

if ($dbSizesOk) {
    $report += "Max-Bytes Cap Invariant: PASS (file size on disk <= maxBytes across all disk-full tests)"
} else {
    $report += "Max-Bytes Cap Invariant: FAIL"
}

Write-Host "`n=== SUMMARY OF AGENT 4 AUDIT ==="
$report | ForEach-Object { Write-Host $_ }

Remove-Item -Recurse -Force $tempDir

