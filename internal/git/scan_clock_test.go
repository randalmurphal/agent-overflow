package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/testutil/mockexec"
)

// scanClock is the clock of a Core under test of a stat-keyed memo. A
// file's change time cannot be set, so a test cannot make a file old: it
// moves the scan's clock instead.
type scanClock struct {
	t *testing.T
	// probeDir is where the clock reads the filesystem's time.
	probeDir string
	mu       sync.Mutex
	at       time.Time
	ticking  bool
}

// freshMargin is how recently a file must have been written for a ticking
// scanClock to read it as inside statRacyWindow.
const freshMargin = 250 * time.Millisecond

// newScanClock returns a clock under which every file written so far is
// settled and every file written from now on is fresh.
func newScanClock(t *testing.T) *scanClock {
	t.Helper()
	clock := &scanClock{t: t, probeDir: t.TempDir()}
	clock.settle()
	return clock
}

// awaitFileTime returns once a file written now is stamped at or after
// instant. A filesystem stamps a write with a clock that runs behind the
// one time.Now reads, by a timer tick and by more on a loaded machine.
func (c *scanClock) awaitFileTime(instant time.Time) {
	c.t.Helper()
	probe := filepath.Join(c.probeDir, "probe")
	for {
		if err := os.WriteFile(probe, []byte{0}, 0o644); err != nil {
			c.t.Fatalf("scan clock probe: %v", err)
		}
		info, err := os.Stat(probe)
		if err != nil {
			c.t.Fatalf("scan clock probe: %v", err)
		}
		if !info.ModTime().Before(instant) {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// drive makes clock the time core scans by.
func (c *scanClock) drive(core *Core) *Core {
	core.nowFn = c.now
	return core
}

func (c *scanClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ticking {
		return time.Now().Add(statRacyWindow - freshMargin)
	}
	return c.at
}

// settle stops the clock where every file written so far has been left
// alone for statRacyWindow, and no file written later has.
func (c *scanClock) settle() {
	c.t.Helper()
	now := time.Now()
	c.mu.Lock()
	c.at, c.ticking = now.Add(statRacyWindow), false
	c.mu.Unlock()
	c.awaitFileTime(now)
}

// tick lets the clock run: every file written so far is settled, and a
// file is fresh for freshMargin after it is written.
func (c *scanClock) tick() {
	c.t.Helper()
	time.Sleep(2 * freshMargin)
	c.awaitFileTime(time.Now())
	c.mu.Lock()
	c.ticking = true
	c.mu.Unlock()
}

// replaceKeepingStat replaces path with a new file of the given content
// and path's modification time: what a copy that preserves times leaves.
// content must be as long as what it replaces.
func replaceKeepingStat(t *testing.T, path, content string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if int64(len(content)) != info.Size() {
		t.Fatalf("replaceKeepingStat %s: %d bytes replace %d", path, len(content), info.Size())
	}
	staged := path + ".replacement"
	mockexec.WriteFile(t, staged, []byte(content), info.Mode().Perm())
	if err := os.Chtimes(staged, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("chtimes %s: %v", staged, err)
	}
	if err := os.Rename(staged, path); err != nil {
		t.Fatalf("rename %s: %v", staged, err)
	}
}

// hookedGit runs the real git, logging "<argv>" per invocation to
// $AO_GIT_LOG, and sources $AO_GIT_HOOKS/before-<subcommand> before it and
// after-<subcommand> after it when a test has put one there.
const hookedGit = `#!/bin/sh
printf '%s\n' "$*" >> "$AO_GIT_LOG"
if [ -f "$AO_GIT_HOOKS/before-$1" ]; then . "$AO_GIT_HOOKS/before-$1"; fi
"$AO_GIT_REAL" "$@"
code=$?
if [ -f "$AO_GIT_HOOKS/after-$1" ]; then . "$AO_GIT_HOOKS/after-$1"; fi
exit $code
`

// gitHooks lets a test run a shell snippet around one git subcommand.
type gitHooks struct {
	t       *testing.T
	dir     string
	logPath string
}

// installHookedGit puts hookedGit on PATH for the test, which therefore
// cannot run in parallel.
func installHookedGit(t *testing.T) *gitHooks {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find git: %v", err)
	}
	hooks := &gitHooks{t: t, dir: t.TempDir()}
	hooks.logPath = installMockGit(t, hookedGit)
	t.Setenv("AO_GIT_REAL", real)
	t.Setenv("AO_GIT_HOOKS", hooks.dir)
	return hooks
}

// set installs script as the hook named "before-diff", "after-status"
// and so on. An empty script removes it.
func (h *gitHooks) set(name, script string) {
	h.t.Helper()
	path := filepath.Join(h.dir, name)
	if script == "" {
		if err := os.Remove(path); err != nil {
			h.t.Fatalf("remove hook %s: %v", name, err)
		}
		return
	}
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		h.t.Fatalf("write hook %s: %v", name, err)
	}
}
