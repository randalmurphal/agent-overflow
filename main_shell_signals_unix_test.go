//go:build unix

// The shell scripts in this repository end on HUP, INT and TERM with their
// cleanup done, under bash 3.2 and later, run as bash or as sh, and under
// dash. This file holds the rule and its checks.
//
// Bash 5.2, and no other release, runs a pending signal handler when it
// starts to parse a $(...), <(...) or >(...) that it is expanding, in that
// substitution's parser state: the handler fails with a syntax error and
// the signal is lost. So under bash 5.2 a script sets no handler for these
// signals, or expands none of these substitutions while one is set.
//
// A POSIX sh script cleans up in its EXIT trap, and sets its handlers, which
// exit with 128 plus the signal's number, with this one line:
//
//	case ${BASH_VERSION:-} in 5.2.*) ;; *) trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' TERM ;; esac
//
// Bash 5.2 runs the EXIT trap on these signals by itself, in subshells too.
// It runs it at once, so its cleanup can run beside a command that was
// running when the signal came, and the script can exit before a subshell's
// own EXIT trap is done; and an INT sent to the script alone, not its
// process group, does not end it when its running command handles the INT
// and exits 0. Every other shell gets the handlers, which wait for the
// running command first: bash 3.2 also runs no subshell's EXIT trap on HUP
// or TERM without one.
//
// The EXIT trap is set before the script makes its first temporary path,
// and removes its temporary paths by a name prefix unique to the run, such
// as ${TMPDIR:-/tmp}/NAME.$$., so a signal while mktemp runs, before the
// script has the path's name, leaves nothing behind.
//
// A bash script that needs handlers of its own, as the Android smoke
// scripts do, expands none of these substitutions in its top-level code
// after setting them. Their functions are checked at run time instead, by
// the substitution probe in main_android_smoke_unix_test.go.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/testutil/mockexec"
)

// shellSignalGuard is the one line that sets the HUP, INT and TERM handlers
// of a POSIX sh script.
const shellSignalGuard = `case ${BASH_VERSION:-} in 5.2.*) ;; *) trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' TERM ;; esac`

// probedShellScripts are bash scripts whose functions the substitution probe
// checks while they run.
var probedShellScripts = map[string]bool{
	"e2e/scripts/android-smoke.sh":    true,
	"e2e/scripts/android-emulator.sh": true,
}

var (
	// A trap command and its first argument: a quoted string or a word.
	shellTrapCommand = regexp.MustCompile(`(?:^|[;&|({]\s*|\b(?:then|do|else)\s+)trap\s+('[^']*'|"[^"]*"|\S+)([^;&|}]*)`)
	shellTrapSignal  = regexp.MustCompile(`(?:^|\s)(?:SIG)?(?:HUP|INT|TERM|1|2|15)(?:\s|$)`)
	// $(...) but not $((...)), <(...) and >(...).
	shellSubstitution = regexp.MustCompile(`\$\([^(]|\$\($|[<>]\(`)
	shellFunctionDef  = regexp.MustCompile(`^(\s*)(?:function\s+)?[A-Za-z_][A-Za-z0-9_]*\s*\(\)\s*[{(]`)
)

