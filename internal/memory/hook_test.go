package memory

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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

// hookContext decodes the SessionStart output both providers read.
func hookContext(t *testing.T, stdout string) string {
	t.Helper()
	return eventContext(t, stdout, "SessionStart")
}

// eventContext decodes the output of a run for event.
func eventContext(t *testing.T, stdout, event string) string {
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
	if output.HookSpecificOutput.HookEventName != event {
		t.Fatalf("hook event = %q", output.HookSpecificOutput.HookEventName)
	}
	return output.HookSpecificOutput.AdditionalContext
}

// planFile writes plan to a plan file, as the app does before a session
// starts, and returns its path.
func planFile(t *testing.T, plan Plan) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := WritePlan(path, plan); err != nil {
		t.Fatal(err)
	}
	return path
}

// rawPlanFile is a plan file holding text, which WritePlan may refuse.
func rawPlanFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// userOutput is Build's user part of plan.
func userOutput(t *testing.T, plan Plan) Output {
	t.Helper()
	built, _ := Build(plan, day)
	return built[slices.Index(plan.Parts(), PartUser)]
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
	if code := RunHook([]string{string(AtSessionStart), string(PartUser), planFile(t, s.plan)}, strings.NewReader("{}"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := hookContext(t, stdout.String()); !strings.Contains(got, "could not be read: open "+s.plan.User+": not a folder.]") ||
		!strings.Contains(stderr.String(), "read the memory folder "+s.plan.User) {
		t.Fatalf("context %q, stderr %q", got, stderr.String())
	}
}

// A refused argument or plan, or a plan file that is gone or cannot be read,
// exits 1 with the reason on stderr and nothing on stdout: the provider
// reports the delivery as one that did not arrive.
func TestRunHookRefusesBadArguments(t *testing.T) {
	t.Parallel()
	abs := t.TempDir()
	plan := func(text string) string { return rawPlanFile(t, text) }
	user := plan(`{"user":"` + abs + `"}`)
	for name, args := range map[string][]string{
		"none":            nil,
		"no part":         {"session-start", user},
		"four":            {"prompt", "user", user, "x"},
		"unknown event":   {"stop", "user", plan(`{"user":"` + abs + `","records":"` + abs + `"}`)},
		"unknown part":    {"session-start", "rules", user},
		"not in the plan": {"session-start", "home", user},
		"no plan file":    {"session-start", "user", filepath.Join(abs, "gone.json")},
		"a folder":        {"session-start", "user", abs},
		"not JSON":        {"session-start", "user", plan("instructions.md")},
		"too long":        {"session-start", "user", plan(`{"user":"` + abs + `"}` + strings.Repeat(" ", maxPlanBytes))},
		"unknown field":   {"session-start", "user", plan(`{"user":"` + abs + `","extra":1}`)},
		"relative":        {"session-start", "user", plan(`{"user":"memory/user"}`)},
		"project name":    {"session-start", "user", plan(`{"user":"` + abs + `","projects":[{"name":"","path":"` + abs + `","dir":"` + abs + `"}]}`)},
		"prompt, no file": {"prompt", "user", user},
		"records":         {"prompt", "user", plan(`{"user":"` + abs + `","records":"records"}`)},
	} {
		var stdout, stderr strings.Builder
		if code := RunHook(args, strings.NewReader(`{"session_id":"s1"}`), &stdout, &stderr); code != 1 || stdout.Len() > 0 || !strings.HasPrefix(stderr.String(), HookSubcommand+": ") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, stdout.String(), stderr.String())
		}
		if name == "no plan file" && !strings.Contains(stderr.String(), "read the memory plan: open "+filepath.Join(abs, "gone.json")+": no such file") {
			t.Errorf("a gone plan file: stderr %q", stderr.String())
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
	if code := RunHook([]string{string(AtSessionStart), string(PartUser), planFile(t, s.plan)}, input, &stdout, &stderr); code != 0 || input.n != 300<<10 {
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
	if code := RunHook([]string{string(AtSessionStart), string(PartUser), planFile(t, s.plan)}, strings.NewReader(""), failingWriter{}, &stderr); code != 1 || !strings.Contains(stderr.String(), "output gone") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("output gone") }

// A run whose report stderr does not take fails: before the delivery it
// prints and records nothing, after it the run still exits 1. A run with
// nothing to report never writes stderr and delivers.
func TestRunHookFailsWhenStderrRefusesAReport(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	plan := planFile(t, s.plan)

	var stdout strings.Builder
	if code := runHookAt([]string{string(AtSessionStart), string(PartUser), plan}, strings.NewReader("{}"), &stdout, failingWriter{}, day); code != 1 || stdout.Len() > 0 {
		t.Fatalf("report before the delivery: exit %d, stdout %q", code, stdout.String())
	}

	blocked := filepath.Join(t.TempDir(), "file")
	write(t, filepath.Dir(blocked), "file", "a file where the records folder goes")
	unwritable := s.plan
	unwritable.Records = filepath.Join(blocked, "thread")
	stdout.Reset()
	if code := runHookAt([]string{string(AtSessionStart), string(PartUser), planFile(t, unwritable)}, strings.NewReader(`{"session_id":"s1"}`), &stdout, failingWriter{}, day); code != 1 {
		t.Fatalf("report after the delivery: exit %d", code)
	}

	stdout.Reset()
	if code := runHookAt([]string{string(AtSessionStart), string(PartUser), plan}, strings.NewReader(`{"session_id":"s1"}`), &stdout, failingWriter{}, day); code != 0 {
		t.Fatalf("nothing to report: exit %d", code)
	}
	if got := hookContext(t, stdout.String()); got != Stamp(userOutput(t, s.plan).Text, day) {
		t.Fatalf("context = %q", got)
	}
}

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
	plan.Records = filepath.Join(config, "memory-records", "thread 1")
	path, err := PlanFile(config, "thread 1")
	if err != nil {
		t.Fatal(err)
	}
	if err := WritePlan(path, plan); err != nil {
		t.Fatal(err)
	}
	run := func(event HookEvent) (string, string) {
		command, err := HookCommand(link, event, PartProject, path)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", "-c", command)
		cmd.Stdin = strings.NewReader(`{"session_id":"s1","hook_event_name":"SessionStart","source":"compact"}`)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		stdout, err := cmd.Output()
		if err != nil || stderr.Len() > 0 {
			t.Fatalf("%s: %v: %s", event, err, stderr.String())
		}
		return string(stdout), stderr.String()
	}
	stdout, _ := run(AtSessionStart)
	got := hookContext(t, stdout)
	built, _ := Build(plan, day)
	if project := built[slices.Index(plan.Parts(), PartProject)]; !strings.Contains(got, strings.TrimPrefix(project.Text, plan.HeadingOf(PartProject))) {
		t.Fatalf("context = %q", got)
	}
	write(t, plan.Projects[0].Dir, "b.md", factText("ruling", "Added `$(y)`."))
	stdout, _ = run(AtPrompt)
	if got := eventContext(t, stdout, "UserPromptSubmit"); !strings.HasPrefix(got, ChangesHeading) || !strings.Contains(got, "\n- ruling: Added `$(y)`. (b.md)\n") || strings.Contains(got, "Quoted") {
		t.Fatalf("changes = %q", got)
	}
}

// A command the hook cannot run from the workspace, or that the provider's
// JSON settings cannot carry, is refused rather than built, and so is a
// plan the hook could not read.
func TestHookCommandRefusesWhatItCannotCarry(t *testing.T) {
	t.Parallel()
	abs := filepath.Join(t.TempDir(), "x")
	for name, tc := range map[string]struct{ exe, plan, want string }{
		"relative exe":   {"agent-overflow", abs, `"agent-overflow" is not an absolute path`},
		"non-UTF-8 exe":  {abs + "\xff", abs, "is not valid UTF-8"},
		"relative plan":  {abs, "plan.json", `"plan.json" is not an absolute path`},
		"non-UTF-8 plan": {abs, abs + "\xfe", "is not valid UTF-8"},
	} {
		if command, err := HookCommand(tc.exe, AtSessionStart, PartUser, tc.plan); err == nil || !strings.Contains(err.Error(), tc.want) || command != "" {
			t.Errorf("%s: %q, %v; want an error with %q", name, command, err, tc.want)
		}
	}
	if _, err := HookCommand(abs, AtPrompt, PartHome, abs); err != nil {
		t.Fatalf("absolute paths: %v", err)
	}
	for name, tc := range map[string]struct {
		plan Plan
		want string
	}{
		"relative user":  {Plan{User: "memory/user"}, `the user folder "memory/user" is not an absolute path`},
		"relative home":  {Plan{User: abs, Home: "agents/a"}, `the home "agents/a" is not an absolute path`},
		"non-UTF-8 home": {Plan{User: abs, Home: abs + "\xfe"}, "is not valid UTF-8"},
		"project dir":    {Plan{User: abs, Projects: []Project{{Name: "app", Path: abs, Dir: "x"}}}, `the memory folder of app "x" is not an absolute path`},
		"records":        {Plan{User: abs, Records: "records"}, `the records folder "records" is not an absolute path`},
	} {
		path := filepath.Join(t.TempDir(), "plan.json")
		if err := WritePlan(path, tc.plan); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v; want an error with %q", name, err, tc.want)
		}
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: the refused plan was written: %v", name, err)
		}
	}
}
