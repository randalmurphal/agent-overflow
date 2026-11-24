// Package providerpath keeps a test binary from running an installed claude
// or codex. Code under test that resolves a bare provider name, such as the
// default binary settings, finds a poisoned executable first on PATH instead
// of the developer's CLI. The poison records the attempt and holds its caller
// while the test process writes down which goroutines are waiting on a child
// process and which tests are running; then it exits 127 and the run fails
// with that evidence. Its Guard also fails a run that leaves a process
// running after the tests (leftovers_linux.go).
//
// The package depends only on the standard library and low-level internal
// packages that import no test fixture, so any package with provider spawn
// code can use it from TestMain, including the packages the shared fixtures
// import.
package providerpath

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/ownedtemp"
	"agent-overflow/internal/procutil"
	"agent-overflow/internal/stringsx"
)

// Names are the provider executables poisoned on PATH.
var Names = []string{"claude", "codex", "agy"}

const (
	// recordName is the file each poisoned executable appends its attempt to.
	recordName = "provider-path-attempts"
	// ackDirName holds one file per poison process, named by its pid, that
	// the watcher creates once it has captured the stacks for it.
	ackDirName = "ack"
	// watchInterval is how often the watcher checks the record. A poison
	// waits for its ack, so this bounds how long it holds its caller, not
	// whether the stacks are captured.
	watchInterval = 20 * time.Millisecond
	// maxStackCaptures bounds the goroutine dumps per run. A path that
	// reaches the poison on every tick would otherwise dump on every tick;
	// the first captures name it.
	maxStackCaptures = 3
)

// Run is a TestMain body for a package that needs nothing else:
// os.Exit(providerpath.Run(m)).
func Run(m *testing.M) int { return Guard(m.Run) }

