# Runs gcgrep unit-test binaries and an end-to-end daemon check on Windows.
$ErrorActionPreference = 'Continue'
[Console]::OutputEncoding = [Text.Encoding]::UTF8
Set-Location C:\t\gcgrep
$script:PASS = 0; $script:FAIL = 0

function Check($name, $out, $want, $forbid) {
    $text = ($out | Out-String)
    $ok = $true
    foreach ($w in $want)   { if ($text -notmatch [regex]::Escape($w)) { $ok = $false } }
    foreach ($f in $forbid) { if ($text -match [regex]::Escape($f))    { $ok = $false } }
    if ($ok) { Write-Output "[PASS] $name"; $script:PASS++ }
    else {
        Write-Output "[FAIL] $name"
        ($text -split "`n") | Select-Object -First 5 | ForEach-Object { Write-Output "  | $_" }
        $script:FAIL++
    }
}

Write-Output "[STEP] unit tests"
foreach ($t in 'index','ignore','daemon','symbol','walkdir') {
    $out = & ".\${t}_test_windows.exe" 2>&1
    $tail = ($out | Select-Object -Last 1)
    Write-Output "[RESULT] $t => $tail"
    if ($LASTEXITCODE -ne 0) { Write-Output "[FAIL-DETAIL] $t"; $out | Select-Object -Last 40; $script:FAIL++ }
    else { $script:PASS++ }
}

Write-Output "[STEP] e2e corpus"
$G = ".\gcgrep-windows-amd64.exe"
$T = "C:\t\gcgrep\e2e"
& $G stop | Out-Null
Remove-Item -Recurse -Force $T -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path "$T\sub", "$T\.hidden" | Out-Null
Set-Content -Path "$T\a.go" -Value 'func MagicNeedleOne() {}'
Set-Content -Path "$T\sub\b.txt" -Value 'magicneedleone lower'
Set-Content -Path "$T\.hidden\h.txt" -Value 'hidNeedle here'
# 3MB file with a needle at the end -> stream set
$big = ('x' * 1000000)
Set-Content -Path "$T\big.txt" -Value "$big`n$big`n$big`nstreamNeedle in big"
# binary file: NUL byte + needle
[IO.File]::WriteAllBytes("$T\bin.dat", [byte[]](98,105,110,0,32) + [Text.Encoding]::ASCII.GetBytes('binNeedle'))
# UTF-16LE with BOM
[IO.File]::WriteAllBytes("$T\utf16.txt", [byte[]](0xFF,0xFE) + [Text.Encoding]::Unicode.GetBytes('utfNeedle ok'))

Write-Output "[STEP] basic + v0.5 features"
Check 'basic' (& $G Needle $T 2>$null) @('a.go','big.txt','utf16.txt') @('bin.dat','.hidden')
Check 'hidden' (& $G --hidden Needle $T 2>$null) @('.hidden') @()
Check 'text' (& $G -a Needle $T 2>$null) @('bin.dat') @()
Check 'maxfs' (& $G --max-filesize 1M Needle $T 2>$null) @('a.go') @('big.txt')
Check 'files' (& $G -l Needle $T 2>$null) @('big.txt','a.go') @()
Check 'count' (& $G -c Needle $T 2>$null) @('big.txt:1') @()
$json = & $G --json Needle $T 2>$null | Select-Object -Last 1
Check 'json-done' $json @('"type":"done"') @()
Check 'status' (& $G status) @('stream-set') @()
Write-Output "[STEP] write-then-search on stream file"
Add-Content -Path "$T\big.txt" -Value 'freshStreamNeedle tail'
Check 'stream-raw' (& $G freshStreamNeedle $T 2>$null) @('big.txt') @()

Write-Output "[STEP] incremental + reconcile"
Set-Content -Path "$T\a.go" -Value 'func MagicNeedleTwo() {}'
Start-Sleep -Seconds 2
& $G MagicNeedleTwo $T | Out-Null; Check 'modify' "exit=$LASTEXITCODE" @('exit=0') @()
& $G stop | Out-Null
Add-Content -Path "$T\a.go" -Value 'func MagicNeedleThree() {}'
& $G MagicNeedleThree $T | Out-Null; Check 'reconcile' "exit=$LASTEXITCODE" @('exit=0') @()
$ms = (Measure-Command { & $G MagicNeedleTwo $T | Out-Null }).TotalMilliseconds
Write-Output "[STATE] warm query ${ms}ms"
& $G stop | Out-Null
Remove-Item -Recurse -Force $T -ErrorAction SilentlyContinue
Write-Output "[STATE] pass=$script:PASS fail=$script:FAIL"
if ($script:FAIL -gt 0) { exit 1 } else { exit 0 }
