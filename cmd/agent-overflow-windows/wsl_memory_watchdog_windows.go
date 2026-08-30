//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agent-overflow/internal/wsllauncher"
)

var wslMemoryWatchInterval = 100 * time.Millisecond

const wslMemoryMaxOutput = 4 << 20

type wslProcStat struct {
	PID       int
	ParentPID int
	StartTime string
	RSSPages  uint64
}

// parseWSLProcStat parses one Linux /proc/<pid>/stat line without treating
// comm as a fixed-width field. Linux permits spaces and parentheses in comm,
// so the final `) ` delimiter is the only safe boundary before the numeric
// fields.
func parseWSLProcStat(line string) (wslProcStat, error) {
	firstSpace := strings.IndexByte(line, ' ')
	closeComm := strings.LastIndex(line, ") ")
	if firstSpace <= 0 || closeComm <= firstSpace {
		return wslProcStat{}, errors.New("malformed /proc stat line")
	}
	pid, err := strconv.Atoi(line[:firstSpace])
	if err != nil || pid <= 0 {
		return wslProcStat{}, fmt.Errorf("invalid /proc stat pid %q", line[:firstSpace])
	}
	fields := strings.Fields(line[closeComm+2:])
	// Fields begin with state (field 3). ppid is relative field 2, starttime
	// is relative field 20, and rss is relative field 22.
	if len(fields) <= 21 {
		return wslProcStat{}, errors.New("short /proc stat line")
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil || parent < 0 {
		return wslProcStat{}, errors.New("invalid /proc stat parent pid")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return wslProcStat{}, errors.New("invalid /proc stat start time")
	}
	rss, err := strconv.ParseUint(fields[21], 10, 64)
	if err != nil {
		return wslProcStat{}, errors.New("invalid /proc stat rss")
	}
	return wslProcStat{PID: pid, ParentPID: parent, StartTime: fields[19], RSSPages: rss}, nil
}

func collectWSLProcTree(lines []string, root, maxProcesses, maxBytes int) ([]wslProcStat, error) {
	if root <= 0 || maxProcesses <= 0 || maxBytes <= 0 {
		return nil, errors.New("invalid WSL process-tree limits")
	}
	if len(lines) > maxProcesses || len(strings.Join(lines, "\n")) > maxBytes {
		return nil, errors.New("WSL process-tree input exceeds limits")
	}
	byPID := make(map[int]wslProcStat, len(lines))
	children := make(map[int][]int)
	for _, line := range lines {
		stat, err := parseWSLProcStat(line)
		if err != nil {
			return nil, err
		}
		if _, exists := byPID[stat.PID]; exists {
			return nil, fmt.Errorf("duplicate WSL process pid %d", stat.PID)
		}
		byPID[stat.PID] = stat
	}
	if _, ok := byPID[root]; !ok {
		return nil, errors.New("WSL process-tree root is missing")
	}
	for _, stat := range byPID {
		children[stat.ParentPID] = append(children[stat.ParentPID], stat.PID)
	}
	queue := []int{root}
	seen := map[int]bool{root: true}
	out := make([]wslProcStat, 0, len(queue))
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		out = append(out, byPID[pid])
		if len(out) > maxProcesses {
			return nil, errors.New("WSL process-tree exceeds process limit")
		}
		for _, child := range children[pid] {
			if seen[child] {
				continue
			}
			seen[child] = true
			queue = append(queue, child)
		}
	}
	return out, nil
}

type wslBackendSample struct {
	PID        int
	StartTime  string
	Executable string
	RSSBytes   uint64
}

var runWSLMemoryProbe = runWSLMemoryProbeCommand

var errWSLMemoryProbeOutputLimit = errors.New("WSL memory probe output limit exceeded")

type cappedProbeOutput struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *cappedProbeOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		b.overflow = true
		return 0, errWSLMemoryProbeOutputLimit
	}
	return b.Buffer.Write(p)
}