// shellSignalFindings checks one script against the rule above and returns
// what breaks it, one line each.
func shellSignalFindings(path string, body []byte) ([]string, error) {
	// A substitution in a function, which may run after a handler set later
	// in the file, breaks the rule only once the file sets one.
	var findings, inFunctions []string
	handlerAt := 0
	// Whether the scan is in a function's body, and the indentation of that
	// function's definition: the body ends at a closing line indented as much.
	inFunction, functionIndent, functionStart := false, "", 0
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for number := 1; scanner.Scan(); number++ {
		line := scanner.Text()
		code := strings.TrimSpace(line)
		if code == "" || strings.HasPrefix(code, "#") {
			continue
		}
		location := path + ":" + strconv.Itoa(number) + ": "
		startsFunction := false
		if !inFunction {
			if match := shellFunctionDef.FindStringSubmatch(line); match != nil {
				inFunction, functionIndent, functionStart, startsFunction = true, match[1], number, true
				// A function whose body closes on its own line.
				if rest := strings.TrimSpace(line[len(match[0]):]); strings.HasSuffix(rest, "}") || strings.HasSuffix(rest, ")") {
					inFunction = false
				}
			}
		}
		substitutes := shellSubstitution.MatchString(code)
		setsHandler := false
		if code != shellSignalGuard {
			for _, trap := range shellTrapCommand.FindAllStringSubmatch(code, -1) {
				handler, signals := trap[1], trap[2]
				if handler == "''" || handler == `""` || strings.HasPrefix(handler, "-") {
					continue
				}
				if shellTrapSignal.MatchString(signals) {
					setsHandler = true
				}
			}
		}
		inBody := inFunction || startsFunction
		switch {
		case setsHandler && handlerAt == 0:
			handlerAt = number
		case !substitutes, inBody && probedShellScripts[path]:
		case handlerAt != 0:
			findings = append(findings, location+"a substitution after the HUP, INT or TERM handler set at line "+strconv.Itoa(handlerAt)+": "+code)
		case inBody:
			inFunctions = append(inFunctions, location+"a substitution in a function of a script that sets a HUP, INT or TERM handler: "+code)
		}
		// A closing line may carry a redirection: `} >&2`.
		closes := strings.HasPrefix(line, functionIndent+"}") || strings.HasPrefix(line, functionIndent+")")
		if inFunction && !startsFunction && closes && (len(code) == 1 || code[1] == ' ' || code[1] == '\t') {
			inFunction = false
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if inFunction {
		return nil, errors.New(path + ":" + strconv.Itoa(functionStart) + ": cannot find where this function ends")
	}
	if handlerAt != 0 {
		findings = append(findings, inFunctions...)
	}
	return findings, nil
}

// shellScripts lists the shell scripts in the tree, tracked or not yet
// added, by extension or by an interpreter line that names a shell.
func shellScripts(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		t.Fatalf("list the tree's files: %v", err)
	}
	interpreter := regexp.MustCompile(`^#!\s*\S*/(?:env\s+)?(?:ba|da)?sh\b`)
	var scripts []string
	for _, path := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if strings.Contains(path, "node_modules/") {
			continue
		}
		if strings.HasSuffix(path, ".sh") {
			scripts = append(scripts, path)
			continue
		}
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue // deleted in the working tree
		}
		if err != nil {
			t.Fatal(err)
		}
		head := make([]byte, 64)
		n, _ := file.Read(head)
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if interpreter.Match(head[:n]) {
			scripts = append(scripts, path)
		}
	}
	return scripts
}

func TestShellSignalHandlersSkipSubstitutions(t *testing.T) {
	scripts := shellScripts(t)
	if len(scripts) == 0 {
		t.Fatal("found no shell scripts in the tree")
	}
	var findings []string
	for _, path := range scripts {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		found, err := shellSignalFindings(path, body)
		if err != nil {
			t.Fatal(err)
		}
		findings = append(findings, found...)
	}
	if len(findings) > 0 {
		t.Fatalf("these scripts break the signal handling rule in main_shell_signals_unix_test.go:\n%s", strings.Join(findings, "\n"))
	}
}

// The check finds a substitution that a handler's signal could be lost in,
// and only that.
func TestShellSignalFindingsSeeOnlyLostSignals(t *testing.T) {
	for _, tc := range []struct {
		name, path, script string
		finds              bool
	}{
		{name: "a substitution after a handler", script: "trap 'exit 143' TERM\nx=$(date)\n", finds: true},
		{name: "a process substitution after a handler", script: "trap 'exit 130' INT\ndiff <(a) <(b)\n", finds: true},
		{name: "a handler set by signal number", script: "trap 'exit 1' 1 2 15\nx=$(date)\n", finds: true},
		{name: "a handler set after another command", script: "set -e; trap cleanup HUP\nx=$(date)\n", finds: true},
		{name: "a function's substitution before a handler", script: "f() {\n\tx=$(date)\n}\ntrap 'exit 143' TERM\nf\n", finds: true},
		{name: "a one-line function's substitution", script: "f() { x=$(date); }\ntrap 'exit 143' TERM\n", finds: true},
		{name: "the guard line", script: shellSignalGuard + "\nx=$(date)\n"},
		{name: "the guard line inside a function", script: "f() (\n\t" + shellSignalGuard + "\n\tx=$(date)\n)\n"},
		{name: "signals ignored", script: "trap '' INT TERM HUP\nx=$(date)\n"},
		{name: "handlers reset", script: "trap - INT TERM\nx=$(date)\n"},
		{name: "an EXIT trap alone", script: "trap 'rm -rf \"$d\"' EXIT\nx=$(date)\n"},
		{name: "arithmetic after a handler", script: "trap 'exit 143' TERM\nx=$((1 + 1))\n"},
		{name: "a substitution before a handler", script: "x=$(date)\ntrap 'exit 143' TERM\n"},
		{name: "a substitution in a comment", script: "trap 'exit 143' TERM\n# x=$(date)\n"},
		{name: "a probed script's function", path: "e2e/scripts/android-smoke.sh", script: "f() {\n  x=\"$(date)\"\n}\ntrap 'f; exit 143' TERM\n"},
		{name: "a probed script's top level", path: "e2e/scripts/android-smoke.sh", script: "trap 'exit 143' TERM\nx=\"$(date)\"\n", finds: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			if path == "" {
				path = "scripts/example.sh"
			}
			findings, err := shellSignalFindings(path, []byte(tc.script))
			if err != nil {
				t.Fatal(err)
			}
			if got := len(findings) > 0; got != tc.finds {
				t.Fatalf("want a finding %v, got %q for:\n%s", tc.finds, findings, tc.script)
			}
		})
	}
	if _, err := shellSignalFindings("scripts/example.sh", []byte("f() {\n\tx=1\n")); err == nil {
		t.Fatal("a function with no end was read as complete")
	}
}

