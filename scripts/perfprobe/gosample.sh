#!/usr/bin/env bash
# Long-horizon Go backend sampler, the pprof counterpart of `probe sample`.
#
# Every INTERVAL seconds it appends one JSON line: goroutine count, the
# runtime.MemStats the /debug/pprof/heap?debug=1 header carries, the
# backend's WSL process RSS, and the Windows launcher's private bytes.
# The backend runs IN WSL (`make dev-wsl`): the WSL process owning the
# [::1]:6363 listener is the backend, and its VmRSS is what catches memory
# outside the Go heap (tree-sitter's cgo allocations are invisible to
# pprof). The Windows launcher is the thin WebView2 host that started that
# backend through wsl.exe, found as the parent of the outermost wsl.exe
# whose command line runs the backend's binary; its private bytes are
# sampled as a separate field, not as the backend. Both PIDs re-resolve
# per tick, so restarts are free.
# Every SNAP_EVERY ticks it also saves a binary heap profile and a
# goroutine?debug=1 text dump for later `go tool pprof -diff_base` /
# stack-diff triage.
#
# Usage: gosample.sh [--every SECS] [--for SECS] [--out DIR]
# Run detached via systemd-run (a plain background job dies with the
# agent session): see .claude/skills/perf-investigation/SKILL.md step 2.
set -u

PPROF="http://[::1]:6363/debug/pprof"
EVERY=120
FOR=28800
OUT="/tmp/ao-gopprof"
SNAP_EVERY=8   # binary heap profile cadence, in ticks (8 x 120s = 16 min)

while [ $# -gt 0 ]; do
  case "$1" in
    --every) EVERY="$2"; shift 2 ;;
    --for)   FOR="$2"; shift 2 ;;
    --out)   OUT="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

mkdir -p "$OUT/snapshots"
ticks=$(( FOR / EVERY ))

backend_pid() {
  ss -tlnp 2>/dev/null | awk '/\[::1\]:6363 /{match($0, /pid=[0-9]+/); if (RSTART) print substr($0, RSTART+4, RLENGTH-4)}' | head -1
}

backend_rss_mb() {
  local pid
  pid=$(backend_pid)
  [ -n "$pid" ] && awk '/^VmRSS:/{printf "%.1f", $2/1024}' "/proc/$pid/status" 2>/dev/null
}

launcher_priv_mb() {
  local pid exe script
  pid=$(backend_pid)
  [ -n "$pid" ] || return 0
  exe=$(readlink "/proc/$pid/exe" 2>/dev/null) || return 0
  local q="'"
  exe=${exe//$q/$q$q}  # quoted as a PowerShell single-quoted literal below
  # The launcher runs `wsl.exe ... --exec <backend> --print-url-fd ...`; that
  # wsl.exe may spawn a second one, so take the one whose parent is not wsl.exe.
  script="\$w = Get-CimInstance Win32_Process -Filter \"Name='wsl.exe'\" | Where-Object { \$_.CommandLine -like '*${exe}*--print-url-fd*' }
foreach (\$p in \$w) { \$parent = Get-Process -Id \$p.ParentProcessId -ErrorAction SilentlyContinue
  if (\$parent -and \$parent.ProcessName -ne 'wsl') { [math]::Round(\$parent.PrivateMemorySize64/1MB,1); break } }"
  powershell.exe -NoProfile -EncodedCommand "$(printf '%s' "$script" | iconv -t UTF-16LE | base64 -w0)" 2>/dev/null | tr -d '\r'
}

for (( i=0; i<ticks; i++ )); do
  t=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  goroutines=$(curl -s --max-time 10 "$PPROF/goroutine?debug=1" | head -1 | grep -o '[0-9]*$')
  stats=$(curl -s --max-time 10 "$PPROF/heap?debug=1" | awk '
    /^# HeapAlloc =/   {a=$4} /^# HeapInuse =/ {hi=$4}
    /^# HeapSys =/     {hs=$4} /^# HeapObjects =/ {ho=$4}
    /^# Sys =/         {s=$4}  /^# NumGC =/     {gc=$4}
    END {printf "\"heapAllocMB\":%.1f,\"heapInuseMB\":%.1f,\"heapSysMB\":%.1f,\"sysMB\":%.1f,\"heapObjects\":%d,\"numGC\":%d", a/1048576, hi/1048576, hs/1048576, s/1048576, ho, gc}')
  rss=$(backend_rss_mb)
  priv=$(launcher_priv_mb)
  echo "{\"t\":\"$t\",\"goroutines\":${goroutines:-0},${stats},\"backendRssMB\":${rss:-null},\"launcherPrivMB\":${priv:-null}}"

  if (( i % SNAP_EVERY == 0 )); then
    stamp=$(date -u +%Y%m%dT%H%M%SZ)
    curl -s --max-time 20 "$PPROF/heap" -o "$OUT/snapshots/heap-$stamp.pb.gz"
    curl -s --max-time 20 "$PPROF/goroutine?debug=1" -o "$OUT/snapshots/goroutine-$stamp.txt"
  fi
  sleep "$EVERY"
done
