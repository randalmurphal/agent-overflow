//go:build unix

// The shell scripts in this repository end on HUP, INT and TERM with their
// cleanup done, run as sh or as bash 3.2 and later, or under dash. This file
// holds the rule and its checks.
//
// A script cleans up in its EXIT trap, and sets its HUP, INT and TERM
// handlers with this one line, on every shell:
//
//	trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' TERM
//
// A handler waits for the running command to end and then exits with 128
// plus the signal's number, which runs the EXIT trap. Without the handlers,
// dash runs no EXIT trap on these signals, bash 3.2 runs none in a subshell,
// and later bash runs it at once, beside the command that is still running.
// Bash 4.4 does not wait while it reads a command substitution's output: it
// runs the handler then, before the substitution's command has ended.
// Under dash a subshell does not keep its parent's handlers, so a subshell
// that cleans up in an EXIT trap of its own sets the line again, as
// publish_macos_bundle does.
//
// After the line, the script expands no $(...), <(...) or >(...), and holds
// none of "$(", "<(" or ">(" at all, comments and arithmetic included: bash
// 5.2 runs a handler that is pending when it starts to parse such an
// expansion in that expansion's parser state, where the handler fails and
// the signal is lost. Backquotes keep the signal. A function defined above
// the line can run after it, so it uses backquotes too.
//
// The EXIT trap is set before the script makes its first temporary path,
// and removes its temporary paths by a name prefix unique to the run, such
// as ${TMPDIR:-/tmp}/NAME.$$., so a signal while mktemp runs, before the
// script has the path's name, leaves nothing behind.
//
// Which check covers which scripts:
//   - TestShellScriptsFollowTheSignalRule holds every script that
//     shellScriptInventory lists to the rule; one that sets neither handlers
//     nor an EXIT trap is free of it. TestShellScriptsCleanUpOnSignal signals
//     each script that makes temporary paths or replaces an app bundle.
//   - The scripts in shellSignalForwarders set the one handler line named
//     there instead, and are otherwise held to the rule.
//   - The Android scripts in probedShellScripts are bash scripts with
//     handlers of their own that run functions using $(...). The substitution
//     probe in main_android_smoke_unix_test.go checks every command they run
//     while their handlers are set.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"mvdan.cc/sh/v3/syntax"

	"agent-overflow/internal/testutil/mockexec"
)

// shellSignalLine is the one line that sets a script's HUP, INT and TERM
// handlers.
const shellSignalLine = `trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' TERM`

// shellScriptsWithoutExtension are the shell scripts whose names do not end
// in .sh or .bash.
var shellScriptsWithoutExtension = []string{
	"mobile/android/gradlew",
	"scripts/perfprobe/probe",
}

// shellSignalForwarders set this handler line in place of shellSignalLine,
// with the reason.
var shellSignalForwarders = map[string]struct{ line, reason string }{
	"scripts/release-upgrade-check.sh": {
		line:   `trap 'signaled=1; kill -TERM "$run" 2>/dev/null || :' INT TERM HUP`,
		reason: "a signal to the script alone stops its background run, and the script waits for the run to end so it removes the sandbox after it",
	},
}

// probedShellScripts are the bash scripts that the substitution probe checks
// while they run.
var probedShellScripts = map[string]bool{
	"e2e/scripts/android-smoke.sh":    true,
	"e2e/scripts/android-emulator.sh": true,
}

// shellSubstitutionText is what the text after the handler line must not
// hold.
var shellSubstitutionText = regexp.MustCompile(`\$\(|<\(|>\(`)