// shellSignalRun is one run of a script that a fake tool on PATH signals
// once, right after its first real call: mktemp has made the path but the
// script has not read its name yet, or a later command has done its work.
type shellSignalRun struct {
	name, script string
	args         []string
	tool         string
	// group sends the signal to the script's process group, as a terminal
	// does, rather than to the script alone.
	group bool
	// left lists globs that must match nothing after the run.
	left []string
	// check inspects the run's directory after the run.
	check func(t *testing.T, dir string)
}

// Each script that makes temporary paths or replaces an app bundle ends on
// HUP, INT and TERM with its cleanup done: by the signal itself or with 128
// plus its number, its temporary paths removed and a bundle it moved aside
// put back. The scripts run under `sh` from PATH, as CI and the release run
// them, so a PATH with another shell first runs them under that shell.
//
// INT goes to the process group, as a terminal sends it: bash 5.2, which
// sets no INT handler, lets a script go on after its foreground command
// handles an INT sent to the script alone and exits 0.
func TestShellScriptsCleanUpOnSignal(t *testing.T) {
	for _, need := range []string{"sh", "ps", "mktemp", "mkdir", "cp", "mv"} {
		if _, err := exec.LookPath(need); err != nil {
			t.Skip(need + " unavailable")
		}
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	installed := "{dir}/apps/Agent Overflow.app"
	runs := []shellSignalRun{
		{name: "test-ci-change.sh makes its directory", script: "scripts/test-ci-change.sh", tool: "mktemp"},
		{name: "test-ci-change.sh runs a command", script: "scripts/test-ci-change.sh", tool: "mkdir"},
		{name: "test-install-download.sh makes its directory", script: "scripts/test-install-download.sh", tool: "mktemp"},
		{name: "test-install-download.sh runs a command", script: "scripts/test-install-download.sh", tool: "mkdir"},
		{name: "install.sh makes its download directory", script: "scripts/install.sh", args: []string{"--macos", "--download", "--source", "{dir}/release"}, tool: "mktemp"},
		{name: "install.sh makes its bundle directory", script: "scripts/install.sh", args: []string{"--macos", "{dir}/Agent Overflow.app"}, tool: "mktemp", left: []string{"{dir}/home/Applications/.ao-bundle.*"}},
		{name: "install.sh copies the bundle", script: "scripts/install.sh", args: []string{"--macos", "{dir}/Agent Overflow.app"}, tool: "cp", left: []string{"{dir}/home/Applications/.ao-bundle.*", "{dir}/home/Applications/Agent Overflow.app"}},
		{name: "package-macos-app.sh makes its stage", script: "scripts/package-macos-app.sh", args: []string{"{dir}/binary", "{dir}/Info.plist", installed}, tool: "mktemp", left: []string{"{dir}/apps/.ao-bundle.*"}},
		{
			// The first mv moves the installed bundle aside for the new one.
			name: "package-macos-app.sh has moved the installed bundle aside", script: "scripts/package-macos-app.sh",
			args: []string{"{dir}/binary", "{dir}/Info.plist", installed}, tool: "mv", group: true,
			left: []string{"{dir}/apps/.ao-bundle.*", "{dir}/apps/.Agent Overflow.app.previous.*/*"},
			check: func(t *testing.T, dir string) {
				app := filepath.Join(dir, "apps", "Agent Overflow.app")
				if marker, err := os.ReadFile(filepath.Join(app, "Contents", "marker")); err != nil || string(marker) != app {
					t.Fatalf("the installed bundle was not put back: %q (%v)", marker, err)
				}
			},
		},
	}
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM} {
		for _, run := range runs {
			t.Run(shellSignalName(sig)+"/"+run.name, func(t *testing.T) {
				dir := t.TempDir()
				expand := func(s string) string { return strings.ReplaceAll(s, "{dir}", dir) }
				tmp := filepath.Join(dir, "tmp")
				for _, d := range []string{tmp, filepath.Join(dir, "home"), filepath.Join(dir, "release")} {
					if err := os.MkdirAll(d, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				// A bundle to install, and one already installed.
				for _, app := range []string{filepath.Join(dir, "Agent Overflow.app"), expand(installed)} {
					if err := os.MkdirAll(filepath.Join(app, "Contents"), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(app, "Contents", "marker"), []byte(app), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				for _, input := range []string{"binary", "Info.plist"} {
					if err := os.WriteFile(filepath.Join(dir, input), []byte(input), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				real, err := exec.LookPath(run.tool)
				if err != nil {
					t.Fatal(err)
				}
				target := `"$pgid"`
				if run.group || sig == syscall.SIGINT {
					target = `-- "-$pgid"`
				}
				fakes := filepath.Join(dir, "fakes")
				mockexec.WriteIn(t, fakes, run.tool, `#!/bin/sh
`+shellQuote(real)+` "$@"
status=$?
if [ ! -e "$AO_TEST_SIGNALLED" ]; then
  : >"$AO_TEST_SIGNALLED"
  pgid=$(ps -o pgid= -p $$ | tr -d ' ')
  kill -`+shellSignalName(sig)+` `+target+`
  sleep 0.3
fi
exit $status
`)
				args := []string{filepath.Join(root, run.script)}
				for _, arg := range run.args {
					args = append(args, expand(arg))
				}
				cmd := exec.Command("sh", args...)
				cmd.Env = append(os.Environ(),
					"PATH="+fakes+string(os.PathListSeparator)+os.Getenv("PATH"),
					"TMPDIR="+tmp,
					"HOME="+filepath.Join(dir, "home"),
					"AO_TEST_SIGNALLED="+filepath.Join(dir, "signalled"),
				)
				// A file, not a pipe: a fake the script left running holds a
				// pipe open.
				output, err := os.Create(filepath.Join(dir, "output"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = output.Close() })
				cmd.Stdout, cmd.Stderr = output, output
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
				err = cmd.Wait()
				// Under bash 5.2 the script can exit before a subshell's EXIT
				// trap is done, so the checks wait for what it started to end.
				deadline := time.Now().Add(10 * time.Second)
				for syscall.Kill(-cmd.Process.Pid, 0) == nil {
					if time.Now().After(deadline) {
						t.Fatal("the script's process group outlived it by 10s")
					}
					time.Sleep(20 * time.Millisecond)
				}
				printed, _ := os.ReadFile(output.Name())
				if _, statErr := os.Stat(filepath.Join(dir, "signalled")); statErr != nil {
					t.Fatalf("the script never ran %s: %v\n%s", run.tool, err, printed)
				}
				if !endedBySignal(err, sig) {
					t.Fatalf("want the script ended by %s, got %v:\n%s", shellSignalName(sig), err, printed)
				}
				for _, glob := range append([]string{filepath.Join(tmp, "*")}, run.left...) {
					if left, _ := filepath.Glob(expand(glob)); len(left) > 0 {
						t.Fatalf("the signal left %v behind:\n%s", left, printed)
					}
				}
				if run.check != nil {
					run.check(t, dir)
				}
			})
		}
	}
}

func shellSignalName(sig syscall.Signal) string {
	return map[syscall.Signal]string{syscall.SIGHUP: "HUP", syscall.SIGINT: "INT", syscall.SIGTERM: "TERM"}[sig]
}
