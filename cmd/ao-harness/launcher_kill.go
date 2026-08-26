package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
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

// tasklistNoTasksPrefix is the notice tasklist prints instead of CSV
// when its filter matches nothing. Recognizing it BY NAME is what lets
// everything else that fails to parse be an error rather than a silent
// "the process is gone" — the two used to collapse into one answer, so a
// tasklist whose output format changed (a locale, a newer Windows, a
// truncated pipe) reported a live launcher as already exited and left
// the window on the desktop with nothing said.
const tasklistNoTasksPrefix = "INFO: No tasks"

// launcherImageName asks tasklist for one pid's image name, "" when no
// process carries that pid, and an error when the answer is neither.
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
	trimmed := strings.TrimSpace(string(out))
	// The two shapes that legitimately mean "no such process": the notice,
	// and nothing at all.
	if trimmed == "" || strings.HasPrefix(trimmed, tasklistNoTasksPrefix) {
		return "", nil
	}
	reader := csv.NewReader(strings.NewReader(trimmed))
	reader.FieldsPerRecord = -1
	record, err := reader.Read()
	if err != nil {
		return "", fmt.Errorf("tasklist output is not CSV (%v): %s", err, truncate(firstLine(trimmed), 120))
	}
	if len(record) < 2 {
		return "", fmt.Errorf("tasklist answered with a %d-field record, want the 5-column CSV form: %s",
			len(record), truncate(firstLine(trimmed), 120))
	}
	return strings.TrimSpace(record[0]), nil
}
