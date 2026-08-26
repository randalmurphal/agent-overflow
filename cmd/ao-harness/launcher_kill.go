package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"os/exec"
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
// it, using the launcher pid the backend publishes in its discovery
// files.
//
// Everything here is best-effort and non-fatal. The backend is already
// stopped by the time we run; failing to reach across the WSL boundary
// costs the operator one window, and saying so beats failing a command
// that did its main job.

// The WSL interop path to the two Windows tools. Package vars rather
// than constants so a test can point them at a fake and exercise the
// match / mismatch / no-interop branches without a Windows host.
var (
	winTasklistExe = "/mnt/c/Windows/System32/tasklist.exe"
	winTaskkillExe = "/mnt/c/Windows/System32/taskkill.exe"
)

// runInterop runs one Windows tool and returns its stdout. A package var
// for the same reason the paths are.
var runInterop = func(ctx context.Context, exe string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, exe, args...).Output()
}

// launcherImagePrefix is what a tasklist image name must start with
// before we are willing to kill the pid. Prefix rather than equality
// because the dev launcher is timestamp-named
// (agent-overflow-dev-20260826123456-1234.exe) while the installed one
// is agent-overflow.exe.
const launcherImagePrefix = "agent-overflow"

// interopTimeout bounds each Windows hop. These are local process
// queries; anything slower than this is a wedged interop layer, and the
// operator would rather read the note than wait.
const interopTimeout = 10 * time.Second

// stopLauncherWindow closes the Windows launcher window belonging to a
// stopped instance. It returns whether the kill was issued and, when it
// was not, one line for the operator explaining what is still on their
// desktop and how to close it.
//
// The identity check is not optional. A pid is a number Windows
// recycles, the value arrives from a file another process wrote, and the
// action is an unconditional /F kill — so the image name has to say
// "agent-overflow" before anything is signalled.
func stopLauncherWindow(pid int) (killed bool, note string) {
	if pid <= 0 {
		return false, ""
	}
	if !interopAvailable() {
		return false, fmt.Sprintf(
			"launcher pid %d is a Windows process and this host has no WSL interop (%s); its window is still open — close it yourself",
			pid, winTaskkillExe)
	}

	ctx, cancel := context.WithTimeout(context.Background(), interopTimeout)
	defer cancel()

	name, err := launcherImageName(ctx, pid)
	switch {
	case err != nil:
		return false, fmt.Sprintf(
			"could not identify Windows pid %d (%v); if the launcher window is still open, close it or run: taskkill.exe /PID %d /F",
			pid, err, pid)
	case name == "":
		// tasklist answers with a header line and no rows for a pid that
		// is gone. The launcher exiting on its own is the ordinary case.
		return false, ""
	case !strings.HasPrefix(strings.ToLower(name), launcherImagePrefix):
		return false, fmt.Sprintf(
			"Windows pid %d is %q, not an %s launcher; refusing to kill it — if a launcher window is still open, close it yourself",
			pid, name, launcherImagePrefix)
	}

	if _, err := runInterop(ctx, winTaskkillExe, "/PID", strconv.Itoa(pid), "/F"); err != nil {
		return false, fmt.Sprintf(
			"taskkill on launcher pid %d failed (%v); its window is still open — close it or run: taskkill.exe /PID %d /F",
			pid, err, pid)
	}
	return true, ""
}

// interopAvailable reports whether both Windows tools are reachable.
// Both, because identifying without being able to kill is useless and
// killing without identifying is what this refuses to do.
func interopAvailable() bool {
	for _, exe := range []string{winTasklistExe, winTaskkillExe} {
		if _, err := os.Stat(exe); err != nil {
			return false
		}
	}
	return true
}

// launcherImageName asks tasklist for one pid's image name, or "" when
// no process carries that pid.
//
// /FO CSV /NH is the machine-readable shape: no header, one quoted
// record per process, image name first. It is parsed with encoding/csv
// rather than split on commas because the other columns contain them
// ("123,456 K").
func launcherImageName(ctx context.Context, pid int) (string, error) {
	out, err := runInterop(ctx, winTasklistExe, "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH")
	if err != nil {
		return "", err
	}
	// A filter that matches nothing answers with a plain-text notice
	// ("INFO: No tasks are running which match...") instead of CSV, so an
	// unparseable answer — or one that is not a real 5-column record —
	// means "no such process", not a broken contract. Getting that wrong
	// in the other direction would hand the caller a bogus image name to
	// compare the prefix against.
	reader := csv.NewReader(strings.NewReader(strings.TrimSpace(string(out))))
	reader.FieldsPerRecord = -1
	record, err := reader.Read()
	if err != nil || len(record) < 2 {
		return "", nil
	}
	return strings.TrimSpace(record[0]), nil
}
