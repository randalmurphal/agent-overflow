#!/usr/bin/env sh
# soak-check.sh — read-only status report for the soak rig
# (docs/architecture/soak-rig.md).
#
# The soak instance is a GUI-subsystem .exe: nothing it does reaches a
# terminal, so its whole narrative lives in
# %APPDATA%\agent-overflow\launcher-soak.log — the launcher's own log,
# Wails' slog (debug level under the soak profile, which is where half
# the render-watchdog lines are), and the WSL backend's stderr piped in
# line by line.
#
# Usage:
#   scripts/soak-check.sh                 # summary of the current/last soak
#   scripts/soak-check.sh --print-log-path # just the log path (make soak uses this)
#
# Reads only. It never touches the soak's data dir or its window.
set -eu

# Watchdog vocabulary from the pinned wails fork
# (webview_window_windows_renderwatch.go). Kept in one place so the
# summary and the printed grep one-liner cannot drift apart.
STALL_PAT='renderer ran no script'
EPISODE_START_PAT='render recovery episode .* started'
EPISODE_CLOSE_PAT='render recovery episode closed'
REBUILD_PAT='rebuilding controller'
# The launcher stamps this once per boot, right after flag parsing — it
# is the marker that separates one soak run from the previous one in an
# append-only log.
BOOT_PAT='launcher: profile=soak'

resolve_log_path() {
	# %APPDATA% has to come from the Windows side: WSLENV in this repo's
	# typical setup does not propagate it into the Linux shell.
	win_appdata=$(/mnt/c/Windows/System32/cmd.exe /c 'echo %APPDATA%' 2>/dev/null | tr -d '\r\n') || win_appdata=''
	if [ -z "$win_appdata" ]; then
		echo "ERROR: could not resolve %APPDATA% via cmd.exe interop (run this from inside WSL)." >&2
		exit 1
	fi
	printf '%s/agent-overflow/launcher-soak.log\n' "$(wslpath -u "$win_appdata")"
}

LOG=$(resolve_log_path)

if [ "${1:-}" = "--print-log-path" ]; then
	printf '%s\n' "$LOG"
	exit 0
fi

printf 'soak log: %s\n' "$LOG"

if [ ! -f "$LOG" ]; then
	printf 'status:   no soak log yet — start one with `make soak`\n'
	exit 0
fi

# Backend liveness is checked WSL-side rather than by scanning the
# Windows task list: `--soak` is on the backend's argv, so this can never
# be confused with the developer's own dev-wsl backend.
if pgrep -f -- '--soak' >/dev/null 2>&1; then
	printf 'status:   RUNNING (soak backend alive in this distro)\n'
else
	printf 'status:   not running (no --soak backend in this distro)\n'
fi

# Everything below is scoped to the current run: the log is append-only
# across launches, and counting a previous soak's episodes would silently
# inflate the one being watched.
BOOT_LINE=$(grep -n "$BOOT_PAT" "$LOG" | tail -1 | cut -d: -f1 || true)
if [ -z "${BOOT_LINE:-}" ]; then
	printf 'status:   log has no soak boot marker (%s) — is this a soak log?\n' "$BOOT_PAT"
	exit 0
fi
RUN=$(tail -n +"$BOOT_LINE" "$LOG")

# Start comes from the boot marker, which the launcher writes through
# the standard logger ("YYYY/MM/DD HH:MM:SS "). The other half of the
# file — Wails' slog — stamps differently (time=RFC3339), so "how far it
# got" uses the file's mtime instead of parsing two timestamp dialects.
START_STAMP=$(printf '%s\n' "$RUN" | head -1 | grep -oE '^[0-9]{4}/[0-9]{2}/[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}' || true)
START_EPOCH=''
if [ -n "$START_STAMP" ]; then
	START_EPOCH=$(date -d "$(printf '%s\n' "$START_STAMP" | tr '/' '-')" +%s 2>/dev/null || true)
fi
LAST_EPOCH=$(date -r "$LOG" +%s 2>/dev/null || true)
if [ -n "$START_EPOCH" ] && [ -n "$LAST_EPOCH" ]; then
	ELAPSED=$((LAST_EPOCH - START_EPOCH))
	printf 'started:  %s\n' "$START_STAMP"
	printf 'last log: %s (uptime %dh%02dm)\n' "$(date -d "@$LAST_EPOCH" '+%Y/%m/%d %H:%M:%S')" "$((ELAPSED / 3600))" "$(((ELAPSED % 3600) / 60))"
fi

count() { printf '%s\n' "$RUN" | grep -cE "$1" || true; }

STALLS=$(count "$STALL_PAT")
STARTED=$(count "$EPISODE_START_PAT")
CLOSED=$(count "$EPISODE_CLOSE_PAT")
REBUILDS=$(count "$REBUILD_PAT")

printf '\nrender watchdog (this run)\n'
printf '  script stalls detected:   %s\n' "$STALLS"
printf '  recovery episodes opened: %s\n' "$STARTED"
printf '  recovery episodes closed: %s\n' "$CLOSED"
printf '  controller rebuilds:      %s\n' "$REBUILDS"
if [ "$STARTED" -gt "$CLOSED" ]; then
	printf '  >> an episode is OPEN right now — the renderer has not come back\n'
fi

if [ "$STALLS" -gt 0 ] || [ "$STARTED" -gt 0 ]; then
	printf '\nmost recent watchdog lines:\n'
	printf '%s\n' "$RUN" | grep -E "$STALL_PAT|render recovery|$REBUILD_PAT" | tail -12 | sed 's/^/  /'
	printf '\nfull history: grep -nE '\''render(er ran no script| recovery)|rebuilding controller'\'' "%s"\n' "$LOG"
else
	printf '\nno watchdog events yet in this run.\n'
fi
