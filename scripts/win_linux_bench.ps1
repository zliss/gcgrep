$ErrorActionPreference = 'Continue'
[Console]::OutputEncoding = [Text.Encoding]::UTF8
Set-Location C:\t\gcgrep
$G = ".\gcgrep-windows-amd64.exe"
$D = "C:\t\linux"
$env:GCGREP_ENGINE = "mem"

& $G stop 2>$null | Out-Null

Write-Output "[STEP] cold build linux kernel (mem engine, ~1.5GB / 93k files)"
$sw = [Diagnostics.Stopwatch]::StartNew()
$out = & $G --json sched_yield $D 2>$null
$sw.Stop()
$last = ($out | Select-Object -Last 1) | ConvertFrom-Json
Write-Output "[STATE] cold build+search $([int]$sw.Elapsed.TotalSeconds)s matches=$($last.matches) dur=$($last.durMs)ms"

Write-Output "[STEP] warm search x5"
for ($i = 1; $i -le 5; $i++) {
    $out = & $G --json sched_yield $D 2>$null
    $j = ($out | Select-Object -Last 1) | ConvertFrom-Json
    Write-Output "  run${i}: $($j.durMs)ms matches=$($j.matches)"
}

Write-Output "[STEP] status"
& $G status

Write-Output "[STEP] RSS"
$st = & $G status 2>$null
$pidLine = $st | Select-String 'pid'
if ($pidLine) {
    $pid = ($pidLine -replace '.*pid\s+','').Trim()
    $proc = Get-Process -Id $pid -ErrorAction SilentlyContinue
    if ($proc) {
        $rss = [math]::Round($proc.WorkingSet64 / 1MB)
        Write-Output "  PID=$pid RSS=${rss}MB"
    }
}

Write-Output "[STEP] correctness"
$r1 = & $G -F 'EXPORT_SYMBOL(schedule)' $D 2>$null
$r1 | Select-Object -First 2
$r2 = & $G do_exit $D 2>$null
Write-Output "  do_exit hits: $($r2.Count)"

Write-Output "[STEP] write-then-read"
Set-Content -Path "$D\_test_probe.c" -Value 'int gcgrepWinTestMarker = 1;'
$r3 = & $G gcgrepWinTestMarker $D 2>$null
$r3
Remove-Item "$D\_test_probe.c" -ErrorAction SilentlyContinue

Write-Output "[STEP] compare with findstr"
$sw2 = [Diagnostics.Stopwatch]::StartNew()
findstr /s /n "sched_yield" "$D\*.*" >$null 2>$null
$sw2.Stop()
Write-Output "[STATE] findstr: $([int]$sw2.Elapsed.TotalMilliseconds)ms"

& $G stop 2>$null | Out-Null
Remove-Variable -Name GCGREP_ENGINE -Scope env -ErrorAction SilentlyContinue
Write-Output "[DONE]"
