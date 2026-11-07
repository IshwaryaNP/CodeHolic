$ErrorActionPreference = 'Stop'
$tempDir = Join-Path $env:TEMP ('agent6_audit_' + [System.Guid]::NewGuid().ToString().Substring(0,8))
New-Item -ItemType Directory -Path $tempDir -Force | Out-Null
$torture = (Resolve-Path '.\torture.exe').Path
$kvdb = (Resolve-Path '.\kvdb.exe').Path

Write-Host "Temp dir: $tempDir"

$modes = @('default', 'large-values', 'compaction', 'recovery-crash', 'large-batches', 'disk-full', 'bulk-import')
$results = [ordered]@{}

foreach ($m in $modes) {
    Write-Host "Running torture mode: $m (50 iterations)..."
    $dbPath = Join-Path $tempDir ("test_$m.db")
    $p = Start-Process -FilePath $torture -ArgumentList "-mode $m -iterations 50 -seed 12345 -kvdb `"$kvdb`" -db `"$dbPath`"" -NoNewWindow -Wait -PassThru -RedirectStandardOutput (Join-Path $tempDir "$m.out") -RedirectStandardError (Join-Path $tempDir "$m.err")
    $results[$m] = $p.ExitCode
}

Write-Host "`n=== TORTURE MODES SUMMARY ==="
foreach ($key in $results.Keys) {
    $code = $results[$key]
    $status = if ($code -eq 0) { "PASS" } else { "FAIL (ExitCode: $code)" }
    Write-Host "Mode: $key -> $status"
}

Remove-Item -Recurse -Force $tempDir

