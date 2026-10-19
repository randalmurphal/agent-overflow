package agenthome

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain runs the hook when a test runs this test binary as the hook
// command, as the providers run the app binary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == HookSubcommand {
		os.Exit(RunHook(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// hookContext decodes the hook output both providers read.
func hookContext(t *testing.T, stdout string) string {
	t.Helper()
	var output struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("hook output %q: %v", stdout, err)
	}
	if output.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatalf("hook event = %q", output.HookSpecificOutput.HookEventName)
	}
	return output.HookSpecificOutput.AdditionalContext
}

func runHook(t *testing.T, args []string, stdin io.Reader) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	code = RunHook(args, stdin, &out, &errOut)
	return code, out.String(), errOut.String()
}

// unstamped is a delivered context without its stamp, which it must carry.
func unstamped(t *testing.T, context string) string {
	t.Helper()
	if _, rest, _ := strings.Cut(context, "\n"); !strings.HasPrefix(rest, StampPrefix) {
		t.Fatalf("no stamp under the heading: %q", context)
	}
	return withoutStamp(context)
}

func TestRunHookDeliversTheCurrentFile(t *testing.T) {
	t.Parallel()
	home := writeHome(t, map[string]string{InstructionsFile: "Release lead\n", MemoryIndexFile: "- [A](memory/a.md): first\n", "memory/a.md": "A", "memory/b.md": "B"})
	for _, file := range ContextFiles {
		code, stdout, stderr := runHook(t, []string{file.Name, home}, strings.NewReader(`{"source":"startup"}`))
		want, _ := ReadContext(home, file)
		if code != 0 || stderr != "" || unstamped(t, hookContext(t, stdout)) != want || !strings.HasSuffix(stdout, "}\n") {
			t.Fatalf("%s: %d, %q, %q; want the context %q", file.Name, code, stdout, stderr, want)
		}
	}
	// Each run reads the file as it is then.
	if err := os.WriteFile(filepath.Join(home, MemoryIndexFile), []byte("- [A](memory/a.md): first\n- [B](memory/b.md): second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stdout, _ := runHook(t, []string{MemoryIndexFile, home}, strings.NewReader(""))
	if got := unstamped(t, hookContext(t, stdout)); got != memoryIndexHeading+"\n\n- [A](memory/a.md): first\n- [B](memory/b.md): second\n\n[End of MEMORY.md]" {
		t.Fatalf("after an edit: %q", got)
	}
}

// Every delivery carries a stamp of its time under the heading, whatever the
// hook input, so each is text of its own, which Claude Code does not leave
// out as one its transcript holds; the digest of the delivery is the digest
// of the file as ReadContext reads it.
func TestRunHookStampsEachDelivery(t *testing.T) {
	t.Parallel()
	home := writeHome(t, map[string]string{InstructionsFile: "Release lead\n", MemoryIndexFile: "- index\n"})
	at := time.Date(2026, 10, 19, 10, 4, 5, 123456789, time.FixedZone("x", 3600))
	for _, file := range ContextFiles {
		read, _ := ReadContext(home, file)
		want := file.Heading + "\n[Delivered at 2026-10-19T09:04:05.123Z; it replaces any earlier copy of " + file.Name + " in this conversation.]" + strings.TrimPrefix(read, file.Heading)
		for _, input := range []string{`{"hook_event_name":"SessionStart","source":"resume"}`, `{"source":"startup"}`, `{"source":"compact"}`, `{}`, ``, `{"source":"resume"`} {
			var stdout, stderr strings.Builder
			code := runHookAt([]string{file.Name, home}, strings.NewReader(input), &stdout, &stderr, at)
			got := hookContext(t, stdout.String())
			if code != 0 || stderr.String() != "" || got != want {
				t.Fatalf("%s for %q: %d, %q\ngot  %q\nwant %q", file.Name, input, code, stderr.String(), got, want)
			}
			if ContextDigest(got) != ContextDigest(read) {
				t.Fatalf("%s for %q: the delivery's digest is not the file's", file.Name, input)
			}
		}
		var stdout, stderr strings.Builder
		runHookAt([]string{file.Name, home}, strings.NewReader(""), &stdout, &stderr, at.Add(time.Millisecond))
		if again := hookContext(t, stdout.String()); again == want || ContextDigest(again) != ContextDigest(want) {
			t.Fatalf("%s a moment later: %q", file.Name, again)
		}
	}
}

// providerContextLimit is what both providers deliver of one hook's context
// in full: Claude Code writes a longer additionalContext to disk and keeps a
// preview (10,000 characters), Codex spills one past 2,500 tokens counted at
// 4 bytes each (DEFAULT_HOOK_OUTPUT_TOKEN_LIMIT, rust-v0.162.1). Both measure
// the decoded text, so JSON escaping does not count; a byte count bounds
// both.
const providerContextLimit = 10_000

// Every context file's largest delivery, its cut notice and its stamp
// included, reaches the model whole on both providers.
func TestRunHookOutputFitsBothProviders(t *testing.T) {
	t.Parallel()
	for _, file := range ContextFiles {
		for name, content := range map[string]string{
			"one line of two-byte characters": strings.Repeat("é", file.Cap),
			// Cut inside a line, with the notice naming the largest numbers.
			"one line of three-byte characters": strings.Repeat("\u20ac", file.Cap),
			"one line of four-byte characters":  strings.Repeat("😀", file.Cap),
			"short lines":                       strings.Repeat("x\n", file.Cap),
			"empty lines":                       strings.Repeat("\n", 2*MaxRawRead),
			"hidden characters, then text":      strings.Repeat("\u200b", MaxRawRead/3) + strings.Repeat("y", file.Cap),
			"text past the raw bound":           strings.Repeat("\x00", MaxRawRead-file.Cap/2) + strings.Repeat("z\n", file.Cap),
		} {
			home := writeHome(t, map[string]string{file.Name: content})
			code, stdout, stderr := runHook(t, []string{file.Name, home}, strings.NewReader(`{"source":"startup"}`))
			if code != 0 || stderr != "" {
				t.Fatalf("%s, %s: %d, %q", file.Name, name, code, stderr)
			}
			if context := hookContext(t, stdout); len(context) > providerContextLimit || !strings.Contains(context, " stops here, ") || !strings.Contains(context, StampPrefix) {
				t.Errorf("%s, %s: %d bytes of context, want a cut at most %d: %q", file.Name, name, len(context), providerContextLimit, context[len(context)-min(len(context), 300):])
			}
		}
	}
}

// The memory index at its cap with the largest notices the index check
// writes, every name at its bound, and its stamp still reaches the model
// whole.
func TestRunHookOutputWithTheIndexCheckFitsBothProviders(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("€", maxListedNameBytes)
	files := map[string]string{}
	var index strings.Builder
	for i := range maxListedNames + 1 {
		index.WriteString("- [gone](memory/" + long + strconv.Itoa(i) + ".md)\n")
		files["memory/unlinked-"+long+strconv.Itoa(i)+".md"] = "x"
	}
	for index.Len() < MemoryIndexCap+100 {
		index.WriteString("- filler line\n")
	}
	files[MemoryIndexFile] = index.String()
	home := writeHome(t, files)
	code, stdout, stderr := runHook(t, []string{MemoryIndexFile, home}, strings.NewReader(`{"source":"startup"}`))
	if code != 0 || stderr != "" {
		t.Fatalf("%d, %q", code, stderr)
	}
	context := hookContext(t, stdout)
	if len(context) > providerContextLimit || strings.Count(context, IndexCheckPrefix) != 2 || !strings.Contains(context, " stops here, ") || !strings.Contains(context, StampPrefix) {
		t.Fatalf("%d bytes of context, want both notices within %d: %q", len(context), providerContextLimit, context[len(context)-min(len(context), 3000):])
	}
}

// A file that cannot be read is still delivered, as its notice, and the
// problem goes to stderr, where both providers keep it in the hook's record.
func TestRunHookDeliversAnUnreadableFileAsANotice(t *testing.T) {
	t.Parallel()
	home := writeHome(t, nil)
	if err := os.Mkdir(filepath.Join(home, InstructionsFile), 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runHook(t, []string{InstructionsFile, home}, strings.NewReader(""))
	if code != 0 || unstamped(t, hookContext(t, stdout)) != instructionsHeading+"\n\n[instructions.md could not be read: not a regular file. Read it yourself.]\n\n[End of instructions.md]" ||
		!strings.HasPrefix(stderr, HookSubcommand+": read "+filepath.Join(home, InstructionsFile)+": not a regular file") {
		t.Fatalf("%d, %q, %q", code, stdout, stderr)
	}
}

func TestRunHookRefusesBadArguments(t *testing.T) {
	t.Parallel()
	home := writeHome(t, nil)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"none":        {nil, "want a context file and a home, got 0 arguments"},
		"no home":     {[]string{MemoryIndexFile}, "want a context file and a home, got 1 arguments"},
		"extra":       {[]string{MemoryIndexFile, home, "x"}, "want a context file and a home, got 3 arguments"},
		"other file":  {[]string{"README.md", home}, `"README.md" is not a context file`},
		"blank home":  {[]string{MemoryIndexFile, " \t"}, "the home is empty"},
		"relative":    {[]string{MemoryIndexFile, "agents/a"}, `the home "agents/a" is not an absolute path`},
		"path escape": {[]string{"../" + MemoryIndexFile, home}, `"../MEMORY.md" is not a context file`},
	} {
		code, stdout, stderr := runHook(t, tc.args, strings.NewReader(""))
		if code != 1 || stdout != "" || stderr != HookSubcommand+": "+tc.want+"\n" {
			t.Errorf("%s: %d, %q, %q", name, code, stdout, stderr)
		}
	}
}

// The hook drains its input, up to a bound, so the provider's write of it
// never fails on a closed pipe; Codex counts that as a failed hook.
func TestRunHookDrainsItsInput(t *testing.T) {
	t.Parallel()
	home := writeHome(t, nil)
	small := strings.NewReader(strings.Repeat("x", 100<<10))
	if code, _, _ := runHook(t, []string{MemoryIndexFile, home}, small); code != 0 || small.Len() != 0 {
		t.Fatalf("code %d, %d bytes left unread", code, small.Len())
	}
	large := strings.NewReader(strings.Repeat("x", maxHookInput+10))
	if code, _, _ := runHook(t, []string{MemoryIndexFile, home}, large); code != 0 || large.Len() != 10 {
		t.Fatalf("code %d, %d bytes left unread, want 10", code, large.Len())
	}
	code, stdout, stderr := runHook(t, []string{MemoryIndexFile, home}, io.MultiReader(strings.NewReader("{"), failingReader{}))
	if code != 0 || !strings.HasPrefix(hookContext(t, stdout), memoryIndexHeading) || stderr != HookSubcommand+": read the hook input: input gone\n" {
		t.Fatalf("a failed input read: %d, %q, %q", code, stdout, stderr)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("input gone") }

func TestRunHookReportsAFailedWrite(t *testing.T) {
	t.Parallel()
	var stderr strings.Builder
	if code := RunHook([]string{MemoryIndexFile, writeHome(t, nil)}, strings.NewReader(""), failingWriter{}, &stderr); code != 1 ||
		stderr.String() != HookSubcommand+": write the output: output gone\n" {
		t.Fatalf("%d, %q", code, stderr.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("output gone") }

// Both providers run the hook command through a POSIX shell, so the binary
// and the home survive spaces, quotes and shell syntax in their paths.
func TestHookCommandRunsThroughAShell(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the providers run hooks through a POSIX shell")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), `bin's "dir" $HOME`)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "agent overflow")
	if err := os.Symlink(exe, link); err != nil {
		t.Skip("no symlinks here:", err)
	}
	home := filepath.Join(t.TempDir(), "it's a `home` & more; $(x)")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, MemoryIndexFile), []byte("- [A](memory/a.md): hook\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, _ := ContextFileNamed(MemoryIndexFile)
	if err := os.Mkdir(filepath.Join(home, "memory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "memory", "a.md"), []byte("A"), 0o600); err != nil {
		t.Fatal(err)
	}
	command, err := HookCommand(link, home, file)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"SessionStart","source":"compact"}`)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	if got := unstamped(t, hookContext(t, string(stdout))); got != memoryIndexHeading+"\n\n- [A](memory/a.md): hook\n\n[End of MEMORY.md]" {
		t.Fatalf("context = %q (stderr %q)", got, stderr.String())
	}
}

// A command the hook cannot run from the workspace, or that the provider's
// JSON settings cannot carry, is refused rather than built.
func TestHookCommandRefusesPathsItCannotCarry(t *testing.T) {
	t.Parallel()
	abs := filepath.Join(t.TempDir(), "x")
	file, _ := ContextFileNamed(InstructionsFile)
	for name, tc := range map[string]struct{ exe, home, want string }{
		"relative exe":   {"agent-overflow", abs, `"agent-overflow" is not an absolute path`},
		"relative home":  {abs, "agents/a", `"agents/a" is not an absolute path`},
		"non-UTF-8 exe":  {abs + "\xff", abs, "is not valid UTF-8"},
		"non-UTF-8 home": {abs, abs + "\xfe", "is not valid UTF-8"},
	} {
		if command, err := HookCommand(tc.exe, tc.home, file); err == nil || !strings.Contains(err.Error(), tc.want) || command != "" {
			t.Errorf("%s: %q, %v; want an error with %q", name, command, err, tc.want)
		}
	}
	if _, err := HookCommand(abs, abs, file); err != nil {
		t.Fatalf("absolute paths: %v", err)
	}
}
