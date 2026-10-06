//go:build !windows

package shellenv

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestMain removes the system fallback shells, so no test runs the
// developer's real login shell; a test that needs a fallback installs a fake
// one.
func TestMain(m *testing.M) {
	fallbackShells = nil
	os.Exit(m.Run())
}

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

func TestPlatformFallbackShellsEndWithBash(t *testing.T) {
	got := platformFallbackShells()
	if len(got) == 0 || got[len(got)-1] != "/bin/bash" {
		t.Fatalf("platform fallbacks = %v, want /bin/bash last", got)
	}
	if runtime.GOOS == "darwin" && got[0] != "/bin/zsh" {
		t.Fatalf("macOS fallbacks = %v, want /bin/zsh first", got)
	}
}

// useFallbackShells installs fallbacks for one test.
func useFallbackShells(t *testing.T, shells ...string) {
	t.Helper()
	previous := fallbackShells
	fallbackShells = shells
	t.Cleanup(func() { fallbackShells = previous })
}

func TestCandidateShells_FallsBackWhenSHELLIsEmpty(t *testing.T) {
	useFallbackShells(t, "/bin/bash")
	t.Setenv("SHELL", "")
	got := candidateShells()
	if len(got) != 1 || got[0] != "/bin/bash" {
		t.Fatalf("expected only the fallback when SHELL is empty; got %v", got)
	}
}

func TestCandidateShells_DropsDuplicates(t *testing.T) {
	useFallbackShells(t, "/bin/bash")
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
		"printf '%s\\n' '" + pathEndSentinel + "'\n" +
		"printf '%s\\n' '" + varsStartSentinel + "' '" + varsEndSentinel + "'\n"
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
	if got.path != "/fake/login/bin:/usr/bin" {
		t.Fatalf("probe returned %q", got.path)
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
	useFallbackShells(t, fakeShell(t, "/fallback/bin:/usr/bin"))
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("PATH", "/usr/bin")

	if err := Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got, want := os.Getenv("PATH"), "/fallback/bin:/usr/bin"; got != want {
		t.Fatalf("PATH = %q, want the fallback shell's %q", got, want)
	}
}

// shellScript writes a stub shell that runs prelude and then prints payload
// between the sentinels, as the real probe's script does.
func shellScript(t *testing.T, prelude, payload string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakesh")
	body := "#!/bin/sh\n" + prelude + "\n" +
		"printf '%s\\n' '" + pathStartSentinel + "' '" + payload + "' '" + pathEndSentinel + "' '" + varsStartSentinel + "' '" + varsEndSentinel + "'\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake shell: %v", err)
	}
	return path
}

// jobControlPrelude does what an interactive bash or zsh does at startup
// when its process group is not the foreground group of its terminal: stop
// the whole group with SIGTTIN until the terminal gives it the foreground.
const jobControlPrelude = "if (: </dev/tty) 2>/dev/null; then kill -s TTIN 0; fi"

const (
	probeHelperRole  = "AO_SHELLENV_PROBE_HELPER"
	probeHelperShell = "AO_SHELLENV_PROBE_SHELL"
)

// TestProbe_InBackgroundProcessGroupCompletes runs the probe the way an
// update trial does: in a background process group of a session that has a
// controlling terminal. The probe's shell must not stop the prober.
func TestProbe_InBackgroundProcessGroupCompletes(t *testing.T) {
	shell := shellScript(t, jobControlPrelude, "/fake/login/bin")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProbeHelper$")
	cmd.Env = append(os.Environ(), probeHelperRole+"=terminal", probeHelperShell+"="+shell)
	terminal, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("start the helper on a terminal: %v", err)
	}
	defer terminal.Close()
	var output bytes.Buffer
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(&output, terminal)
	}()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err = <-waited:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the terminal helper did not exit")
	}
	terminal.Close()
	<-copied
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "probe returned /fake/login/bin err=<nil>") {
		t.Fatalf("probe result missing:\n%s", output.String())
	}
}

// TestProbeHelper is the subprocess of
// TestProbe_InBackgroundProcessGroupCompletes. As "terminal" it is the
// foreground of its terminal and runs itself as "background" in a new
// process group of the same session, which runs the probe.
func TestProbeHelper(t *testing.T) {
	switch os.Getenv(probeHelperRole) {
	case "terminal":
		child := exec.Command(os.Args[0], "-test.run=^TestProbeHelper$")
		child.Env = append(os.Environ(), probeHelperRole+"=background")
		var out bytes.Buffer
		child.Stdout, child.Stderr = &out, &out
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := child.Start(); err != nil {
			fmt.Printf("start the background prober: %v\n", err)
			os.Exit(1)
		}
		waited := make(chan error, 1)
		go func() { waited <- child.Wait() }()
		select {
		case err := <-waited:
			fmt.Print(out.String())
			if err != nil {
				fmt.Printf("background prober failed: %v\n", err)
				os.Exit(1)
			}
		case <-time.After(15 * time.Second):
			_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
			fmt.Println("background prober did not finish: its process group was stopped")
			os.Exit(1)
		}
		os.Exit(0)
	case "background":
		got, err := probe(context.Background(), os.Getenv(probeHelperShell))
		fmt.Printf("probe returned %s err=%v\n", got.path, err)
		os.Exit(0)
	default:
		t.Skip("subprocess of TestProbe_InBackgroundProcessGroupCompletes")
	}
}

// A daemon started by the rc files inherits stdout and outlives the shell.
// The probe still answers once the shell exits.
func TestProbe_DescendantHoldingStdoutDoesNotBlock(t *testing.T) {
	shell := shellScript(t, "sleep 30 &", "/fake/login/bin")
	started := time.Now()
	got, err := probe(context.Background(), shell)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got.path != "/fake/login/bin" {
		t.Fatalf("probe returned %q", got.path)
	}
	if elapsed := time.Since(started); elapsed > probeTimeout {
		t.Fatalf("probe took %s, past its %s cap", elapsed, probeTimeout)
	}
}
