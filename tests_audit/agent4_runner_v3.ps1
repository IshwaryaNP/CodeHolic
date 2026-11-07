$ErrorActionPreference = 'Stop'
$tempDir = Join-Path $env:TEMP ('agent4_audit_v3_' + [System.Guid]::NewGuid().ToString().Substring(0,8))
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
# Test 1: Space reclamation via Compaction
# -------------------------------------------------------------
Write-Host "=== TEST 1: Space Reclamation via Compaction ==="
$dbShrink = Join-Path $tempDir 'shrink.db'
$batchPop = Join-Path $tempDir 'pop.txt'

$sb = [System.Text.StringBuilder]::new()
for ($i = 0; $i -lt 500; $i++) {
    [void]$sb.AppendLine("put k_$i v_$i")
}
for ($i = 0; $i -lt 500; $i++) {
    [void]$sb.AppendLine("del k_$i")
}
for ($i = 0; $i -lt 5; $i++) {
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

if ($sizeAfter -lt ($sizeBefore / 5) -and $sizeAfter -gt 0 -and $cShrink.Out -match 'OK' -and $getLive.Out -match 'live_val_0') {
    $report += "Space Reclamation: PASS (File shrank from $sizeBefore to $sizeAfter bytes, live data intact, check OK)"
} else {
    $report += "Space Reclamation: FAIL (sizeBefore=$sizeBefore, sizeAfter=$sizeAfter)"
}

# -------------------------------------------------------------
# Test 2: Disk-Full Enforcement in Serve (--max-bytes placed BEFORE path)
# -------------------------------------------------------------
Write-Host "=== TEST 2: Disk-Full Enforcement in Serve ==="
$dbCap = Join-Path $tempDir 'cap.db'
$maxBytes = 200

$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName = $kvdb
$psi.Arguments = "serve --max-bytes $maxBytes `"$dbCap`""
$psi.UseShellExecute = $false
$psi.RedirectStandardInput = $true
$psi.RedirectStandardOutput = $true
$psi.CreateNoWindow = $true

$proc = [System.Diagnostics.Process]::Start($psi)
$sw = $proc.StandardInput
$sr = $proc.StandardOutput

$ackedOps = 0
$gotDiskFull = $false

for ($i = 1; $i -le 20; $i++) {
    $kHex = [System.BitConverter]::ToString([System.Text.Encoding]::UTF8.GetBytes("key_$i")).Replace('-','').ToLower()
    $vHex = [System.BitConverter]::ToString([System.Text.Encoding]::UTF8.GetBytes("val_$i")).Replace('-','').ToLower()
    $sw.WriteLine("$i put $kHex $vHex")
    $line = $sr.ReadLine()
    if ($line -match "^ACK $i") {
        $ackedOps++
    } elseif ($line -match "^ERR $i.*disk full") {
        $gotDiskFull = $true
        Write-Host ("Received expected disk full error on op " + $i + ": " + $line)
        break
    }
}
$sw.Close()
$proc.WaitForExit(5000)

$capFileSize = (Get-Item $dbCap).Length
Write-Host "ACKed ops before cap: $ackedOps, file size on disk: $capFileSize bytes (max: $maxBytes)"

if ($gotDiskFull -and $capFileSize -le $maxBytes -and $ackedOps -gt 0) {
    $report += "E1/E2 Disk Full Enforcement: PASS (Rejected when exceeding $maxBytes bytes with 'ERR <id> disk full', size on disk $capFileSize <= $maxBytes)"
} else {
    $report += "E1/E2 Disk Full Enforcement: FAIL (gotDiskFull=$gotDiskFull, size=$capFileSize)"
}

# -------------------------------------------------------------
# Test 3: Scenario E5 - Repeated Overwrites at Cap succeed via Auto-Compaction
# -------------------------------------------------------------
Write-Host "=== TEST 3: Scenario E5 - Auto-Compaction at Cap ==="
$dbE5 = Join-Path $tempDir 'e5.db'
$capE5 = 1000

$psiE5 = New-Object System.Diagnostics.ProcessStartInfo
$psiE5.FileName = $kvdb
$psiE5.Arguments = "serve --max-bytes $capE5 `"$dbE5`""
$psiE5.UseShellExecute = $false
$psiE5.RedirectStandardInput = $true
$psiE5.RedirectStandardOutput = $true
$psiE5.CreateNoWindow = $true

$procE5 = [System.Diagnostics.Process]::Start($psiE5)
$swE5 = $procE5.StandardInput
$srE5 = $procE5.StandardOutput

$e5Success = 0
for ($i = 1; $i -le 50; $i++) {
    $v = "val_cycle_$i"
    $vHex = [System.BitConverter]::ToString([System.Text.Encoding]::UTF8.GetBytes($v)).Replace('-','').ToLower()
    $swE5.WriteLine("$i put 6b6579 $vHex") # key="key"
    $ack = $srE5.ReadLine()
    if ($ack -match "^ACK $i") {
        $e5Success++
    }
}
$swE5.Close()
$procE5.WaitForExit(5000)

$e5FinalSize = (Get-Item $dbE5).Length
$getFinalE5 = Run-Kvdb @('get', $dbE5, 'key')
$cE5 = Run-Kvdb @('check', $dbE5)

if ($e5Success -eq 50 -and $e5FinalSize -le $capE5 -and $getFinalE5.Out -match 'val_cycle_50' -and $cE5.Out -match 'OK') {
    $report += "E5 Auto-Compaction Overwrites: PASS (50/50 overwrites succeeded under $capE5 byte cap, final size $e5FinalSize <= $capE5, check OK)"
} else {
    $report += "E5 Auto-Compaction Overwrites: FAIL (success=$e5Success, size=$e5FinalSize, expected <= $capE5)"
}

Write-Host "`n=== SUMMARY OF AGENT 4 AUDIT ==="
$report | ForEach-Object { Write-Host $_ }

Remove-Item -Recurse -Force $tempDir

