//go:build windows

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"agent-overflow/internal/procutil"
	"agent-overflow/internal/wsllauncher"
	"golang.org/x/sys/windows"
)

var wslMemoryWatchInterval = 100 * time.Millisecond

const wslMemoryStderrTail = 16 << 10

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

type wslMemorySampler interface {
	Sample(context.Context) (wslBackendSample, error)
	Close() error
}

var openWSLMemoryProbe = openWSLMemoryProbeCommand

// startWSLMemoryWatchdog monitors the Linux backend from Windows. The
// launcher's Job Object covers the Windows process tree, while this watcher
// covers the WSL namespace. Every sample rechecks PID, /proc birth time and
// executable before accepting memory, so a recycled PID cannot be treated as
// the harness backend.
func startWSLMemoryWatchdog(parent context.Context, distro, executable string, bs *wsllauncher.Bootstrap, limit uint64, stop func()) context.CancelFunc {
	ctx, cancel := context.WithCancel(parent)
	var stopOnce sync.Once
	fail := func() { stopOnce.Do(stop) }
	go func() {
		defer cancel()
		if bs == nil || bs.PID <= 0 {
			log.Printf("harness memory watchdog: backend bootstrap has no Linux pid")
			fail()
			return
		}
		probe, err := openWSLMemoryProbe(ctx, distro, bs.PID, executable)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("harness memory watchdog: start WSL probe: %v", err)
				fail()
			}
			return
		}
		defer func() {
			if err := probe.Close(); err != nil {
				log.Printf("harness memory watchdog: close WSL probe: %v", err)
				fail()
			}
		}()
		sample, err := probe.Sample(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("harness memory watchdog: initial WSL identity probe failed: %v", err)
			fail()
			return
		}
		if sample.PID != bs.PID || sample.RSSBytes > limit {
			log.Printf("harness memory watchdog: initial WSL sample is unsafe (pid=%d rss=%d limit=%d)", sample.PID, sample.RSSBytes, limit)
			fail()
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
			sample, err := probe.Sample(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("harness memory watchdog: WSL identity probe failed: %v", err)
				fail()
				return
			}
			if sample.PID != identity.PID || sample.StartTime != identity.StartTime || sample.Executable != identity.Executable {
				log.Printf("harness memory watchdog: WSL backend identity changed (pid=%d start=%s executable=%s)", sample.PID, sample.StartTime, sample.Executable)
				fail()
				return
			}
			if sample.RSSBytes > limit {
				log.Printf("harness memory watchdog: WSL process tree exceeded limit (rss=%d limit=%d)", sample.RSSBytes, limit)
				fail()
				return
			}
		}
	}()
	return cancel
}

type wslMemoryProbeProcess struct {
	ctx        context.Context
	cancel     context.CancelFunc
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	lines      chan string
	done       chan struct{}
	readErr    error
	waitErr    error
	stderr     *procutil.TailBuffer
	pid        int
	executable string
	closeOnce  sync.Once
	closeErr   error
}

func openWSLMemoryProbeCommand(parent context.Context, distro string, pid int, executable string) (wslMemorySampler, error) {
	if strings.TrimSpace(distro) == "" || pid <= 0 || strings.TrimSpace(executable) == "" {
		return nil, errors.New("invalid WSL memory probe target")
	}
	ctx, cancel := context.WithCancel(parent)
	// --exec preserves the explicit shell's script and positional arguments.
	cmd := exec.CommandContext(ctx, "wsl.exe", "-d", distro, "--exec", "/bin/sh", "-c", wslMemoryProbeScript, "agent-overflow-memory-watchdog", strconv.Itoa(pid), executable)
	// A hidden console still takes the desktop's window-management lock.
	// Keep one pipe-only WSL session for all samples instead of relaunching it.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	cmd.WaitDelay = 2 * time.Second
	p := &wslMemoryProbeProcess{ctx: ctx, cancel: cancel, lines: make(chan string, 1), done: make(chan struct{}), pid: pid, executable: executable}
	p.stderr = procutil.NewTailBuffer(wslMemoryStderrTail)
	cmd.Stderr = p.stderr
	var err error
	p.stdin, err = cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("WSL probe stdin: %w", err)
	}
	p.stdout, err = cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, errors.Join(fmt.Errorf("WSL probe stdout: %w", err), p.stdin.Close())
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, errors.Join(fmt.Errorf("start WSL probe: %w", err), p.stdin.Close(), p.stdout.Close())
	}
	go func() {
		defer close(p.done)
		// A response contains only PID, birth time and RSS. ReadSlice bounds
		// retained output and requires a complete line before accepting it.
		reader := bufio.NewReaderSize(p.stdout, 128)
		for {
			line, err := reader.ReadSlice('\n')
			if err != nil {
				p.readErr = err
				if errors.Is(err, io.EOF) {
					p.readErr = nil
					if len(line) > 0 {
						p.readErr = io.ErrUnexpectedEOF
					}
				}
				break
			}
			select {
			case p.lines <- string(line):
			case <-ctx.Done():
				p.readErr = ctx.Err()
				close(p.lines)
				p.waitErr = cmd.Wait()
				return
			}
		}
		if p.readErr != nil {
			cancel()
		}
		close(p.lines)
		p.waitErr = cmd.Wait()
	}()
	return p, nil
}

func (p *wslMemoryProbeProcess) Sample(ctx context.Context) (sample wslBackendSample, err error) {
	defer func() {
		if err != nil {
			p.cancel()
		}
	}()
	if err := p.ctx.Err(); err != nil {
		return wslBackendSample{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return wslBackendSample{}, err
	}
	if _, err := io.WriteString(p.stdin, "sample\n"); err != nil {
		return wslBackendSample{}, fmt.Errorf("request WSL memory sample: %w", err)
	}
	select {
	case <-ctx.Done():
		return wslBackendSample{}, ctx.Err()
	case line, ok := <-p.lines:
		if ok {
			return parseWSLMemorySample(line, p.pid, p.executable)
		}
		select {
		case <-ctx.Done():
			return wslBackendSample{}, ctx.Err()
		case <-p.done:
			return wslBackendSample{}, fmt.Errorf("WSL memory probe ended: %w (stderr: %q)", errors.Join(io.EOF, p.readErr, p.waitErr), p.stderr.String())
		}
	}
}

func (p *wslMemoryProbeProcess) Close() error {
	p.closeOnce.Do(func() {
		p.cancel()
		for _, pipe := range []io.Closer{p.stdin, p.stdout} {
			if err := pipe.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				p.closeErr = errors.Join(p.closeErr, err)
			}
		}
		<-p.done
		if errors.Is(p.waitErr, exec.ErrWaitDelay) {
			p.closeErr = errors.Join(p.closeErr, p.waitErr)
		}
	})
	return p.closeErr
}

func parseWSLMemorySample(line string, pid int, executable string) (wslBackendSample, error) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return wslBackendSample{}, fmt.Errorf("WSL memory probe returned %d fields", len(fields))
	}
	gotPID, err := strconv.Atoi(fields[0])
	if err != nil || gotPID != pid {
		return wslBackendSample{}, fmt.Errorf("WSL memory probe returned unexpected pid %q", fields[0])
	}
	if _, err := strconv.ParseUint(fields[1], 10, 64); err != nil {
		return wslBackendSample{}, fmt.Errorf("parse WSL birth time: %w", err)
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
target_pid="$1"
target_executable="$2"
sample() {
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
}
while IFS= read -r request; do
  [ "$request" = sample ] || exit 46
  sample "$target_pid" "$target_executable"
done
`