// startWSLMemoryWatchdog monitors the Linux backend from Windows. The
// launcher's Job Object covers the Windows process tree, while this watcher
// covers the WSL namespace. Every sample rechecks PID, /proc birth time and
// executable before accepting memory, so a recycled PID cannot be treated as
// the harness backend.
func startWSLMemoryWatchdog(parent context.Context, distro, executable string, bs *wsllauncher.Bootstrap, limit uint64, stop func()) context.CancelFunc {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		defer cancel()
		if bs == nil || bs.PID <= 0 {
			log.Printf("harness memory watchdog: backend bootstrap has no Linux pid")
			stop()
			return
		}
		sample, err := runWSLMemoryProbe(ctx, distro, bs.PID, executable)
		if err != nil {
			log.Printf("harness memory watchdog: initial WSL identity probe failed: %v", err)
			stop()
			return
		}
		if sample.PID != bs.PID || sample.RSSBytes > limit {
			log.Printf("harness memory watchdog: initial WSL sample is unsafe (pid=%d rss=%d limit=%d)", sample.PID, sample.RSSBytes, limit)
			stop()
			return
		}
		identity := sample
		log.Printf("harness memory watchdog: WSL pid=%d start=%s executable=%s limit=%d", identity.PID, identity.StartTime, identity.Executable, limit)
		ticker := time.NewTicker(wslMemoryWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			sample, err := runWSLMemoryProbe(ctx, distro, identity.PID, identity.Executable)
			if err != nil {
				log.Printf("harness memory watchdog: WSL identity probe failed: %v", err)
				stop()
				return
			}
			if sample.StartTime != identity.StartTime || sample.Executable != identity.Executable {
				log.Printf("harness memory watchdog: WSL backend identity changed (pid=%d start=%s executable=%s)", sample.PID, sample.StartTime, sample.Executable)
				stop()
				return
			}
			if sample.RSSBytes > limit {
				log.Printf("harness memory watchdog: WSL process tree exceeded limit (rss=%d limit=%d)", sample.RSSBytes, limit)
				stop()
				return
			}
		}
	}()
	return cancel
}

func runWSLMemoryProbeCommand(ctx context.Context, distro string, pid int, executable string) (wslBackendSample, error) {
	if strings.TrimSpace(distro) == "" || pid <= 0 || strings.TrimSpace(executable) == "" {
		return wslBackendSample{}, errors.New("invalid WSL memory probe target")
	}
	// --exec, never --: the -- form re-parses the joined argv through the
	// user's login shell, destroying the script's quoting and positional
	// args (wsllauncher.buildLaunchArgs has the incident note). A mangled
	// probe reads as a failed probe, and a failed probe stops the backend.
	cmd := exec.CommandContext(ctx, "wsl.exe", "-d", distro, "--exec", "/bin/sh", "-c", wslMemoryProbeScript, "agent-overflow-memory-watchdog", strconv.Itoa(pid), executable)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	var output cappedProbeOutput
	output.limit = wslMemoryMaxOutput
	cmd.Stdout = &output
	err := cmd.Run()
	if err != nil {
		if output.overflow {
			return wslBackendSample{}, errWSLMemoryProbeOutputLimit
		}
		return wslBackendSample{}, fmt.Errorf("wsl memory probe: %w", err)
	}
	out := output.Bytes()
	if len(out) > wslMemoryMaxOutput {
		return wslBackendSample{}, fmt.Errorf("wsl memory probe output exceeds %d bytes", wslMemoryMaxOutput)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 3 {
		return wslBackendSample{}, fmt.Errorf("wsl memory probe returned %d fields", len(fields))
	}
	gotPID, err := strconv.Atoi(fields[0])
	if err != nil {
		return wslBackendSample{}, fmt.Errorf("parse WSL pid: %w", err)
	}
	rss, err := strconv.ParseUint(fields[2], 10, 64)
	if err != nil {
		return wslBackendSample{}, fmt.Errorf("parse WSL rss: %w", err)
	}
	return wslBackendSample{PID: gotPID, StartTime: fields[1], Executable: executable, RSSBytes: rss}, nil
}

// The script is passed only positional arguments. It never evaluates the
// executable as shell source. /proc stat's comm field can contain spaces, so
// the parser removes the final `)` before reading the stable numeric fields.
const wslMemoryProbeScript = `
set -eu
pid="$1"
expected="$2"
stat="/proc/$pid/stat"
[ -r "$stat" ] || exit 41
line=$(cat "$stat")
rest=${line##*) }
set -- $rest
shift 19
start=$1
exe=$(readlink "/proc/$pid/exe")
[ "$exe" = "$expected" ] || exit 42
pids="$pid"
count=1
while :; do
  changed=0
  for childStat in /proc/[0-9]*/stat; do
    [ -r "$childStat" ] || continue
    childLine=$(cat "$childStat") || continue
    childPid=${childStat#/proc/}; childPid=${childPid%/stat}
    rest=${childLine##*) }
    set -- $rest
    parent=$2
    case " $pids " in
      *" $parent "*) case " $pids " in *" $childPid "*) ;; *) count=$((count + 1)); [ "$count" -le 65536 ] || exit 44; pids="$pids $childPid"; [ "${#pids}" -le 4194304 ] || exit 45; changed=1 ;; esac ;;
    esac
  done
  [ "$changed" = 1 ] || break
done
pagesize=$(getconf PAGESIZE)
total=0
for member in $pids; do
  memberStat="/proc/$member/stat"
  [ -r "$memberStat" ] || continue
  memberLine=$(cat "$memberStat") || continue
  rest=${memberLine##*) }
  set -- $rest
  shift 21
  rssPages=$1
  case "$rssPages" in ''|*[!0-9]*) exit 43 ;; esac
  total=$((total + rssPages * pagesize))
done
printf '%s %s %s\n' "$pid" "$start" "$total"
`
