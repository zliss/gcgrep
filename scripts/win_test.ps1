# Runs gcgrep unit-test binaries and an end-to-end daemon check on Windows.
$ErrorActionPreference = 'Continue'
[Console]::OutputEncoding = [Text.Encoding]::UTF8
Set-Location C:\t\gcgrep

Write-Output "[STEP] unit tests"
foreach ($t in 'index','ignore','daemon') {
    $out = & ".\${t}_test_windows.exe" 2>&1
    $tail = ($out | Select-Object -Last 1)
    Write-Output "[RESULT] $t => $tail"
    if ($LASTEXITCODE -ne 0) { Write-Output "[FAIL-DETAIL] $t"; $out | Select-Object -Last 40 }
}

Write-Output "[STEP] e2e"
$G = ".\gcgrep-windows-amd64.exe"
$T = "C:\t\gcgrep\e2e"
Remove-Item -Recurse -Force $T -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path "$T\sub" | Out-Null
Set-Content -Path "$T\a.go" -Value 'func MagicNeedleOne() {}'
Set-Content -Path "$T\sub\b.txt" -Value 'magicneedleone lower'

& $G stop | Out-Null
Write-Output "[STEP] first search (builds index)"
& $G MagicNeedleOne $T; Write-Output "exit=$LASTEXITCODE (expect 0)"
Write-Output "[STEP] case-insensitive"
& $G -i magicneedleone $T; Write-Output "exit=$LASTEXITCODE (expect 0, 2 lines)"
Write-Output "[STEP] modify file, wait, search"
Set-Content -Path "$T\a.go" -Value 'func MagicNeedleTwo() {}'
Start-Sleep -Seconds 2
& $G MagicNeedleTwo $T; Write-Output "exit=$LASTEXITCODE (expect 0)"
& $G MagicNeedleOne $T | Out-Null; Write-Output "old exit=$LASTEXITCODE (expect 1)"
Write-Output "[STEP] status"
& $G status
Write-Output "[STEP] stop + offline append + restart reconcile"
& $G stop
Add-Content -Path "$T\a.go" -Value 'func MagicNeedleThree() {}'
& $G MagicNeedleThree $T; Write-Output "exit=$LASTEXITCODE (expect 0)"
Write-Output "[STEP] daemon survives after client exits; second query latency"
$ms = (Measure-Command { & $G MagicNeedleTwo $T | Out-Null }).TotalMilliseconds
Write-Output "[STATE] warm query ${ms}ms"
& $G stop
Remove-Item -Recurse -Force $T -ErrorAction SilentlyContinue
Write-Output "[DONE]"
