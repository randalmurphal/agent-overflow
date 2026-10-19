// Package providerpath keeps a test binary from running an installed claude
// or codex. Code under test that resolves a bare provider name, such as the
// default binary settings, finds a poisoned executable first on PATH instead
// of the developer's CLI: it records the attempt and exits 127, and the run
// fails naming the tests that reached it.
//
// The package depends only on the standard library so any package with
// provider spawn code can use it from TestMain, including the packages the
// shared fixtures import.
package providerpath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/stringsx"
)

// Names are the provider executables poisoned on PATH.
var Names = []string{"claude", "codex"}

// recordName is the file each poisoned executable appends its attempt to.
const recordName = "provider-path-attempts"

// Run is a TestMain body for a package that needs nothing else:
// os.Exit(providerpath.Run(m)).
func Run(m *testing.M) int { return Guard(m.Run) }

// Guard runs a TestMain's test run, such as storetest.Run(m), with the
// poison installed, and turns any attempt it recorded into a failed run.
func Guard(run func() int) int {
	check, err := Install()
	if err != nil {
		fmt.Fprintf(os.Stderr, "providerpath: %v\n", err)
		return 1
	}
	code := run()
	if err := check(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// shellName is the stand-in Install points SHELL at.
const shellName = "login-shell"

// Install prepends a directory of poisoned provider executables to PATH for
// the rest of the process, and points SHELL at a stand-in that runs /bin/sh
// without the login and interactive flags. A login shell's startup files can
// put an installed CLI ahead of the poison, and the boot's shell sync puts
// the login shell's PATH first; the stand-in reports the process's own PATH,
// so the sync cannot reorder it. The returned check, called after m.Run, reports
// every attempt to run one and removes the directory. On Windows, where the
// poison cannot be a shell script, Install changes nothing.
func Install() (check func() error, err error) {
	if runtime.GOOS == "windows" {
		return func() error { return nil }, nil
	}
	dir, err := os.MkdirTemp("", "agent-overflow-provider-path-")
	if err != nil {
		return nil, fmt.Errorf("create poisoned provider PATH: %w", err)
	}
	record := filepath.Join(dir, recordName)
	for _, name := range Names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(poisonScript(record)), 0o755); err != nil {
			return nil, errors.Join(fmt.Errorf("write poisoned %s: %w", name, err), os.RemoveAll(dir))
		}
	}
	shell := filepath.Join(dir, shellName)
	if err := os.WriteFile(shell, []byte(standInShell), 0o755); err != nil {
		return nil, errors.Join(fmt.Errorf("write stand-in shell: %w", err), os.RemoveAll(dir))
	}
	if err := os.Setenv("SHELL", shell); err != nil {
		return nil, errors.Join(fmt.Errorf("set SHELL: %w", err), os.RemoveAll(dir))
	}
	path := dir
	if existing := os.Getenv("PATH"); existing != "" {
		path += string(os.PathListSeparator) + existing
	}
	if err := os.Setenv("PATH", path); err != nil {
		return nil, errors.Join(fmt.Errorf("set PATH: %w", err), os.RemoveAll(dir))
	}
	return func() error {
		attempts, readErr := os.ReadFile(record)
		removeErr := os.RemoveAll(dir)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return errors.Join(fmt.Errorf("providerpath: read attempts: %w", readErr), removeErr)
		}
		return errors.Join(attemptsError(attempts), removeErr)
	}, nil
}

// poisonScript records the attempt with what can name the test: the
// argument list, the working directory and the provider home variables,
// which a test points at its own t.TempDir. Builtins only, so a test that
// narrows PATH cannot break the record.
func poisonScript(record string) string {
	quoted := stringsx.ShellQuote(record)
	return "#!/bin/sh\n" +
		"line=\"ran $0\"\n" +
		"for arg in \"$@\"; do line=\"$line $arg\"; done\n" +
		"# One printf, one append: concurrent attempts do not interleave.\n" +
		"printf '%s\\n  pwd=%s HOME=%s CODEX_HOME=%s CLAUDE_CONFIG_DIR=%s\\n' \"$line\" \"$PWD\" \"$HOME\" \"${CODEX_HOME:-}\" \"${CLAUDE_CONFIG_DIR:-}\" >> " + quoted + "\n" +
		"echo \"agent-overflow tests: refused to run the installed ${0##*/} found on PATH; give the test a mock binary\" >&2\n" +
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

// testNamePattern matches the test name t.TempDir puts in its path.
var testNamePattern = regexp.MustCompile(`Test[A-Za-z0-9_]+`)

func attemptsError(attempts []byte) error {
	text := strings.TrimSpace(string(attempts))
	if text == "" {
		return nil
	}
	var tests []string
	for _, name := range testNamePattern.FindAllString(text, -1) {
		// t.TempDir appends a random number to the sanitized name.
		name = strings.TrimRight(name, "0123456789")
		if !slices.Contains(tests, name) {
			tests = append(tests, name)
		}
	}
	named := "tests not identifiable from the record"
	if len(tests) > 0 {
		named = strings.Join(tests, ", ")
	}
	return fmt.Errorf("providerpath: a provider binary on PATH was run (%s); give each test a mock binary:\n%s", named, text)
}
