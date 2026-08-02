package git

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// installMockGit puts a `git` shim on PATH for the duration of the test and
// returns the path of the log file the shim appends to ($AO_GIT_LOG).
func installMockGit(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock git is unix-only")
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write mock git: %v", err)
	}
	logPath := filepath.Join(binDir, "git.log")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AO_GIT_LOG", logPath)
	return logPath
}

// userLocale is the non-English locale the tests below pretend the user runs
// under. A git built with NLS translates its own messages when this is in
// effect, which is exactly what the C-locale pinning has to defeat.
const userLocale = "de_DE.UTF-8"

// localeProbeGit logs "<LC_ALL>|<LANG>|<argv>" for every invocation and
// succeeds with just enough output for its callers to keep going.
const localeProbeGit = `#!/bin/sh
printf '%s|%s|%s\n' "${LC_ALL-}" "${LANG-}" "$*" >> "$AO_GIT_LOG"
case "$1" in
  merge-tree) echo 0000000000000000000000000000000000000000 ;;
esac
exit 0
`

// commandLocales parses installMockGit's log into argv -> "LC_ALL|LANG".
func commandLocales(t *testing.T, logPath string) map[string]string {
	t.Helper()
	locales := make(map[string]string)
	for _, line := range strings.Split(readFile(t, logPath), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			t.Fatalf("malformed mock git log line %q", line)
		}
		locales[parts[2]] = parts[0] + "|" + parts[1]
	}
	return locales
}

// TestLocalePinningIsPerCommand pins both halves of the contract: every git
// invocation whose output this package pattern-matches in English runs under
// LC_ALL=C, and the invocations that do not pattern-match keep the user's
// locale (LC_ALL is not only about messages — it also pins date, number, and
// collation formatting, so it must not be applied wholesale on the runner).
func TestLocalePinningIsPerCommand(t *testing.T) {
	logPath := installMockGit(t, localeProbeGit)
	t.Setenv("LC_ALL", userLocale)
	t.Setenv("LANG", userLocale)

	core := NewCore()
	cwd := t.TempDir()

	if _, err := core.Status(cwd); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if _, err := core.WatchRoots(cwd); err != nil {
		t.Fatalf("WatchRoots: %v", err)
	}
	if _, err := core.StashPushIncludeUntracked(cwd, "ao-carry-test"); err != nil {
		t.Fatalf("StashPushIncludeUntracked: %v", err)
	}
	if _, err := core.MergeTreeConflicts(cwd, "origin/main", "feature"); err != nil {
		t.Fatalf("MergeTreeConflicts: %v", err)
	}

	locales := commandLocales(t, logPath)

	pinned := []string{
		"status --porcelain=v2 --branch",
		"rev-parse --absolute-git-dir",
		"stash push -u -m ao-carry-test",
		"merge-tree --write-tree --name-only origin/main feature",
	}
	for _, argv := range pinned {
		got, ok := locales[argv]
		if !ok {
			t.Fatalf("mock git never saw %q; log:\n%s", argv, readFile(t, logPath))
		}
		if got != "C|C" {
			t.Errorf("`git %s` ran with LC_ALL|LANG = %q, want \"C|C\"", argv, got)
		}
	}

	// Control: a command nobody string-matches keeps the user's locale.
	const unpinned = "remote get-url origin"
	got, ok := locales[unpinned]
	if !ok {
		t.Fatalf("mock git never saw %q; log:\n%s", unpinned, readFile(t, logPath))
	}
	if want := userLocale + "|" + userLocale; got != want {
		t.Errorf("`git %s` ran with LC_ALL|LANG = %q, want %q (pinning must be per-command)", unpinned, got, want)
	}
}

// localizedGit translates the messages this package matches unless the
// caller pinned the locale, standing in for a git built with NLS.
const localizedGit = `#!/bin/sh
english=0
[ "${LC_ALL-}" = "C" ] && english=1
case "$1" in
  status|rev-parse)
    if [ "$english" = "1" ]; then
      echo "fatal: not a git repository (or any of the parent directories): .git" 1>&2
    else
      echo "fatal: Kein Git-Repository (oder eines der übergeordneten Verzeichnisse): .git" 1>&2
    fi
    exit 128
    ;;
  stash)
    if [ "$english" = "1" ]; then
      echo "No local changes to save"
    else
      echo "Keine lokalen Änderungen zu sichern"
    fi
    exit 0
    ;;
esac
exit 0
`

// TestStatusReportsNonRepoUnderLocalizedGit is the user-visible half of the
// locale fix: a non-repository path must classify as IsRepo=false, not as a
// hard error, when git's "not a git repository" message is translated.
func TestStatusReportsNonRepoUnderLocalizedGit(t *testing.T) {
	installMockGit(t, localizedGit)
	t.Setenv("LC_ALL", userLocale)
	t.Setenv("LANG", userLocale)

	status, err := NewCore().Status(t.TempDir())
	if err != nil {
		t.Fatalf("Status: %v (a localized non-repo message must not surface as an error)", err)
	}
	if status.IsRepo {
		t.Error("IsRepo = true, want false")
	}
}

// TestWatchRootsFallsBackForNonRepoUnderLocalizedGit covers the same
// classification on the watcher's rev-parse path, which degrades a non-repo
// to a single recursive root rather than failing the watch install.
func TestWatchRootsFallsBackForNonRepoUnderLocalizedGit(t *testing.T) {
	installMockGit(t, localizedGit)
	t.Setenv("LC_ALL", userLocale)
	t.Setenv("LANG", userLocale)

	cwd := t.TempDir()
	roots, err := NewCore().WatchRoots(cwd)
	if err != nil {
		t.Fatalf("WatchRoots: %v (a localized non-repo message must not surface as an error)", err)
	}
	if len(roots) != 1 || roots[0].Path != cwd || !roots[0].Recursive {
		t.Fatalf("roots = %+v, want one recursive root at %s", roots, cwd)
	}
}

// TestStashPushDetectsNothingToStashUnderLocalizedGit guards the carry-over
// flow: reporting created=true when git actually stashed nothing sends the
// caller looking for a stash ref that was never written.
func TestStashPushDetectsNothingToStashUnderLocalizedGit(t *testing.T) {
	installMockGit(t, localizedGit)
	t.Setenv("LC_ALL", userLocale)
	t.Setenv("LANG", userLocale)

	created, err := NewCore().StashPushIncludeUntracked(t.TempDir(), "ao-carry-test")
	if err != nil {
		t.Fatalf("StashPushIncludeUntracked: %v", err)
	}
	if created {
		t.Error("created = true, want false when git reports nothing to stash")
	}
}
