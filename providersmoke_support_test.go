//go:build providersmoke

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerschema"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// providerSmokeWorkflowID is the id of the definition the gate writes and runs.
// The item's branch is derived from it, so the branch assertion reads it too.
const providerSmokeWorkflowID = "provider-smoke"

// providerSmokePhaseID / providerSmokeOutput are the single phase and its single
// declared output. Both must satisfy def's `[a-z0-9-]+` identifier grammar.
const (
	providerSmokePhaseID = "smoke"
	providerSmokeOutput  = "created-file"
)

// providerSmokeFileName is the file the phase is told to create inside its
// worktree. The assertions follow the envelope's declared output rather than
// this constant — the name only has to be concrete enough for the instruction
// to be unambiguous.
const providerSmokeFileName = "ao-provider-smoke.txt"

// providerSmokeFileBody is the exact content the phase is told to write.
const providerSmokeFileBody = "provider smoke ok"

// writeProviderSmokeWorkflow writes the trivial one-agent-phase definition the
// gate runs. Everything about it is deliberately minimal except the three
// things the gate measures: a generated envelope schema the CLI must accept, a
// declared output the envelope must carry back, and `access: write`, which is
// what makes the run provision an isolated worktree (spec §9).
func writeProviderSmokeWorkflow(t *testing.T, configRoot string, smoke providerSmokeCase) {
	t.Helper()
	dir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	definition := fmt.Sprintf(`id: %s
name: Provider smoke
outputs:
  %s:
    from: %s.%s
phases:
  - id: %s
    name: Smoke
    driver: agent
    provider: %s
    model: %s
    prompt: smoke.md
    access: write
    outputs:
      %s:
        schema:
          type: string
    gate:
      routes:
        - to: done
cleanup: manual
`,
		providerSmokeWorkflowID,
		providerSmokeOutput, providerSmokePhaseID, providerSmokeOutput,
		providerSmokePhaseID, smoke.providerName, smoke.model,
		providerSmokeOutput,
	)
	if err := os.WriteFile(filepath.Join(dir, providerSmokeWorkflowID+".yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	prompt := fmt.Sprintf(
		"Create a file named `%s` in your current working directory. Its entire contents must be this single line:\n\n"+
			"%s\n\n"+
			"That is the whole task. Do not explore the repository, do not run any checks, and do not commit anything.\n"+
			"When the file exists, set outputs.%s to the file's path relative to your working directory.\n",
		providerSmokeFileName, providerSmokeFileBody, providerSmokeOutput,
	)
	if err := os.WriteFile(filepath.Join(dir, "smoke.md"), []byte(prompt), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeProviderSmokeProfile writes the project profile the run binds against.
//
// The backoff schedule is deliberately a single fast entry rather than the
// documented 30s/2m/5m default: the gate's budget is one turn per provider, and
// the default would both multiply spend on a transient failure and outrun the
// test deadline waiting between retries. One retry is kept so a genuine network
// blip does not read as a gate failure. An empty list is not an option — the
// runner falls back to the default when the profile declares none.
func writeProviderSmokeProfile(t *testing.T, configRoot, slug string) {
	t.Helper()
	dir := filepath.Join(configRoot, "projects", slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	profileYAML := "base_branch: main\n" +
		"reliability:\n" +
		"  watchdog: " + providerSmokeWatchdog.String() + "\n" +
		"  backoff: [2s]\n" +
		"worktree_setup:\n" +
		"  timeout: 1m\n"
	if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte(profileYAML), 0o600); err != nil {
		t.Fatal(err)
	}
}

// providerSmokeCollector records the provider events that explain a red gate:
// process deaths (which carry the CLI's stderr — where a `--json-schema`
// rejection lands), wire errors (where Codex's `outputSchema` rejection lands),
// and API retries. Everything else about a real turn — text deltas, tool calls —
// is noise here and is dropped, so a failure dump stays readable.
type providerSmokeCollector struct {
	mu      sync.Mutex
	entries []providerSmokeEntry
	dropped int
}

// providerSmokeEntry is one recorded diagnostic line.
type providerSmokeEntry struct {
	source  string
	kind    string
	content string
	meta    string
}

func (e providerSmokeEntry) String() string {
	parts := []string{e.source}
	if e.kind != "" {
		parts = append(parts, "kind="+e.kind)
	}
	if e.content != "" {
		parts = append(parts, "content="+e.content)
	}
	if e.meta != "" {
		parts = append(parts, "meta="+e.meta)
	}
	return strings.Join(parts, " ")
}

// text is everything a classifier should scan for a given entry.
func (e providerSmokeEntry) text() string { return e.content + " " + e.meta }

// providerSmokeEntryCap bounds retention so a pathological run cannot grow the
// collector without limit; the count of what was dropped is reported instead.
const providerSmokeEntryCap = 256

func (c *providerSmokeCollector) add(entry providerSmokeEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= providerSmokeEntryCap {
		c.dropped++
		return
	}
	c.entries = append(c.entries, entry)
}

func (c *providerSmokeCollector) snapshot() ([]providerSmokeEntry, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]providerSmokeEntry(nil), c.entries...), c.dropped
}

// observeProviderEvent is the triage router hook — the one seam that sees every
// wire event with its Meta intact, including the ones the router drops for a
// stopped thread (the hook is deferred ahead of that gate). It runs on the
// provider read loop, so it only appends.
//
// EventSessionStatus{"error"} is the load-bearing one: its Meta is
// provider.ProcessExitInfo, whose StderrTail carries the CLI's own rejection
// text when `claude --json-schema` refuses a schema at spawn. EventError is
// where Codex's per-turn `outputSchema` rejection arrives.
func (c *providerSmokeCollector) observeProviderEvent(event provider.ProviderEvent) {
	switch event.Kind {
	case provider.EventError, provider.EventAPIRetry, provider.EventSessionStatus:
	default:
		return
	}
	if event.Kind == provider.EventSessionStatus && strings.TrimSpace(event.Content) != "error" {
		return
	}
	c.add(providerSmokeEntry{
		source:  "provider-event",
		kind:    string(event.Kind),
		content: providerSmokeTruncate(event.Content),
		meta:    providerSmokeTruncate(string(event.Meta)),
	})
}

// providerSmokeTruncate bounds one logged field. It cuts on a rune boundary:
// CLI rejection text is frequently quoted and non-ASCII, and a byte-sliced tail
// would leave a mojibake fragment in the one log a red gate is diagnosed from.
func providerSmokeTruncate(value string) string {
	const limit = 1024
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "…(truncated)"
}

// providerSmokeSchemaRejectionMarkers are the verbatim fragments both CLIs emit
// when they refuse a structured-output schema. Every one is an observed
// rejection recorded in internal/providerschema; this list is the live-gate
// mirror of that package's rule table.
//
// Each entry is a phrase the CLI only produces when REFUSING a schema — never a
// field name that could also appear in an echoed request. A marker broad enough
// to match a request payload would report an unrelated failure as a schema
// defect, which is the one thing this gate must not get wrong.
var providerSmokeSchemaRejectionMarkers = []string{
	"is not a valid json schema", // claude: --json-schema is not a valid JSON Schema: ...
	"invalid json schema",
	"strict mode:",        // claude: strict mode: unknown keyword: "..."
	"invalid_json_schema", // codex: 400 invalid_json_schema
	"'additionalproperties' is required",
	"'required' is required",
}

// providerSmokeAuthMarkers are the fragments that mean "the binary is installed
// but the account behind it is not usable". They exist so an unauthenticated
// host fails with that sentence rather than as a generic red gate. Like the
// schema markers they are phrases, not bare codes: a lone "401" also matches a
// token count.
var providerSmokeAuthMarkers = []string{
	"invalid api key",
	"invalid_api_key",
	"please run /login",
	"claude login",
	"codex login",
	"not logged in",
	"login required",
	"unauthenticated",
	"unauthorized",
	"authentication_error",
	"oauth token has expired",
}

func providerSmokeMatches(entries []providerSmokeEntry, markers []string) (providerSmokeEntry, string, bool) {
	for _, entry := range entries {
		lowered := strings.ToLower(entry.text())
		for _, marker := range markers {
			if strings.Contains(lowered, marker) {
				return entry, marker, true
			}
		}
	}
	return providerSmokeEntry{}, "", false
}

// providerSmokeSchemaVerdict explains a live schema rejection in terms of the
// package that is supposed to prevent it. If our own generated schema already
// breaks a known rule, the mock-level gate has a hole and the fix is in the
// generator; if it satisfies every known rule and the CLI still refused it, the
// CLI enforces a rule internal/providerschema does not know about yet.
func providerSmokeSchemaVerdict(schema []byte) string {
	violations := providerschema.Validate(schema)
	if len(violations) == 0 {
		return "the generated schema satisfies every rule in internal/providerschema, so the CLI enforces a rule that package does not know about yet — reproduce it, add the rule with the verbatim rejection, and the mock provider will catch it from then on"
	}
	messages := make([]string, 0, len(violations))
	for _, violation := range violations {
		messages = append(messages, violation.Error())
	}
	return "the generated schema ALREADY breaks known internal/providerschema rules (" +
		strings.Join(messages, "; ") +
		"), so schema generation regressed and the unit-level gate should have caught it"
}

// providerSmokePhaseSchema regenerates the envelope schema for the run's frozen
// phase — the exact bytes handed to the CLI — so a failure can be explained
// against internal/providerschema without guessing what was sent.
func providerSmokePhaseSchema(item store.WorkItem) ([]byte, error) {
	if len(item.Snapshot) == 0 {
		return nil, fmt.Errorf("work item %q has no frozen workflow snapshot", item.ID)
	}
	var snapshot engine.Snapshot
	if err := json.Unmarshal(item.Snapshot, &snapshot); err != nil {
		return nil, fmt.Errorf("decode work item %q snapshot: %w", item.ID, err)
	}
	for _, phase := range snapshot.Workflow.Phases {
		if phase.ID != providerSmokePhaseID {
			continue
		}
		return def.EnvelopeSchema(phase)
	}
	return nil, fmt.Errorf("frozen snapshot has no phase %q", providerSmokePhaseID)
}

// waitForProviderSmokeTerminal blocks until the run leaves `running`, returning
// the run row and whether it got there before the deadline. Unlike the
// millisecond-scale harness waiter this polls for minutes, because a real turn
// legitimately takes them — and it reports a deadline overrun as itself rather
// than letting a still-running row read as a wrong terminal state.
func waitForProviderSmokeTerminal(
	t *testing.T, app *App, itemID string, deadline time.Duration,
) (item store.WorkItem, settled bool) {
	t.Helper()
	started := time.Now()
	expiry := started.Add(deadline)
	for {
		current, err := app.store.GetWorkItem(itemID)
		if err != nil {
			t.Fatalf("provider smoke: read run %s: %v", itemID, err)
		}
		if engine.State(current.State) != engine.StateRunning {
			t.Logf("provider smoke: run %s reached %s/%s after %s",
				itemID, current.State, current.Reason, time.Since(started).Round(time.Second))
			return current, true
		}
		if time.Now().After(expiry) {
			return current, false
		}
		time.Sleep(providerSmokePollInterval)
	}
}

// dumpProviderSmokeDiagnostics writes everything needed to explain a red gate
// without re-running it: the run row, every phase attempt with its envelope, the
// phase thread's error/notification timeline, the narrative the phase wrote, and
// the collected provider diagnostics.
func dumpProviderSmokeDiagnostics(t *testing.T, app *App, itemID string, collector *providerSmokeCollector) {
	t.Helper()
	item, err := app.store.GetWorkItem(itemID)
	if err != nil {
		t.Logf("DIAGNOSTICS: read run %s failed: %v", itemID, err)
	} else {
		t.Logf("DIAGNOSTICS run: state=%s reason=%s worktree=%q branch=%q base=%q currentPhase=%q",
			item.State, item.Reason, item.WorktreePath, item.Branch, item.BaseBranch, item.CurrentPhaseID)
	}

	phases, err := app.store.ListWorkItemPhases(itemID)
	if err != nil {
		t.Logf("DIAGNOSTICS: list phases failed: %v", err)
	}
	for _, phase := range phases {
		t.Logf("DIAGNOSTICS phase: id=%s attempt=%d status=%s thread=%s envelope=%s gate=%s",
			phase.PhaseID, phase.Attempt, phase.Status, phase.ThreadID,
			providerSmokeTruncate(string(phase.OutputEnvelope)),
			providerSmokeTruncate(string(phase.GateTrace)))
		dumpProviderSmokeNarrative(t, phase.NarrativePath)
		dumpProviderSmokeThread(t, app, phase.ThreadID)
	}

	units, err := app.store.ListWorkItemUnits(itemID)
	if err != nil {
		t.Logf("DIAGNOSTICS: list units failed: %v", err)
	}
	for _, unit := range units {
		t.Logf("DIAGNOSTICS unit: id=%s status=%s thread=%s", unit.UnitID, unit.Status, unit.ThreadID)
	}

	entries, dropped := collector.snapshot()
	if len(entries) == 0 {
		t.Log("DIAGNOSTICS provider events: none recorded")
	}
	for _, entry := range entries {
		t.Logf("DIAGNOSTICS %s", entry)
	}
	if dropped > 0 {
		t.Logf("DIAGNOSTICS: %d further provider diagnostics dropped at the retention cap", dropped)
	}
}

func dumpProviderSmokeNarrative(t *testing.T, narrativePath string) {
	t.Helper()
	if strings.TrimSpace(narrativePath) == "" {
		return
	}
	data, err := os.ReadFile(narrativePath)
	if err != nil {
		t.Logf("DIAGNOSTICS narrative %s: %v", narrativePath, err)
		return
	}
	t.Logf("DIAGNOSTICS narrative %s: %s", narrativePath, providerSmokeTruncate(string(data)))
}

// dumpProviderSmokeThread logs the timeline rows that carry failure text.
// Assistant prose and tool traffic are omitted: they are the run's work, not its
// diagnosis, and a real turn produces enough of both to bury the signal.
func dumpProviderSmokeThread(t *testing.T, app *App, threadID string) {
	t.Helper()
	if strings.TrimSpace(threadID) == "" {
		return
	}
	thread, err := app.store.GetThread(threadID)
	if err != nil {
		t.Logf("DIAGNOSTICS thread %s: %v", threadID, err)
	} else {
		t.Logf("DIAGNOSTICS thread: id=%s provider=%s model=%s runtimeMode=%s workspace=%q worktree=%q branch=%q",
			thread.ID, thread.Provider, thread.Model, thread.RuntimeMode,
			thread.WorkspacePath, thread.WorktreePath, thread.Branch)
	}
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Logf("DIAGNOSTICS thread %s items: %v", threadID, err)
		return
	}
	for _, item := range items {
		switch item.Kind {
		case "error", "notification":
		default:
			continue
		}
		t.Logf("DIAGNOSTICS thread item: kind=%s role=%s status=%s summary=%s meta=%s",
			item.Kind, item.Role, item.Status,
			providerSmokeTruncate(item.Summary), providerSmokeTruncate(item.Meta))
	}
}

