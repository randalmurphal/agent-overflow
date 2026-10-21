package memory

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

func encodedPlan(t *testing.T, plan Plan) string {
	t.Helper()
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The hook prints what Build makes, stamped with the time it ran.
func TestRunHookDeliversTheBuild(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	write(t, s.project, "a.md", factText("ruling", "Never push to main."))
	var stdout, stderr strings.Builder
	if code := runHookAt([]string{encodedPlan(t, s.plan)}, strings.NewReader("{}"), &stdout, &stderr, day); code != 0 || stderr.Len() > 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	built, _ := Build(s.plan, day)
	if got := hookContext(t, stdout.String()); got != Stamp(built, day) {
		t.Fatalf("context = %q", got)
	}
}

// A folder the hook cannot read is a notice in the delivery and a line on
// stderr; the hook still delivers.
func TestRunHookReportsAnUnreadableFolder(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	if err := os.RemoveAll(s.plan.User); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Dir(s.plan.User), "user", "a file where the folder goes")
	var stdout, stderr strings.Builder
	if code := RunHook([]string{encodedPlan(t, s.plan)}, strings.NewReader("{}"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := hookContext(t, stdout.String()); !strings.Contains(got, "could not be read: open "+s.plan.User+": not a folder.]") ||
		!strings.Contains(stderr.String(), "read the memory folder "+s.plan.User) {
		t.Fatalf("context %q, stderr %q", got, stderr.String())
	}
}

func TestRunHookRefusesBadArguments(t *testing.T) {
	t.Parallel()
	abs := t.TempDir()
	for name, args := range map[string][]string{
		"none":          nil,
		"two":           {`{"user":"` + abs + `"}`, "x"},
		"not JSON":      {"instructions.md"},
		"unknown field": {`{"user":"` + abs + `","extra":1}`},
		"relative":      {`{"user":"memory/user"}`},
		"project name":  {`{"user":"` + abs + `","projects":[{"name":"","path":"` + abs + `","dir":"` + abs + `"}]}`},
	} {
		var stdout, stderr strings.Builder
		if code := RunHook(args, strings.NewReader(""), &stdout, &stderr); code != 1 || stdout.Len() > 0 || stderr.Len() == 0 {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, stdout.String(), stderr.String())
		}
	}
}

// The hook drains its input, so the provider's write never meets a closed
// pipe, which Codex counts as a failed hook.
func TestRunHookDrainsItsInput(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	input := &countingReader{r: strings.NewReader(strings.Repeat("x", 300<<10))}
	var stdout, stderr strings.Builder
	if code := RunHook([]string{encodedPlan(t, s.plan)}, input, &stdout, &stderr); code != 0 || input.n != 300<<10 {
		t.Fatalf("exit %d, drained %d", code, input.n)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestRunHookReportsAFailedWrite(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	var stderr strings.Builder
	if code := RunHook([]string{encodedPlan(t, s.plan)}, strings.NewReader(""), failingWriter{}, &stderr); code != 1 || !strings.Contains(stderr.String(), "output gone") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("output gone") }

// Both providers run the hook command through a POSIX shell, so the binary
// and every folder survive spaces, quotes and shell syntax in their paths.
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
	config := filepath.Join(t.TempDir(), "it's a `config` & more; $(x)")
	home := filepath.Join(config, "agents", "a 1")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	plan := Plan{User: UserDir(config), Home: home, Projects: []Project{{Name: `it's "app"`, Path: "/Users/u/it's app", Dir: filepath.Join(config, "memory", "projects", "it's app")}}}
	if _, err := Prepare(plan); err != nil {
		t.Fatal(err)
	}
	write(t, plan.Projects[0].Dir, "a.md", factText("ruling", "Quoted `$(x)` & 'more'."))
	command, err := HookCommand(link, plan)
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
	got := hookContext(t, string(stdout))
	if built, _ := Build(plan, day); Digest(got) != Digest(built) {
		t.Fatalf("context = %q (stderr %q)", got, stderr.String())
	}
}

// A command the hook cannot run from the workspace, or that the provider's
// JSON settings cannot carry, is refused rather than built.
func TestHookCommandRefusesWhatItCannotCarry(t *testing.T) {
	t.Parallel()
	abs := filepath.Join(t.TempDir(), "x")
	for name, tc := range map[string]struct {
		exe  string
		plan Plan
		want string
	}{
		"relative exe":   {"agent-overflow", Plan{User: abs}, `"agent-overflow" is not an absolute path`},
		"non-UTF-8 exe":  {abs + "\xff", Plan{User: abs}, "is not valid UTF-8"},
		"relative user":  {abs, Plan{User: "memory/user"}, `the user folder "memory/user" is not an absolute path`},
		"relative home":  {abs, Plan{User: abs, Home: "agents/a"}, `the home "agents/a" is not an absolute path`},
		"non-UTF-8 home": {abs, Plan{User: abs, Home: abs + "\xfe"}, "is not valid UTF-8"},
		"project dir":    {abs, Plan{User: abs, Projects: []Project{{Name: "app", Path: abs, Dir: "x"}}}, `the memory folder of app "x" is not an absolute path`},
	} {
		if command, err := HookCommand(tc.exe, tc.plan); err == nil || !strings.Contains(err.Error(), tc.want) || command != "" {
			t.Errorf("%s: %q, %v; want an error with %q", name, command, err, tc.want)
		}
	}
	if _, err := HookCommand(abs, Plan{User: abs}); err != nil {
		t.Fatalf("absolute paths: %v", err)
	}
}