// Guard runs a TestMain's test run, such as storetest.Run(m), with the
// poison installed, and turns any attempt it recorded into a failed run. On
// Linux it also fails the run when a process a test started is still
// running five seconds after the tests, naming and killing it
// (markProcesses), so the verdict is the test binary's and go test never
// caches a run that left one.
func Guard(run func() int) int {
	leftovers, err := markProcesses()
	if err != nil {
		fmt.Fprintf(os.Stderr, "providerpath: %v\n", err)
		return 1
	}
	check, err := Install()
	if err != nil {
		fmt.Fprintf(os.Stderr, "providerpath: %v\n", err)
		return 1
	}
	code := run()
	if err := errors.Join(check(), leftovers()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// shellName is the stand-in Install points SHELL at.
const shellName = "login-shell"

// writeExecutables writes the poison under each name and the stand-in shell
// in dir and returns the shell's path.
func writeExecutables(dir string) (shell string, err error) {
	for _, name := range Names {
		if err := procutil.InstallExecutable(filepath.Join(dir, name), []byte(poisonScript(dir)), 0o755); err != nil {
			return "", fmt.Errorf("write poisoned %s: %w", name, err)
		}
	}
	shell = filepath.Join(dir, shellName)
	if err := procutil.InstallExecutable(shell, []byte(standInShell), 0o755); err != nil {
		return "", fmt.Errorf("write stand-in shell: %w", err)
	}
	return shell, nil
}

// Install prepends a directory of poisoned provider executables to PATH for
// the rest of the process, and points SHELL at a stand-in that runs /bin/sh
// without the login and interactive flags. A login shell's startup files can
// put an installed CLI ahead of the poison, and the boot's shell sync puts
// the login shell's PATH first; the stand-in reports the process's own PATH,
// so the sync cannot reorder it. It also starts the watcher that captures
// the stacks behind each attempt. The returned check, called after m.Run,
// stops the watcher, reports every attempt with its evidence and removes the
// directory, and those of test processes that exited without their check
// (ownedtemp). On Windows, where the poison cannot be a shell script, Install
// changes nothing.
func Install() (check func() error, err error) {
	if runtime.GOOS == "windows" {
		return func() error { return nil }, nil
	}
	sweepDirsOfExitedProcesses()
	dir, release, err := ownedtemp.Make("", dirPrefix)
	if err != nil {
		return nil, fmt.Errorf("create poisoned provider PATH: %w", err)
	}
	if err := os.Mkdir(filepath.Join(dir, ackDirName), 0o755); err != nil {
		return nil, errors.Join(fmt.Errorf("create poison ack directory: %w", err), release())
	}
	shell, err := writeExecutables(dir)
	if err != nil {
		return nil, errors.Join(err, release())
	}
	if err := os.Setenv("SHELL", shell); err != nil {
		return nil, errors.Join(fmt.Errorf("set SHELL: %w", err), release())
	}
	path := dir
	if existing := os.Getenv("PATH"); existing != "" {
		path += string(os.PathListSeparator) + existing
	}
	if err := os.Setenv("PATH", path); err != nil {
		return nil, errors.Join(fmt.Errorf("set PATH: %w", err), release())
	}
	w := startWatcher(dir)
	return func() error {
		attempts, watchErr := w.stop()
		removeErr := release()
		// Helper processes this binary started and killed, or that ended
		// in os.Exit, made directories of their own.
		sweepDirsOfExitedProcesses()
		return errors.Join(watchErr, attemptsError(attempts), removeErr)
	}, nil
}

// dirPrefix names the poisoned PATH directories, one per test process.
const dirPrefix = "agent-overflow-provider-path-"

// sweepDirsOfExitedProcesses removes the poisoned PATH directories of test
// processes that are gone without their check: killed, timed out, or a
// helper process that ended in os.Exit. A failure is reported, not fatal:
// it leaves a directory, not a wrong result.
func sweepDirsOfExitedProcesses() {
	if err := ownedtemp.Sweep("", dirPrefix); err != nil {
		fmt.Fprintln(os.Stderr, "providerpath: remove the directories of exited test processes:", err)
	}
}

// poisonScript records the attempt with what can name the test: the
// argument list, the working directory and the provider home variables,
// which a test points at its own t.TempDir, plus its pid and parent pid.
// It then holds its caller in the wait for the child until the watcher acks
// its pid, gives up after about 15 seconds, or sees the directory removed
// because the run ended. The record uses builtins only, so a test that
// narrows PATH cannot break it; the wait sleeps through /bin/sleep and
// stops waiting if that is missing.
func poisonScript(dir string) string {
	return "#!/bin/sh\n" +
		"dir=" + stringsx.ShellQuote(dir) + "\n" +
		"line=\"ran $0\"\n" +
		"for arg in \"$@\"; do line=\"$line $arg\"; done\n" +
		"# One printf, one append: concurrent attempts do not interleave.\n" +
		"printf '%s\\n  pid=%s ppid=%s pwd=%s HOME=%s CODEX_HOME=%s CLAUDE_CONFIG_DIR=%s\\n' \"$line\" \"$$\" \"$PPID\" \"$PWD\" \"$HOME\" \"${CODEX_HOME:-}\" \"${CLAUDE_CONFIG_DIR:-}\" >> \"$dir/" + recordName + "\"\n" +
		"echo \"agent-overflow tests: refused to run the installed ${0##*/} found on PATH; give the test a mock binary\" >&2\n" +
		"i=0\n" +
		"while [ -d \"$dir\" ] && [ ! -e \"$dir/" + ackDirName + "/$$\" ] && [ \"$i\" -lt 300 ]; do\n" +
		"  /bin/sleep 0.05 </dev/null >/dev/null 2>&1 || break\n" +
		"  i=$((i + 1))\n" +
		"done\n" +
		"exit 127\n"
}

// standInShell drops the login and interactive flags from the options
// before the first operand, keeps -c, and runs /bin/sh with the rest. With
// no arguments it is an interactive /bin/sh, as a terminal opens it.
const standInShell = `#!/bin/sh
options=1
first=1
for arg do
  if [ "$first" = 1 ]; then set --; first=0; fi
  if [ "$options" = 1 ]; then
    case "$arg" in
      --login|-l|-i|-il|-li) continue ;;
      -c|-ilc|-lic|-icl|-cil|-lci|-cli|-lc|-cl|-ic|-ci) set -- "$@" -c; options=0; continue ;;
      -*) ;;
      *) options=0 ;;
    esac
  fi
  set -- "$@" "$arg"
done
exec /bin/sh "$@"
`

// attempt is one recorded run of the poison and the evidence captured
// while it held its caller.
type attempt struct {
	// record is the two lines the poison wrote.
	record string
	pid    int
	ppid   int
	// evidence is the stack report, or why there is none, as indented
	// lines that each end in a newline.
	evidence string
}

// watcher polls the record and, for each new attempt, captures every
// goroutine's stack while the poison holds its caller, then acks the
// poison so it exits.
type watcher struct {
	dir     string
	selfPID int
	done    chan struct{}
	exited  chan struct{}

	// Owned by the watcher goroutine until exited is closed.
	read     int
	attempts []attempt
	captures int
	err      error
	stopOnce sync.Once
}

func startWatcher(dir string) *watcher {
	w := &watcher{dir: dir, selfPID: os.Getpid(), done: make(chan struct{}), exited: make(chan struct{})}
	go w.run()
	return w
}

func (w *watcher) run() {
	defer close(w.exited)
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.done:
			// An attempt recorded after the last tick is reported, with
			// no stacks: its caller may be gone by now.
			w.scan(false)
			return
		case <-ticker.C:
			w.scan(true)
		}
	}
}