// providerSmokeGitOutput runs a git command in the project checkout and returns
// its trimmed combined output. Any failure is fatal: a gate that cannot inspect
// the checkout cannot claim the checkout was left alone.
func providerSmokeGitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = repo
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("provider smoke: git %v in %s failed: %v\n%s", args, repo, err, output)
	}
	return strings.TrimSpace(string(output))
}

// providerSmokeDeclaredOutput reads the phase envelope's single declared output
// as a non-empty string, naming what it found when it is anything else.
func providerSmokeDeclaredOutput(envelope json.RawMessage) (string, error) {
	var control struct {
		Status  string         `json:"status"`
		Outputs map[string]any `json:"outputs"`
	}
	if err := json.Unmarshal(envelope, &control); err != nil {
		return "", fmt.Errorf("decode envelope: %w", err)
	}
	if control.Status != "done" {
		return "", fmt.Errorf("envelope status = %q, want done", control.Status)
	}
	value, ok := control.Outputs[providerSmokeOutput]
	if !ok {
		names := make([]string, 0, len(control.Outputs))
		for name := range control.Outputs {
			names = append(names, name)
		}
		sort.Strings(names)
		return "", fmt.Errorf("envelope outputs %v do not include declared output %q", names, providerSmokeOutput)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("declared output %q = %#v, want a string", providerSmokeOutput, value)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("declared output %q is empty", providerSmokeOutput)
	}
	return text, nil
}
