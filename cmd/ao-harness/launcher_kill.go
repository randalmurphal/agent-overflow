package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Windows-launcher teardown, the second half of `down` on a
// launcher-hosted instance.
//
// The Windows launcher deliberately does NOT exit when its WSL backend
// child dies: that is what leaves a crashed instance's window, its
// devtools, and its chrome_debug.log on screen to be examined. The cost
// is that a DELIBERATE `ao-harness down` — which stops the backend from
// inside WSL — leaves a dead WebView2 window on the desktop until
// somebody runs taskkill by hand (observed 2026-08-26). So down closes
// it, using the immutable launcher registration the backend publishes in its
// discovery files. A PID without that registration is never actionable.
//
// Everything here is best-effort and non-fatal. The backend is already
// stopped by the time we run; failing to reach across the WSL boundary
// costs the operator one window, and saying so beats failing a command
// that did its main job.

// The WSL interop path to the two Windows tools. Package vars rather
// than constants so a test can point them at a fake and exercise the
// match / mismatch / no-interop branches without a Windows host.
var (
	winTaskkillExe   = "/mnt/c/Windows/System32/taskkill.exe"
	winPowerShellExe = "powershell.exe"
)

// runInterop runs one Windows tool and returns its stdout. A package var
// for the same reason the paths are.
var runInterop = func(ctx context.Context, exe string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, exe, args...).Output()
	if err == nil {
		return out, nil
	}
	// A Windows tool that refuses says WHY on stderr, and Output() throws
	// that away: the operator note read "exit status 1" for everything
	// from a bad filter to a denied kill. Splice it back in.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(bytes.TrimSpace(exitErr.Stderr)) > 0 {
		return out, fmt.Errorf("%w: %s", err, firstLine(string(exitErr.Stderr)))
	}
	return out, err
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

// interopTimeout bounds each Windows hop. These are local process
// queries; anything slower than this is a wedged interop layer, and the
// operator would rather read the note than wait.
const interopTimeout = 10 * time.Second

type launcherProcessIdentity struct {
	Path      string `json:"path"`
	StartTime int64  `json:"startTime"`
	Command   string `json:"command"`
}

// queryLauncherProcess obtains the Windows-side identity immediately before
// taskkill. tasklist's image name is not enough because Windows recycles PIDs
// and unrelated executables can share the agent-overflow prefix.
var queryLauncherProcess = func(ctx context.Context, pid int) (launcherProcessIdentity, error) {
	command := fmt.Sprintf("$p=Get-CimInstance Win32_Process -Filter 'ProcessId=%d'; if ($null -eq $p) { throw 'process not found' }; $o=Get-Process -Id %d -ErrorAction Stop; [pscustomobject]@{path=$o.Path; startTime=(([DateTimeOffset]$o.StartTime).ToFileTime()-116444736000000000)*100; command=$p.CommandLine} | ConvertTo-Json -Compress", pid, pid)
	out, err := runInterop(ctx, winPowerShellExe, "-NoProfile", "-NonInteractive", "-Command", command)
	if err != nil {
		return launcherProcessIdentity{}, err
	}
	var identity launcherProcessIdentity
	if err := json.Unmarshal(out, &identity); err != nil {
		return launcherProcessIdentity{}, fmt.Errorf("parse process identity: %w", err)
	}
	if identity.Path == "" || identity.StartTime == 0 {
		return launcherProcessIdentity{}, errors.New("process identity is incomplete")
	}
	return identity, nil
}

func stopLauncherWindowVerified(reg launcherRegistration) (bool, string) {
	if !reg.valid() {
		return false, "launcher identity is incomplete; refusing to kill a launcher by PID"
	}
	if reg.Namespace != "windows" {
		return false, fmt.Sprintf("launcher pid %d has unsupported namespace %q; refusing to kill it", reg.PID, reg.Namespace)
	}
	if reg.PID <= 0 {
		return false, fmt.Sprintf("launcher pid %d is invalid; refusing to kill it", reg.PID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), interopTimeout)
	defer cancel()
	identity, err := queryLauncherProcess(ctx, reg.PID)
	if err != nil {
		return false, fmt.Sprintf("could not identify Windows pid %d (%v); refusing to kill it", reg.PID, err)
	}
	if identity.StartTime != parseStartTime(reg.StartTime) || !sameWindowsPath(identity.Path, reg.Executable) {
		return false, fmt.Sprintf("Windows pid %d identity changed; refusing to kill it", reg.PID)
	}
	if present, matches := windowsArgumentMatches(identity.Command, "--profile", reg.Profile); present && !matches {
		return false, fmt.Sprintf("Windows pid %d profile identity changed; refusing to kill it", reg.PID)
	}
	if present, matches := windowsArgumentMatchesAny(identity.Command, []string{"--user-data-dir", "--webview-user-data-dir"}, reg.WebviewProfile); present && !matches {
		return false, fmt.Sprintf("Windows pid %d WebView profile identity changed; refusing to kill it", reg.PID)
	}
	if _, err := runInterop(ctx, winTaskkillExe, "/PID", strconv.Itoa(reg.PID), "/T", "/F"); err != nil {
		return false, fmt.Sprintf("taskkill on launcher pid %d failed (%v); its window is still open", reg.PID, err)
	}
	return true, ""
}

func parseStartTime(raw string) int64 {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return value
}

func sameWindowsPath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(strings.ReplaceAll(a, `\`, `/`)), filepath.Clean(strings.ReplaceAll(b, `\`, `/`)))
}

// windowsArgumentMatches checks an argument only when the launcher exposes
// it in its command line. The WebView2 profile is configured through Wails,
// not argv, so absence is valid. When present, compare the parsed argument
// value exactly rather than accepting a substring such as "perf-other".
func windowsArgumentMatches(command, name, expected string) (present, matches bool) {
	return windowsArgumentMatchesAny(command, []string{name}, expected)
}

func windowsArgumentMatchesAny(command string, names []string, expected string) (present, matches bool) {
	args := splitWindowsCommandLine(command)
	for i, arg := range args {
		for _, name := range names {
			if arg == name {
				if i+1 >= len(args) {
					return true, false
				}
				return true, sameWindowsPath(args[i+1], expected)
			}
			prefix := name + "="
			if strings.HasPrefix(strings.ToLower(arg), strings.ToLower(prefix)) {
				return true, sameWindowsPath(arg[len(prefix):], expected)
			}
		}
	}
	return false, true
}

func splitWindowsCommandLine(command string) []string {
	var args []string
	var current strings.Builder
	inQuotes := false
	for _, r := range command {
		switch {
		case r == '"':
			inQuotes = !inQuotes
		case (r == ' ' || r == '\t') && !inQuotes:
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}