// shellSignalFindings checks one script against the rule and returns what
// breaks it, one line each.
func shellSignalFindings(path string, body []byte) ([]string, error) {
	file, err := syntax.NewParser().Parse(bytes.NewReader(body), path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(body), "\n")
	allowed, why := shellSignalLine, "set them with that exact line, on a line of its own"
	if forwarder, ok := shellSignalForwarders[path]; ok {
		allowed, why = forwarder.line, "this script is in shellSignalForwarders: "+forwarder.reason
	}
	var findings []string
	at := func(line uint) string { return path + ":" + strconv.Itoa(int(line)) + ": " }
	handlersAt, exitAt := uint(0), uint(0)
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 || call.Args[0].Lit() != "trap" {
			return true
		}
		line := call.Pos().Line()
		names, literal := shellTrapSignals(call.Args[1:])
		if !literal {
			findings = append(findings, at(line)+"names its signals through an expansion; name HUP, INT and TERM only in the line "+allowed)
			return true
		}
		if names["EXIT"] && exitAt == 0 {
			exitAt = line
		}
		if !names["HUP"] && !names["INT"] && !names["TERM"] {
			return true
		}
		if strings.TrimSpace(lines[line-1]) != allowed {
			findings = append(findings, at(line)+"traps HUP, INT or TERM other than by the line "+allowed+"; "+why)
			return true
		}
		if handlersAt == 0 || line < handlersAt {
			handlersAt = line
		}
		return true
	})
	if handlersAt == 0 {
		if exitAt != 0 {
			findings = append(findings, at(exitAt)+"sets an EXIT trap without the HUP, INT and TERM handlers, so dash and a bash 3.2 subshell skip it on those signals; add the line "+shellSignalLine)
		}
		return findings, nil
	}
	for number := handlersAt + 1; number <= uint(len(lines)); number++ {
		if found := shellSubstitutionText.FindString(lines[number-1]); found != "" {
			findings = append(findings, at(number)+found+" after the handlers set at line "+strconv.Itoa(int(handlersAt))+
				"; write a substitution with backquotes, a nested one as an assignment of its own, or move the code above that line, and rewrite a comment or literal that holds "+found)
		}
	}
	for _, stmt := range file.Stmts {
		function, ok := stmt.Cmd.(*syntax.FuncDecl)
		if !ok || function.Pos().Line() >= handlersAt {
			continue
		}
		syntax.Walk(function.Body, func(node syntax.Node) bool {
			switch expansion := node.(type) {
			case *syntax.CmdSubst:
				if !expansion.Backquotes {
					findings = append(findings, at(expansion.Pos().Line())+"$( in "+function.Name.Value+", which is defined above the handlers set at line "+strconv.Itoa(int(handlersAt))+" and can run after them; write it with backquotes")
				}
			case *syntax.ProcSubst:
				findings = append(findings, at(expansion.Pos().Line())+"a process substitution in "+function.Name.Value+", which is defined above the handlers set at line "+strconv.Itoa(int(handlersAt))+" and can run after them")
			}
			return true
		})
	}
	return findings, nil
}

// shellTrapSignals returns the conditions that a trap command's arguments
// name, by name without SIG, and whether every one is a literal.
func shellTrapSignals(args []*syntax.Word) (map[string]bool, bool) {
	if len(args) > 0 && args[0].Lit() == "--" {
		args = args[1:]
	}
	if len(args) == 0 || args[0].Lit() == "-p" || args[0].Lit() == "-l" {
		return nil, true
	}
	numbers := map[string]string{"0": "EXIT", "1": "HUP", "2": "INT", "15": "TERM"}
	names := map[string]bool{}
	for _, arg := range args[1:] {
		name := strings.TrimPrefix(strings.ToUpper(arg.Lit()), "SIG")
		if name == "" {
			return nil, false
		}
		if byNumber, ok := numbers[name]; ok {
			name = byNumber
		}
		names[name] = true
	}
	return names, true
}

// shellScriptInventory lists the shell scripts in the tree, tracked or not
// yet added: the *.sh and *.bash files and shellScriptsWithoutExtension. It
// reports an executable file outside that list whose interpreter line names
// a shell, and a listed name that is not a shell script.
func shellScriptInventory(t *testing.T) (scripts, problems []string) {
	t.Helper()
	out, err := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		t.Fatalf("list the tree's files: %v", err)
	}
	listed := map[string]bool{}
	for _, path := range shellScriptsWithoutExtension {
		listed[path] = true
	}
	interpreter := regexp.MustCompile(`^#!\s*\S*/(?:env\s+)?(?:ba|da)?sh\b`)
	isShell := func(path string) bool {
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		head := make([]byte, 64)
		n, err := file.Read(head)
		if closeErr := file.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil && n == 0 {
			return false
		}
		return interpreter.Match(head[:n])
	}
	for _, path := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if strings.HasSuffix(path, ".sh") || strings.HasSuffix(path, ".bash") {
			scripts = append(scripts, path)
			continue
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue // deleted in the working tree
		}
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case listed[path]:
			delete(listed, path)
			if !isShell(path) {
				problems = append(problems, path+" is in shellScriptsWithoutExtension but is not a shell script")
				continue
			}
			scripts = append(scripts, path)
		case info.Mode().IsRegular() && info.Mode()&0o111 != 0 && isShell(path):
			problems = append(problems, path+" is a shell script; name it .sh or add it to shellScriptsWithoutExtension")
		}
	}
	for path := range listed {
		problems = append(problems, path+" is in shellScriptsWithoutExtension but not in the tree")
	}
	return scripts, problems
}

