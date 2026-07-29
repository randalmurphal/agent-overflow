package aocli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `agent-overflow notes …` and `agent-overflow schedule`.

func TestNotesSetReadsAFileOrStdin(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentSetNotes", map[string]any{"automationId": "auto-1"})
	path := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(path, []byte("the suite is flaky on tuesdays"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI([]string{"notes", "set", "auto-1", "--file", path}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, "automation=auto-1") {
		t.Fatalf("output = %q", stdout)
	}
	calls := backend.recorded("WorkflowAgentSetNotes")
	if len(calls) != 1 || string(calls[1-1].Params[1]) != `"the suite is flaky on tuesdays"` {
		t.Fatalf("sent notes = %#v", calls)
	}

	// A missing file must fail rather than silently clear the notes.
	code, _, stderr = runCLI([]string{"notes", "set", "auto-1", "--file", path + ".missing"}, backend.env())
	if code != exitError {
		t.Fatalf("missing file exit = %d", code)
	}
	if !strings.Contains(stderr, "read notes") {
		t.Fatalf("stderr = %q", stderr)
	}
	if len(backend.recorded("WorkflowAgentSetNotes")) != 1 {
		t.Fatal("a missing file still wrote notes")
	}
}

func TestNotesGetPrintsTheNotesUnlabelled(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentGetNotes", "the suite is flaky on tuesdays")
	code, stdout, stderr := runCLI([]string{"notes", "get", "auto-1"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	// Human output is the prose itself so it can be piped straight into a file.
	if stdout != "the suite is flaky on tuesdays\n" {
		t.Fatalf("output = %q", stdout)
	}
	calls := backend.recorded("WorkflowAgentGetNotes")
	if len(calls) != 1 || string(calls[0].Params[0]) != `"auto-1"` {
		t.Fatalf("sent = %#v", calls)
	}

	code, stdout, stderr = runCLI([]string{"notes", "get", "auto-1", "--json"}, backend.env())
	if code != exitOK {
		t.Fatalf("--json exit = %d (%s)", code, stderr)
	}
	var decoded string
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil || decoded != "the suite is flaky on tuesdays" {
		t.Fatalf("--json output = %q (%v)", stdout, err)
	}
}

func TestScheduleSendsItsCronNameAndSeeds(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentSchedule", map[string]any{
		"automationId": "auto-1", "name": "Nightly audit", "cron": "0 3 * * *",
	})
	code, stdout, stderr := runCLI([]string{
		"schedule", "dep-audit", "--cron", "0 3 * * *", "--name", "Nightly audit",
		"--scope", "shared", "--seed", "depth=2",
	}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	for _, want := range []string{"automation=auto-1", `name="Nightly audit"`, `cron="0 3 * * *"`} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("output missing %q: %q", want, stdout)
		}
	}
	calls := backend.recorded("WorkflowAgentSchedule")
	if len(calls) != 1 || len(calls[0].Params) != 1 {
		t.Fatalf("calls = %#v", calls)
	}
	var sent struct {
		WorkflowID string         `json:"workflowId"`
		Scope      string         `json:"scope"`
		Name       string         `json:"name"`
		Cron       string         `json:"cron"`
		Seeds      map[string]any `json:"seeds"`
	}
	if err := json.Unmarshal(calls[0].Params[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent.WorkflowID != "dep-audit" || sent.Scope != "shared" || sent.Name != "Nightly audit" {
		t.Fatalf("sent = %#v", sent)
	}
	if sent.Cron != "0 3 * * *" || sent.Seeds["depth"] != float64(2) {
		t.Fatalf("sent = %#v", sent)
	}
}

func TestScheduleWithoutCronNeverReachesTheBackend(t *testing.T) {
	backend := newFakeBackend(t)
	code, _, stderr := runCLI([]string{"schedule", "dep-audit"}, backend.env())
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "--cron is required") {
		t.Fatalf("stderr = %q", stderr)
	}
	if calls := backend.recorded("WorkflowAgentSchedule"); len(calls) != 0 {
		t.Fatalf("a schedule with no cron still called the backend: %#v", calls)
	}
}
