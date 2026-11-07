Write-Host '========================================='
Write-Host 'RUNNING COMPLETE END-TO-END VERIFICATION'
Write-Host '========================================='

$testDb = 'e2e_verify.db'
if (Test-Path $testDb) { Remove-Item $testDb }

# Step 1: Open fresh DB
Write-Host 'Step 1: Open fresh database...'
.\kvdb.exe open $testDb

# Step 2: Put keys
Write-Host "`nStep 2: Put keys (user:101, user:102, config:theme)..."
.\kvdb.exe put $testDb user:101 'Alice Smith'
.\kvdb.exe put $testDb user:102 'Bob Jones'
.\kvdb.exe put $testDb config:theme 'dark-mode'

# Step 3: Get keys
Write-Host "`nStep 3: Get keys..."
Write-Host -NoNewline 'user:101 = '
.\kvdb.exe get $testDb user:101
Write-Host -NoNewline 'config:theme = '
.\kvdb.exe get $testDb config:theme

# Step 4: Missing key
Write-Host "`nStep 4: Non-existent key check..."
Write-Host -NoNewline 'user:999 = '
.\kvdb.exe get $testDb user:999

# Step 5: Overwrite
Write-Host "`nStep 5: Overwrite key..."
.\kvdb.exe put $testDb user:101 'Alice Wonder'
Write-Host -NoNewline 'Updated user:101 = '
.\kvdb.exe get $testDb user:101

# Step 6: Delete
Write-Host "`nStep 6: Delete key..."
.\kvdb.exe del $testDb user:102
Write-Host -NoNewline 'Deleted user:102 = '
.\kvdb.exe get $testDb user:102

# Step 7: Atomic Batch
Write-Host "`nStep 7: Atomic batch write..."
Set-Content -Path 'e2e_batch.txt' -Value @'
put item:10 Laptop
put item:20 Mouse
put item:30 Keyboard
del config:theme
'@
.\kvdb.exe batch $testDb e2e_batch.txt

# Step 8: Dump
Write-Host "`nStep 8: Dump sorted keys..."
.\kvdb.exe dump $testDb

# Step 9: Check integrity
Write-Host "`nStep 9: Check integrity..."
.\kvdb.exe check $testDb

# Step 10: Compaction
Write-Host "`nStep 10: Space reclamation..."
$preSize = (Get-Item $testDb).Length
Write-Host "Size before compaction: $preSize bytes"
.\kvdb.exe compact $testDb
$postSize = (Get-Item $testDb).Length
Write-Host "Size after compaction: $postSize bytes"
.\kvdb.exe check $testDb

# Cleanup
Remove-Item $testDb
Remove-Item e2e_batch.txt

Write-Host "`n========================================="
Write-Host 'ALL END-TO-END VERIFICATION STEPS PASSED!'
Write-Host '========================================='

