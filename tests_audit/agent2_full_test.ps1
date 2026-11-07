$ErrorActionPreference = 'Stop'
$tempDir = Join-Path $env:TEMP ('agent2_audit_' + [System.Guid]::NewGuid().ToString().Substring(0,8))
New-Item -ItemType Directory -Path $tempDir -Force | Out-Null
$kvdb = (Resolve-Path '.\kvdb.exe').Path
$dbPath = Join-Path $tempDir 'serve_protocol.db'

Write-Host "=== TEST 1: Protocol parsing & validation ==="
$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName = $kvdb
$psi.Arguments = "serve `"$dbPath`""
$psi.UseShellExecute = $false
$psi.RedirectStandardInput = $true
$psi.RedirectStandardOutput = $true
$psi.RedirectStandardError = $true
$psi.CreateNoWindow = $true

$proc = [System.Diagnostics.Process]::Start($psi)
$sw = $proc.StandardInput
$sr = $proc.StandardOutput

# 1. Normal put
$sw.WriteLine('1 put 6b657931 76616c31') # key1=val1
# 2. Put with empty value '-'
$sw.WriteLine('2 put 656d707479 -')       # empty=""
# 3. Del key1
$sw.WriteLine('3 del 6b657931')
# 4. Binary key with 0x00 and 0x0A
$sw.WriteLine('4 put 000a00 010203000a')
# 5. Batch 2 ops
$sw.WriteLine('5 batch 2')
$sw.WriteLine('put 626174636831 76616c756531') # batch1=value1
$sw.WriteLine('put 626174636832 -')             # batch2=""
# 6. Compact
$sw.WriteLine('6 compact')
# 7. Bad hex in put key
$sw.WriteLine('7 put ZZ 1234')
# 8. Bad hex in put val
$sw.WriteLine('8 put 1234 ZZ')
# 9. Missing args in put
$sw.WriteLine('9 put 1234')
# 10. Unknown op
$sw.WriteLine('10 unknown')
# 11. Batch with bad hex on 2nd line
$sw.WriteLine('11 batch 2')
$sw.WriteLine('put 6661696c6b6579 76616c')
$sw.WriteLine('put ZZ 1234')
# 12. Batch 3 with early EOF (only 1 line provided before close)
$sw.WriteLine('12 batch 3')
$sw.WriteLine('put 6661696c32 76616c')

$sw.Close()
$output = $sr.ReadToEnd()
$proc.WaitForExit(5000)

Write-Host "Protocol test output:"
Write-Host $output

# Verify check and dump on the resulting database
Write-Host "=== TEST 1 Check DB ==="
$checkRes = & $kvdb check $dbPath
Write-Host "Check result: $checkRes"

$dumpRes = & $kvdb dump $dbPath
Write-Host "Dump result:"
Write-Host $dumpRes

# Test 2: 1MB value (2MB hex line) round-trip
Write-Host "=== TEST 2: 1MB value round-trip ==="
$dbPath2 = Join-Path $tempDir 'serve_1mb.db'
$psi2 = New-Object System.Diagnostics.ProcessStartInfo
$psi2.FileName = $kvdb
$psi2.Arguments = "serve `"$dbPath2`""
$psi2.UseShellExecute = $false
$psi2.RedirectStandardInput = $true
$psi2.RedirectStandardOutput = $true
$psi2.CreateNoWindow = $true

$proc2 = [System.Diagnostics.Process]::Start($psi2)
$sw2 = $proc2.StandardInput
$sr2 = $proc2.StandardOutput

$valHex = 'aa' * (1024 * 1024) # 1 MB of 0xAA bytes -> 2 MB hex
$keyHex = [System.BitConverter]::ToString([System.Text.Encoding]::UTF8.GetBytes('large_1mb')).Replace('-','').ToLower()

$sw2.WriteLine("100 put $keyHex $valHex")
$sw2.Close()
$out2 = $sr2.ReadToEnd()
$proc2.WaitForExit(10000)
Write-Host "1MB put output: $out2"

$check2 = & $kvdb check $dbPath2
Write-Host "1MB DB check: $check2"

# Test 3: ACK Durability across 100 process kills
Write-Host "=== TEST 3: ACK Durability across 100 process kills ==="
$dbPath3 = Join-Path $tempDir 'serve_durability.db'
$durabilityFails = 0

for ($i = 1; $i -le 100; $i++) {
    $p = New-Object System.Diagnostics.ProcessStartInfo
    $p.FileName = $kvdb
    $p.Arguments = "serve `"$dbPath3`""
    $p.UseShellExecute = $false
    $p.RedirectStandardInput = $true
    $p.RedirectStandardOutput = $true
    $p.CreateNoWindow = $true

    $sp = [System.Diagnostics.Process]::Start($p)
    $w = $sp.StandardInput
    $r = $sp.StandardOutput

    $k = "dur_k_$i"
    $v = "dur_v_$i"
    $kHex = [System.BitConverter]::ToString([System.Text.Encoding]::UTF8.GetBytes($k)).Replace('-','').ToLower()
    $vHex = [System.BitConverter]::ToString([System.Text.Encoding]::UTF8.GetBytes($v)).Replace('-','').ToLower()

    $w.WriteLine("$i put $kHex $vHex")
    $ackLine = $r.ReadLine()
    if ($ackLine -notmatch "^ACK $i") {
        Write-Host "Round $i FAILED: did not receive ACK, got '$ackLine'"
        $durabilityFails++
        $sp.Kill()
        break
    }

    # Immediately kill process right after ACK
    $sp.Kill()
    $sp.WaitForExit()

    # Reopen via kvdb get and assert value is present
    $getVal = & $kvdb get $dbPath3 $k
    if ($getVal -ne $v) {
        Write-Host "Round $i FAILED: value '$getVal' did not match expected '$v'"
        $durabilityFails++
        break
    }
}

if ($durabilityFails -eq 0) {
    Write-Host "Durability 100/100 rounds PASSED with zero lost ACKs!"
} else {
    Write-Host "Durability failed on some rounds!"
}

Remove-Item -Recurse -Force $tempDir