// stop ends the watcher and returns every attempt it saw.
func (w *watcher) stop() ([]attempt, error) {
	w.stopOnce.Do(func() { close(w.done) })
	<-w.exited
	return w.attempts, w.err
}

// scan reads attempts appended since the last scan. A record is only read
// once it is complete, two lines per attempt, so a partial append waits for
// the next tick.
func (w *watcher) scan(capture bool) {
	data, err := os.ReadFile(filepath.Join(w.dir, recordName))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		if w.err == nil {
			w.err = fmt.Errorf("providerpath: read attempts: %w", err)
		}
		return
	}
	var fresh []attempt
	for {
		rest := data[w.read:]
		first := bytes.IndexByte(rest, '\n')
		if first < 0 {
			break
		}
		second := bytes.IndexByte(rest[first+1:], '\n')
		if second < 0 {
			break
		}
		end := first + 1 + second + 1
		fresh = append(fresh, parseAttempt(string(rest[:end-1])))
		w.read += end
	}
	if len(fresh) == 0 {
		return
	}
	var evidence string
	switch {
	case !capture:
		evidence = "  no stacks: recorded as the run ended\n"
	case w.captures >= maxStackCaptures:
		evidence = fmt.Sprintf("  no stacks: the first %d captures are above\n", maxStackCaptures)
	default:
		w.captures++
		evidence = stackEvidence(allStacks())
	}
	for i := range fresh {
		a := &fresh[i]
		a.evidence = evidence
		if capture && a.ppid != w.selfPID {
			a.evidence = fmt.Sprintf("  parent pid %d is not this test process (pid %d): a child process of a test ran it\n%s",
				a.ppid, w.selfPID, evidence)
		}
		if a.pid > 0 {
			if err := os.WriteFile(filepath.Join(w.dir, ackDirName, strconv.Itoa(a.pid)), nil, 0o644); err != nil && w.err == nil {
				w.err = fmt.Errorf("providerpath: release poison pid %d: %w", a.pid, err)
			}
		}
	}
	w.attempts = append(w.attempts, fresh...)
}

var recordIDPattern = regexp.MustCompile(`\bpid=(\d+) ppid=(\d+)\b`)

func parseAttempt(record string) attempt {
	a := attempt{record: record}
	if m := recordIDPattern.FindStringSubmatch(record); m != nil {
		a.pid, _ = strconv.Atoi(m[1])
		a.ppid, _ = strconv.Atoi(m[2])
	}
	return a
}

