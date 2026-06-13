# Benchmark gcgrep vs findstr / PowerShell Select-String on the
# kubernetes tree at C:\t\kubernetes.
$ErrorActionPreference = 'Continue'
[Console]::OutputEncoding = [Text.Encoding]::UTF8
Set-Location C:\t\gcgrep
$G = ".\gcgrep-windows-amd64.exe"
$K = "C:\t\kubernetes"

& $G stop | Out-Null
Remove-Item "$env:LOCALAPPDATA\gcgrep\index-*.gob.gz" -ErrorAction SilentlyContinue

Write-Output "[STEP] cold: index build + first search"
$t = Measure-Command { & $G -c NewSchedulerCommand $K | Out-Null }
Write-Output ("[STATE] cold = {0:n1}s" -f $t.TotalSeconds)

Write-Output "[STEP] warm literal x3"
1..3 | ForEach-Object {
    $t = Measure-Command { & $G NewSchedulerCommand $K | Out-Null }
    Write-Output ("[STATE] warm literal = {0:n0}ms" -f $t.TotalMilliseconds)
}
Write-Output "[STEP] warm -i literal"
$n = (& $G --limit 0 -i leaderelection $K | Measure-Object).Count
$t = Measure-Command { & $G --limit 0 -i leaderelection $K | Out-Null }
Write-Output ("[STATE] -i = {0:n0}ms, {1} lines" -f $t.TotalMilliseconds, $n)
Write-Output "[STEP] warm regex"
$t = Measure-Command { & $G 'func New\w+Controller\(' $K | Out-Null }
Write-Output ("[STATE] regex = {0:n0}ms" -f $t.TotalMilliseconds)

Write-Output "[STEP] restart: SIGTERM-less stop + reload + reconcile"
& $G stop | Out-Null
$t = Measure-Command { & $G NewSchedulerCommand $K | Out-Null }
Write-Output ("[STATE] restart first query = {0:n1}s" -f $t.TotalSeconds)

Write-Output "[STEP] baseline: findstr /s literal"
$t = Measure-Command { findstr /s /n "NewSchedulerCommand" "$K\*" | Out-Null }
Write-Output ("[STATE] findstr = {0:n1}s" -f $t.TotalSeconds)
Write-Output "[STEP] baseline: findstr 2nd run (warm cache)"
$t = Measure-Command { findstr /s /n "NewSchedulerCommand" "$K\*" | Out-Null }
Write-Output ("[STATE] findstr warm = {0:n1}s" -f $t.TotalSeconds)
Write-Output "[STEP] baseline: Select-String literal (warm cache)"
$t = Measure-Command { Get-ChildItem -Recurse -File $K | Select-String -SimpleMatch "NewSchedulerCommand" | Out-Null }
Write-Output ("[STATE] select-string = {0:n1}s" -f $t.TotalSeconds)

& $G status
Write-Output "[STEP] index location"
Get-ChildItem "$env:LOCALAPPDATA\gcgrep" | Select-Object Name, @{n='MB';e={[math]::Round($_.Length/1MB,1)}}
& $G stop
Write-Output "[DONE]"
