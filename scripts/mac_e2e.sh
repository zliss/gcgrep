#!/bin/bash
# E2E for gcgrep v0.5 stream-set + rg-alignment features (mac/darwin).
set -u
BIN=/tmp/gcgrep-e2e-bin
D=/tmp/gcgrep-e2e
OUT=/tmp/e2e.out
SRC="$HOME/pj/ai/ai-agent-project5/claude/gcgrep/dist/gcgrep-darwin-arm64"
PASS=0; FAIL=0
step() { echo "[STEP] $*"; }
ck() { # ck <name> <substr|!substr>... ; checks $OUT
  local name="$1"; shift
  local ok=1
  for want in "$@"; do
    if [[ "$want" == !* ]]; then
      grep -qF -- "${want#!}" "$OUT" && ok=0
    else
      grep -qF -- "$want" "$OUT" || ok=0
    fi
  done
  if [[ $ok == 1 ]]; then echo "[PASS] $name"; PASS=$((PASS+1));
  else echo "[FAIL] $name"; head -5 "$OUT" | sed 's/^/  | /'; FAIL=$((FAIL+1)); fi
}

step "stop any old daemon, install fresh binary (rm-then-cp)"
[ -x "$BIN" ] && "$BIN" stop >/dev/null 2>&1
rm -f "$BIN"; cp "$SRC" "$BIN"
"$BIN" stop >/dev/null 2>&1
step "build corpus at $D"
rm -rf "$D" /tmp/gcgrep-e2e-ext; mkdir -p "$D/.hidden" /tmp/gcgrep-e2e-ext
echo "small fileNeedle here" > "$D/small.txt"
{ head -c 3000000 /dev/zero | tr '\0' 'x'; echo " streamNeedle in big"; } > "$D/big.txt"
printf 'bin\x00ary binNeedle\n' > "$D/bin.dat"
echo "hidden hidNeedle" > "$D/.hidden/h.txt"
printf '\xff\xfe' > "$D/utf16.txt"; printf 'utfNeedle ok' | iconv -f UTF-8 -t UTF-16LE >> "$D/utf16.txt"
echo "external extNeedle" > /tmp/gcgrep-e2e-ext/ext.txt
ln -s /tmp/gcgrep-e2e-ext "$D/linkdir"

step "1 basic search: indexed + stream + utf16, no binary/hidden"
"$BIN" Needle "$D" >"$OUT" 2>/dev/null; ck "basic" "small.txt" "big.txt:1" "utf16.txt" "!bin.dat" "!.hidden" "!linkdir"
step "2 --hidden"
"$BIN" --hidden Needle "$D" >"$OUT" 2>/dev/null; ck "hidden" ".hidden/h.txt" "small.txt"
step "3 -a binary as text"
"$BIN" -a Needle "$D" >"$OUT" 2>/dev/null; ck "text" "bin.dat" "small.txt"
step "4 -L follow symlinks"
"$BIN" -L Needle "$D" >"$OUT" 2>/dev/null; ck "follow" "linkdir/ext.txt" "small.txt"
step "5 --max-filesize 1M drops big.txt"
"$BIN" --max-filesize 1M Needle "$D" >"$OUT" 2>/dev/null; ck "maxfs" "small.txt" "!big.txt"
step "6 -l and -c include stream files"
"$BIN" -l Needle "$D" >"$OUT" 2>/dev/null; ck "files" "big.txt" "small.txt"
"$BIN" -c Needle "$D" >"$OUT" 2>/dev/null; ck "count" "big.txt:1" "small.txt:1"
step "7 --json done counts include stream matches"
"$BIN" --json Needle "$D" 2>/dev/null | tail -1 >"$OUT"; ck "json-done" '"type":"done"' '"matches":3'
"$BIN" --json Needle "$D" >"$OUT" 2>/dev/null; ck "json-match" '"file":"big.txt"' '!"type":"streamfile"'
step "8 status shows stream set"
"$BIN" status >"$OUT" 2>&1; ck "status" "stream-set"
step "9 write-then-search consistency on stream file"
echo "freshStreamNeedle tail" >> "$D/big.txt"
"$BIN" freshStreamNeedle "$D" >"$OUT" 2>/dev/null; ck "stream-raw" "big.txt"
step "10 glob filter applies to stream files"
"$BIN" -g '*.txt' Needle "$D" >"$OUT" 2>/dev/null; ck "glob" "big.txt" "!bin.dat"
step "11 exit codes"
"$BIN" definitelyAbsentNeedle "$D" >/dev/null 2>&1; [[ $? == 1 ]] && { echo "[PASS] exit1"; PASS=$((PASS+1)); } || { echo "[FAIL] exit1"; FAIL=$((FAIL+1)); }

"$BIN" stop >/dev/null 2>&1
echo "[STATE] pass=$PASS fail=$FAIL"
[[ $FAIL == 0 ]]
