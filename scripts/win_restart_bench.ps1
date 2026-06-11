# Measures gcgrep restart (persisted-index load + reconcile) on Windows.
[Console]::OutputEncoding = [Text.Encoding]::UTF8
Set-Location C:\t\gcgrep
$G = ".\gcgrep-windows-amd64.exe"
$K = "C:\t\kubernetes"

Write-Output "[STEP] warm up daemon + index"
& $G NewSchedulerCommand $K | Out-Null
Write-Output "[STEP] stop and wait for process exit"
& $G stop
$deadline = (Get-Date).AddSeconds(60)
while ((Get-Process gcgrep-windows-amd64 -ErrorAction SilentlyContinue) -and (Get-Date) -lt $deadline) {
    Start-Sleep -Milliseconds 200
}
$alive = Get-Process gcgrep-windows-amd64 -ErrorAction SilentlyContinue
Write-Output "[STATE] daemon process alive after stop: $([bool]$alive)"
Write-Output "[STEP] restart: first query loads persisted index + reconciles"
$t = Measure-Command { & $G NewSchedulerCommand $K | Out-Null }
Write-Output ("[STATE] restart first query = {0:n1}s" -f $t.TotalSeconds)
& $G status
$t = Measure-Command { & $G NewSchedulerCommand $K | Out-Null }
Write-Output ("[STATE] next warm query = {0:n0}ms" -f $t.TotalMilliseconds)
& $G stop
Write-Output "[DONE]"