// allStacks returns every goroutine's stack, as a panic prints them.
func allStacks() string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// stackEvidence names what the dump says about the caller: the goroutines
// waiting on a child process, one of which started the poison, with their
// creation sites, and the tests that were running.
func stackEvidence(dump string) string {
	var waiting []string
	var running []string
	// testOf maps a goroutine id to the test it runs, so a waiting
	// goroutine's creator can be named when it is a test still running.
	testOf := map[string]string{}
	live := map[string]bool{}
	for _, block := range strings.Split(dump, "\n\n") {
		id := goroutineID(block)
		live[id] = true
		if strings.Contains(block, "os.(*Process).Wait(") {
			waiting = append(waiting, block)
		}
		if name := runningTest(block); name != "" {
			testOf[id] = name
			if !slices.Contains(running, name) {
				running = append(running, name)
			}
		}
	}
	var b strings.Builder
	b.WriteString("  tests running: ")
	if len(running) == 0 {
		b.WriteString("none")
	} else {
		b.WriteString(strings.Join(running, ", "))
	}
	b.WriteString("\n")
	if len(waiting) == 0 {
		b.WriteString("  no goroutine was waiting on a child process: the caller started it without waiting for it\n")
		return b.String()
	}
	fmt.Fprintf(&b, "  goroutines waiting on a child process, one of which ran it (%d):\n", len(waiting))
	for _, block := range waiting {
		b.WriteString(indent(block, "    "))
		b.WriteString("\n")
		creator := creatorID(block)
		switch {
		case creator == "":
		case testOf[creator] != "":
			fmt.Fprintf(&b, "    goroutine %s runs %s\n", creator, testOf[creator])
		case !live[creator] && !strings.Contains(block, "[originating from goroutine "):
			fmt.Fprintf(&b, "    goroutine %s has exited; rerun with GODEBUG=tracebackancestors=10 to print its stack\n", creator)
		}
	}
	return b.String()
}

var (
	goroutineHeader = regexp.MustCompile(`^goroutine (\d+) `)
	createdBy       = regexp.MustCompile(`(?m)^created by .* in goroutine (\d+)$`)
)

func goroutineID(block string) string {
	if m := goroutineHeader.FindStringSubmatch(block); m != nil {
		return m[1]
	}
	return ""
}

// creatorID is the goroutine that started this one, from its first
// "created by" line; ancestor stacks, when printed, follow it.
func creatorID(block string) string {
	if m := createdBy.FindStringSubmatch(block); m != nil {
		return m[1]
	}
	return ""
}

// runningTest returns the test function a goroutine runs, from the frame
// testing.tRunner called, or "" for a goroutine that runs no test or a test
// paused in t.Parallel.
func runningTest(block string) string {
	if strings.Contains(block, "testing.(*T).Parallel(") {
		return ""
	}
	lines := strings.Split(block, "\n")
	caller := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "goroutine ") || strings.HasPrefix(line, "created by ") {
			continue
		}
		if strings.HasPrefix(line, "testing.tRunner(") {
			return testFunctionName(caller)
		}
		caller = line
	}
	return ""
}

// testFunctionName turns a frame such as
// "agent-overflow/internal/app.TestX.func1(0xc000)" into "TestX.func1".
func testFunctionName(frame string) string {
	if i := strings.LastIndexByte(frame, '('); i > 0 {
		frame = frame[:i]
	}
	frame = frame[strings.LastIndexByte(frame, '/')+1:]
	_, name, ok := strings.Cut(frame, ".")
	if !ok || !strings.HasPrefix(name, "Test") {
		return ""
	}
	return name
}

func indent(text, prefix string) string {
	return prefix + strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", "\n"+prefix)
}

// testNamePattern matches the test name t.TempDir puts in its path.
var testNamePattern = regexp.MustCompile(`Test[A-Za-z0-9_]+`)

func attemptsError(attempts []attempt) error {
	if len(attempts) == 0 {
		return nil
	}
	var tests []string
	var b strings.Builder
	for _, a := range attempts {
		for _, name := range testNamePattern.FindAllString(a.record, -1) {
			// t.TempDir appends a random number to the sanitized name.
			name = strings.TrimRight(name, "0123456789")
			if !slices.Contains(tests, name) {
				tests = append(tests, name)
			}
		}
		b.WriteString(a.record)
		b.WriteString("\n")
		b.WriteString(a.evidence)
	}
	named := "the record names no test; see the stacks"
	if len(tests) > 0 {
		named = strings.Join(tests, ", ")
	}
	return fmt.Errorf("providerpath: a provider binary on PATH was run (%s); give each test a mock binary:\n%s",
		named, strings.TrimRight(b.String(), "\n"))
}
