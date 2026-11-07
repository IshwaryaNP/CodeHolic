$tempDir = Join-Path $env:TEMP ('agent2_audit_' + [System.Guid]::NewGuid().ToString().Substring(0,8))
New-Item -ItemType Directory -Path $tempDir -Force | Out-Null
$kvdb = (Resolve-Path '.\kvdb.exe').Path
$dbPath = Join-Path $tempDir 'serve_test.db'

Write-Host "Temp dir: $tempDir"

$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName = $kvdb
$psi.Arguments = "serve $dbPath"
$psi.UseShellExecute = $false
$psi.RedirectStandardInput = $true
$psi.RedirectStandardOutput = $true
$psi.RedirectStandardError = $true
$psi.CreateNoWindow = $true

$proc = [System.Diagnostics.Process]::Start($psi)
$sw = $proc.StandardInput
$sr = $proc.StandardOutput

# 1. put k1 (6b31) v1 (7631)
$sw.WriteLine('1 put 6b31 7631')
# 2. get k1
$sw.WriteLine('2 get 6b31')
# 3. put empty value '-' for k_empty (6b5f65)
$sw.WriteLine('3 put 6b5f65 -')
# 4. get k_empty
$sw.WriteLine('4 get 6b5f65')
# 5. batch 2: put k_b1 v_b1, del k1
$sw.WriteLine('5 batch 2')
$sw.WriteLine('put 6b5f6231 765f6231')
$sw.WriteLine('del 6b31')
# 6. bad hex top-level
$sw.WriteLine('6 put ZZ NOTHEX')
# 7. unknown op
$sw.WriteLine('7 foobar 123')
# 8. batch with bad hex in line 2
$sw.WriteLine('8 batch 2')
$sw.WriteLine('put 6b5f626164 76616c')
$sw.WriteLine('put ZZ NOTHEX')
# 9. compact
$sw.WriteLine('9 compact')
# 10. get deleted k1
$sw.WriteLine('10 get 6b31')
# 11. get batch put k_b1
$sw.WriteLine('11 get 6b5f6231')
# 12. get k_bad (should not exist because batch had bad hex)
$sw.WriteLine('12 get 6b5f626164')

$sw.Close()

$output = $sr.ReadToEnd()
$proc.WaitForExit(5000)

Write-Host "=== Output from kvdb serve ==="
Write-Host $output