func TestShellScriptsFollowTheSignalRule(t *testing.T) {
	scripts, findings := shellScriptInventory(t)
	if len(scripts) == 0 {
		t.Fatal("found no shell scripts in the tree")
	}
	for _, path := range scripts {
		if probedShellScripts[path] {
			continue
		}
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

// The rule finds what can lose a signal or skip the cleanup, and passes the
// handler line with backquotes after it.
func TestShellSignalFindings(t *testing.T) {
	forwarder := "scripts/release-upgrade-check.sh"
	for _, tc := range []struct {
		name, path, script string
		finds              bool
	}{
		{name: "backquotes after the line", script: shellSignalLine + "\nx=`date`\ny=\"`date``date`\"\n"},
		{name: "a substitution before the line", script: "x=$(date)\n" + shellSignalLine + "\n"},
		{name: "the line inside a function, first", script: "f() (\n\tset -eu\n\t" + shellSignalLine + "\n\tx=`date`\n)\n"},
		{name: "a function above the line with backquotes", script: "f() {\n\tx=`date`\n}\n" + shellSignalLine + "\nf\n"},
		{name: "an EXIT trap with the line", script: "trap 'rm -rf \"$d\"' EXIT\n" + shellSignalLine + "\n"},
		{name: "a script with neither", script: "x=$(date)\ndiff <(a) <(b)\n"},
		{name: "the line in a heredoc only", script: "cat <<EOF\n" + shellSignalLine + "\nEOF\nx=$(date)\n"},
		{name: "a substitution after the line", script: shellSignalLine + "\nx=$(date)\n", finds: true},
		{name: "one of two nested in backquotes", script: shellSignalLine + "\nx=`dirname \"$(pwd)\"`\n", finds: true},
		{name: "a process substitution after the line", script: shellSignalLine + "\ndiff <(a) <(b)\n", finds: true},
		{name: "arithmetic after the line", script: shellSignalLine + "\nx=$((1 + 1))\n", finds: true},
		{name: "a comment after the line", script: shellSignalLine + "\n# x=$(date)\n", finds: true},
		{name: "a function above the line with $(", script: "f() {\n\tx=$(date)\n}\n" + shellSignalLine + "\nf\n", finds: true},
		{name: "a one-line function above the line with $(", script: "f() { x=$(date); }\n" + shellSignalLine + "\n", finds: true},
		{name: "a handler of its own", script: "trap cleanup TERM\n", finds: true},
		{name: "handlers by number", script: "trap 'exit 1' 1 2 15\n", finds: true},
		{name: "the line after another command", script: "set -eu; " + shellSignalLine + "\n", finds: true},
		{name: "the line split by a continuation", script: "trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' \\\n\tTERM\n", finds: true},
		{name: "a handler quoted across lines", script: "trap '\n\texit 143' TERM\n", finds: true},
		{name: "a signal ignored", script: shellSignalLine + "\ntrap '' INT\n", finds: true},
		{name: "a handler reset", script: shellSignalLine + "\ntrap - TERM\n", finds: true},
		{name: "signals through an expansion", script: "trap 'exit 1' $signals\n", finds: true},
		{name: "an EXIT trap without the line", script: "trap 'rm -rf \"$d\"' EXIT\n", finds: true},
		{name: "a forwarder's line", path: forwarder, script: "run=$!\n" + shellSignalForwarders[forwarder].line + "\nwait \"$run\"\n"},
		{name: "a forwarder's substitution after its line", path: forwarder, script: shellSignalForwarders[forwarder].line + "\nx=$(date)\n", finds: true},
		{name: "a forwarder with the line of other scripts", path: forwarder, script: shellSignalLine + "\n", finds: true},
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
		t.Fatal("a script that does not parse was read as complete")
	}
}

// A TERM that lands during a backquote substitution, with the handlers set,
// ends the script before its next command, under the local sh. Bash 5.2
// loses it when the same word goes on to a $(...).
func TestShellBackquotesKeepAPendingSignal(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	script := filepath.Join(t.TempDir(), "gate.sh")
	body := shellSignalLine + "\nx=\"`kill -TERM $$``true`\"\necho survived\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", script).CombinedOutput()
	if !endedBySignal(err, syscall.SIGTERM) || strings.Contains(string(out), "survived") {
		t.Fatalf("want the script ended by TERM, got %v:\n%s", err, out)
	}
}

// shellSignalRun is one run of a script in which a fake tool on PATH stops
// at its first call, after the real tool has done its work, while the test
// signals the script.
type shellSignalRun struct {
	name, script string
	args         []string
	// tool is the fake. It runs the real tool of that name, or true for one
	// the machine need not have.
	tool string
	real string
	// left lists globs that must match nothing after the run.
	left []string
	// check inspects the run's directory after the run; group says whether
	// the signal went to the process group.
	check func(t *testing.T, dir string, group bool)
}

// Each script that makes temporary paths or replaces an app bundle ends on
// HUP, INT and TERM with its cleanup done, whether the signal goes to the
// script alone or to its process group, as a terminal sends it. A script
// signalled alone waits for the running command: it neither exits nor cleans
// up before the command ends. The script then ends by the signal or with
// 128 plus its number, nothing it started still runs, its temporary paths
// are gone, and a bundle it moved aside is back.
//
// The scripts run under `sh` from PATH, as CI and the release run them, so a
// PATH with another shell first runs them under that shell. The fake tells
// the test through a FIFO that it is running and blocks on a second FIFO
// until the test releases it or the signal ends it; /proc shows when the
// script has taken a signal sent to it alone.
func TestShellScriptsCleanUpOnSignal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc to see the script take the signal and to list its process group")
	}
	for _, need := range []string{"sh", "mktemp", "mkdir", "cp", "mv", "git"} {
		if _, err := exec.LookPath(need); err != nil {
			t.Skip(need + " unavailable")
		}
	}
	signals := []syscall.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM}
	_, masks, err := shellProcStatus(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for _, sig := range signals {
		if masks["SigIgn"]&(1<<(sig-1)) != 0 {
			t.Fatalf("this test process ignores SIG%s, and the scripts it starts inherit that and cannot set a handler for it; run the tests where it is not ignored (a non-interactive shell's background job ignores INT)", shellSignalName(sig))
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
		{name: "release-check.sh makes its directory", script: "scripts/release-check.sh", tool: "mktemp"},
		{name: "install.sh makes its download directory", script: "scripts/install.sh", args: []string{"--macos", "--download", "--source", "{dir}/release"}, tool: "mktemp"},
		{name: "install.sh downloads from GitLab", script: "scripts/install.sh", args: []string{"--macos", "--source", "gitlab:example.test/group/agent-overflow", "--version", "0.0.1"}, tool: "glab", real: "true"},
		{name: "install.sh makes its bundle directory", script: "scripts/install.sh", args: []string{"--macos", "{dir}/Agent Overflow.app"}, tool: "mktemp", left: []string{"{dir}/home/Applications/.ao-bundle.*"}},
		{name: "install.sh copies the bundle", script: "scripts/install.sh", args: []string{"--macos", "{dir}/Agent Overflow.app"}, tool: "cp", left: []string{"{dir}/home/Applications/.ao-bundle.*", "{dir}/home/Applications/Agent Overflow.app"}},
		{name: "package-macos-app.sh makes its stage", script: "scripts/package-macos-app.sh", args: []string{"{dir}/binary", "{dir}/Info.plist", installed}, tool: "mktemp", left: []string{"{dir}/apps/.ao-bundle.*"}},
		{
			// The first mv moves the installed bundle aside for the new one.
			// A signal to the group stops the publish and puts the installed
			// bundle back; the publish runs on through a signal to the script
			// alone.
			name: "package-macos-app.sh has moved the installed bundle aside", script: "scripts/package-macos-app.sh",
			args: []string{"{dir}/binary", "{dir}/Info.plist", installed}, tool: "mv",
			left: []string{"{dir}/apps/.ao-bundle.*", "{dir}/apps/.Agent Overflow.app.previous.*/*"},
			check: func(t *testing.T, dir string, group bool) {
				app := filepath.Join(dir, "apps", "Agent Overflow.app")
				if !group {
					if binary, err := os.ReadFile(filepath.Join(app, "Contents", "MacOS", "agent-overflow")); err != nil || string(binary) != "binary" {
						t.Fatalf("the new bundle was not published: %q (%v)", binary, err)
					}
					return
				}
				if marker, err := os.ReadFile(filepath.Join(app, "Contents", "marker")); err != nil || string(marker) != app {
					t.Fatalf("the installed bundle was not put back: %q (%v)", marker, err)
				}
			},
		},
	}
	for _, sig := range signals {
		for _, group := range []bool{false, true} {
			target := "the script"
			if group {
				target = "the group"
			}
			for _, run := range runs {
				t.Run(shellSignalName(sig)+"/"+target+"/"+run.name, func(t *testing.T) {
					runShellSignalCase(t, root, installed, run, sig, group)
				})
			}
		}
	}
}

func runShellSignalCase(t *testing.T, root, installed string, run shellSignalRun, sig syscall.Signal, group bool) {
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
	real := run.real
	if real == "" {
		real = run.tool
	}
	real, err := exec.LookPath(real)
	if err != nil {
		t.Fatal(err)
	}
	// The FIFOs are opened read-write, so neither side's open blocks: the
	// fake's write to announce, and its read of release, which returns at the
	// line the test writes.
	fifo := func(name string) *os.File {
		path := filepath.Join(dir, name)
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		return file
	}
	announce, release := fifo("announce.fifo"), fifo("release.fifo")
	findings := filepath.Join(dir, "findings")
	// The path the tool made or wrote: mktemp's output, or the last argument.
	watched := `for watched; do :; done`
	if run.tool == "mktemp" {
		watched = `watched=$out`
	}
	fakes := filepath.Join(dir, "fakes")
	mockexec.WriteIn(t, fakes, run.tool, `#!/bin/sh
[ ! -e "$AO_TEST_SEEN" ] || exec `+shellQuote(real)+` "$@"
: >"$AO_TEST_SEEN"
out=$(`+shellQuote(real)+` "$@")
status=$?
`+watched+`
echo $$ >"$AO_TEST_ANNOUNCE"
read -r _ <"$AO_TEST_RELEASE" || :
[ -e "$watched" ] || echo "$watched was removed while `+run.tool+` ran" >>"$AO_TEST_FINDINGS"
[ -z "$out" ] || printf '%s\n' "$out"
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
		"AO_TEST_SEEN="+filepath.Join(dir, "seen"),
		"AO_TEST_ANNOUNCE="+announce.Name(),
		"AO_TEST_RELEASE="+release.Name(),
		"AO_TEST_FINDINGS="+findings,
	)
	// A file, not a pipe: a process the script left running would hold a pipe
	// open.
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
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	printed := func() string {
		out, _ := os.ReadFile(output.Name())
		return string(out)
	}
	ended := make(chan error, 1)
	go func() { ended <- cmd.Wait() }()
	running := make(chan string, 1)
	go func() {
		if line, err := bufio.NewReader(announce).ReadString('\n'); err == nil {
			running <- strings.TrimSpace(line)
		}
	}()
	var fake int
	select {
	case line := <-running:
		if fake, err = strconv.Atoi(line); err != nil {
			t.Fatalf("the fake %s announced %q, want its pid", run.tool, line)
		}
	case err := <-ended:
		t.Fatalf("the script ended before it ran %s: %v\n%s", run.tool, err, printed())
	case <-time.After(time.Minute):
		t.Fatalf("the script did not run %s within a minute:\n%s", run.tool, printed())
	}
	if group {
		if err := syscall.Kill(-pid, sig); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := syscall.Kill(pid, sig); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Minute)
		for {
			select {
			case err := <-ended:
				t.Fatalf("the script ended while %s was still running: %v\n%s", run.tool, err, printed())
			default:
			}
			if held, err := shellSignalHeld(pid, fake, sig); err != nil {
				t.Fatal(err)
			} else if held {
				break
			}
			if time.Now().After(deadline) {
				group, _ := shellGroup(pid)
				t.Fatalf("the script did not settle to wait for %s with %s taken or blocked within a minute; its group: %v\n%s", run.tool, shellSignalName(sig), group, printed())
			}
			time.Sleep(time.Millisecond)
		}
		if _, err := release.WriteString("\n"); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err = <-ended:
	case <-time.After(time.Minute):
		t.Fatalf("the script did not end within a minute of the signal:\n%s", printed())
	}
	survivors, groupErr := shellGroup(pid)
	if groupErr != nil {
		t.Fatal(groupErr)
	}
	if len(survivors) > 0 {
		t.Fatalf("the script exited with these still running: %v\n%s", survivors, printed())
	}
	if !endedBySignal(err, sig) {
		t.Fatalf("want the script ended by %s, got %v:\n%s", shellSignalName(sig), err, printed())
	}
	if found, _ := os.ReadFile(findings); len(found) > 0 {
		t.Fatalf("the script cleaned up while a command was running: %s\n%s", found, printed())
	}
	for _, glob := range append([]string{filepath.Join(tmp, "*")}, run.left...) {
		if left, _ := filepath.Glob(expand(glob)); len(left) > 0 {
			t.Fatalf("the signal left %v behind:\n%s", left, printed())
		}
	}
	if run.check != nil {
		run.check(t, dir, group)
	}
}

// shellProcStatus reads /proc/PID/status: the process's state and its
// signal masks by name, such as SigIgn.
func shellProcStatus(pid int) (state string, masks map[string]uint64, err error) {
	status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return "", nil, err
	}
	masks = map[string]uint64{}
	for _, line := range strings.Split(string(status), "\n") {
		key, value, _ := strings.Cut(line, ":")
		value = strings.TrimSpace(value)
		switch key {
		case "State":
			state = value
		case "SigPnd", "ShdPnd", "SigBlk", "SigIgn":
			mask, err := strconv.ParseUint(value, 16, 64)
			if err != nil {
				return "", nil, fmt.Errorf("/proc/%d/status: %s: %w", pid, key, err)
			}
			masks[key] = mask
		}
	}
	return state, masks, nil
}

// shellSignalHeld reports whether the script pid has done what it can with
// sig while the fake runs: pid sleeps, sig is taken or pending and blocked
// (bash 5.2 and later block INT while they read a command substitution), and
// pid's process group runs nothing but pid, the fake and the processes
// between them. A script that exits holds nothing; the caller sees the exit.
func shellSignalHeld(pid, fake int, sig syscall.Signal) (bool, error) {
	state, masks, err := shellProcStatus(pid)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	bit := uint64(1) << (sig - 1)
	pending := (masks["SigPnd"] | masks["ShdPnd"]) & bit
	if !strings.HasPrefix(state, "S") || pending != 0 && masks["SigBlk"]&bit == 0 {
		return false, nil
	}
	group, err := shellGroup(pid)
	if err != nil {
		return false, err
	}
	for p := fake; p != pid; {
		process, ok := group[p]
		if !ok {
			return false, nil
		}
		delete(group, p)
		p = process.ppid
	}
	delete(group, pid)
	return len(group) == 0, nil
}

// shellProcess is a process in a script's process group.
type shellProcess struct {
	ppid int
	name string
}

func (p shellProcess) String() string { return p.name }

// shellGroup reads the processes in process group pgid that have not exited
// from /proc. An exited child that its new parent has not reaped yet runs
// nothing.
func shellGroup(pgid int) (map[int]shellProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	group := map[int]shellProcess{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// pid (comm) state ppid pgrp ...; comm can hold spaces and parentheses.
		start, end := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')')
		if start < 0 || end < start {
			return nil, fmt.Errorf("/proc/%d/stat: unexpected %q", pid, stat)
		}
		fields := strings.Fields(string(stat[end+1:]))
		if len(fields) < 3 {
			return nil, fmt.Errorf("/proc/%d/stat: unexpected %q", pid, stat)
		}
		if fields[2] != strconv.Itoa(pgid) || fields[0] == "Z" {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("/proc/%d/stat: %w", pid, err)
		}
		group[pid] = shellProcess{ppid: ppid, name: entry.Name() + " " + string(stat[start:end+1])}
	}
	return group, nil
}

func shellSignalName(sig syscall.Signal) string {
	return map[syscall.Signal]string{syscall.SIGHUP: "HUP", syscall.SIGINT: "INT", syscall.SIGTERM: "TERM"}[sig]
}
