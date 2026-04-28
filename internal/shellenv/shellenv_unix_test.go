//go:build !windows

package shellenv

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMergePath_DedupesPreservesLoginOrdering(t *testing.T) {
	login := "/usr/local/bin:/home/u/.nvm/versions/node/v24/bin:/home/u/.local/bin"
	current := "/usr/bin:/usr/local/bin:/bin"

	got := mergePath(login, current)
	want := "/usr/local/bin:/home/u/.nvm/versions/node/v24/bin:/home/u/.local/bin:/usr/bin:/bin"
	if got != want {
		t.Fatalf("mergePath result wrong\n got: %q\nwant: %q", got, want)
	}
}

func TestMergePath_DropsEmptyEntries(t *testing.T) {
	// Leading / trailing / doubled colons (common when rc files
	// prepend/append to an unset PATH) must not produce empty entries
	// in the merged result.
	login := ":/foo::/bar:"
	current := ":/baz:"

	got := mergePath(login, current)
	want := "/foo:/bar:/baz"
	if got != want {
		t.Fatalf("mergePath did not drop empties\n got: %q\nwant: %q", got, want)
	}
}

func TestMergePath_BothEmpty(t *testing.T) {
	if got := mergePath("", ""); got != "" {
		t.Fatalf("mergePath(\"\",\"\") = %q, want \"\"", got)
	}
}

func TestExtractPath_HappyPath(t *testing.T) {
	// MOTD banner + sentinel block + post-sentinel cruft. The middle
	// is what we care about; everything else is ignored.
	body := strings.Join([]string{
		"Welcome to Ubuntu 24.04 LTS",
		"Last login: ...",
		pathStartSentinel,
		"/usr/local/bin:/usr/bin:/bin",
		pathEndSentinel,
		"logout banner",
	}, "\n")

	got, err := extractPath(body)
	if err != nil {
		t.Fatalf("extractPath: %v", err)
	}
	if got != "/usr/local/bin:/usr/bin:/bin" {
		t.Fatalf("extractPath returned %q", got)
	}
}

func TestExtractPath_MissingStart(t *testing.T) {
	if _, err := extractPath("no sentinels here"); err == nil {
		t.Fatal("extractPath should error when start sentinel is missing")
	}
}

func TestExtractPath_MissingEnd(t *testing.T) {
	body := pathStartSentinel + "\n/usr/bin\n(no end sentinel)"
	if _, err := extractPath(body); err == nil {
		t.Fatal("extractPath should error when end sentinel is missing")
	}
}

func TestCandidateShells_PrefersUserShell(t *testing.T) {
	t.Setenv("SHELL", "/usr/local/bin/zsh")
	got := candidateShells()
	if len(got) == 0 || got[0] != "/usr/local/bin/zsh" {
		t.Fatalf("user shell must be first; got %v", got)
	}
}

func TestCandidateShells_FallsBackToBashOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("linux-specific fallback: skipping on %s", runtime.GOOS)
	}
	t.Setenv("SHELL", "")
	got := candidateShells()
	if len(got) == 0 || got[0] != "/bin/bash" {
		t.Fatalf("expected /bin/bash fallback when SHELL is empty; got %v", got)
	}
}

func TestCandidateShells_DropsDuplicates(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	got := candidateShells()
	// /bin/bash from $SHELL and the linux fallback are the same string;
	// we should see it exactly once.
	count := 0
	for _, s := range got {
		if s == "/bin/bash" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("/bin/bash appears %d times in candidates: %v", count, got)
	}
}

// fakeShell writes a stub shell script that emits sentinel-bracketed
// PATH content as our real probe does. It's the cheapest way to prove
// the probe + extract pipeline against an actual exec.Cmd round-trip
// without depending on bash being present (or on a CI runner having
// nvm installed).
//
// The script ignores its arguments — Probe invokes it with -ilc
// "<our script>", and we deliberately throw that script away because
// we want the captured PATH to match the value we baked in below, not
// whatever printenv on the test host would print.
func fakeShell(t *testing.T, payload string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fakesh")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' '" + pathStartSentinel + "'\n" +
		"printf '%s\\n' '" + payload + "'\n" +
		"printf '%s\\n' '" + pathEndSentinel + "'\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake shell: %v", err)
	}
	return path
}

func TestProbe_ReturnsSentinelPATH(t *testing.T) {
	shell := fakeShell(t, "/fake/login/bin:/usr/bin")
	got, err := probe(context.Background(), shell)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got != "/fake/login/bin:/usr/bin" {
		t.Fatalf("probe returned %q", got)
	}
}

func TestProbe_NonExistentShell(t *testing.T) {
	if _, err := probe(context.Background(), "/no/such/shell-aocadft"); err == nil {
		t.Fatal("probe should error when shell is missing")
	}
}

func TestSync_MergesIntoOSEnv(t *testing.T) {
	shell := fakeShell(t, "/fake/login/bin:/usr/bin")
	t.Setenv("SHELL", shell)
	t.Setenv("PATH", "/inherited/bin:/usr/bin")

	if err := Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	got := os.Getenv("PATH")
	want := "/fake/login/bin:/usr/bin:/inherited/bin"
	if got != want {
		t.Fatalf("merged PATH wrong\n got: %q\nwant: %q", got, want)
	}
}

func TestSync_NoOpWhenLoginPathSubsetOfCurrent(t *testing.T) {
	shell := fakeShell(t, "/usr/bin")
	t.Setenv("SHELL", shell)
	t.Setenv("PATH", "/usr/bin")

	before := os.Getenv("PATH")
	if err := Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if os.Getenv("PATH") != before {
		t.Fatalf("PATH unexpectedly changed: %q -> %q", before, os.Getenv("PATH"))
	}
}

func TestSync_FallsBackWhenPrimaryShellFails(t *testing.T) {
	// Primary shell exits non-zero; the loop should fall through to
	// the next candidate. We can't easily test the system fallback
	// (/bin/bash) without depending on the host so we install the
	// "primary" as a non-existent path and a working stub as $SHELL —
	// then ensure the wrong $SHELL isn't fatal.
	failingShell := filepath.Join(t.TempDir(), "missing")
	t.Setenv("SHELL", failingShell)
	if runtime.GOOS != "linux" {
		t.Skip("relies on /bin/bash being present and working")
	}
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skipf("/bin/bash not available: %v", err)
	}

	t.Setenv("PATH", "/usr/bin")
	if err := Sync(context.Background()); err != nil {
		// Bash on the test host may or may not produce sentinels — it
		// runs our actual probe script. The important thing is that
		// the missing primary shell didn't kill the call entirely;
		// surfaces both as nil error or a different (bash-shaped)
		// error rather than the file-not-found from the missing shell.
		if strings.Contains(err.Error(), failingShell) {
			t.Fatalf("Sync error still references missing primary shell: %v", err)
		}
	}
}
