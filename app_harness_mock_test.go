package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/store"
)

func TestHarnessScenarioRulesResolveAndFallBack(t *testing.T) {
	h, _ := newHarnessTestApp(t)

	// Zero rules: both providers fall back to their shipped defaults.
	claude, err := h.resolveScenario(control.Registration{Protocol: "claude", Cwd: "/ws/a"})
	if err != nil || claude.ScenarioName != "streaming-text" {
		t.Fatalf("claude fallback = %+v, %v", claude, err)
	}
	codex, err := h.resolveScenario(control.Registration{Protocol: "codex", Cwd: "/ws/a"})
	if err != nil || codex.ScenarioName != "codex-basic" {
		t.Fatalf("codex fallback = %+v, %v", codex, err)
	}
	if claude.FixtureRoot != h.paths.DataRoot {
		t.Fatalf("fallback fixture root = %q, want data root", claude.FixtureRoot)
	}

	// A catch-all claude rule applies to any claude workspace, but never
	// to codex mocks.
	if _, err := h.HarnessSetScenario(HarnessScenarioSpec{Name: "streaming-text"}); err != nil {
		t.Fatalf("HarnessSetScenario: %v", err)
	}
	inline := json.RawMessage(`{
		"version": 1, "name": "ws-b-special", "provider": "claude",
		"turns": [{"steps": [{"emit": {"lines": ["{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false}"]}}]}]
	}`)
	if _, err := h.HarnessSetScenario(HarnessScenarioSpec{Scenario: inline, Cwd: "/ws/b"}); err != nil {
		t.Fatalf("HarnessSetScenario inline: %v", err)
	}

	got, err := h.resolveScenario(control.Registration{Protocol: "claude", Cwd: "/ws/b"})
	if err != nil || got.ScenarioName != "ws-b-special" {
		t.Fatalf("cwd-scoped rule lost to catch-all: %+v, %v", got, err)
	}
	got, err = h.resolveScenario(control.Registration{Protocol: "claude", Cwd: "/ws/other"})
	if err != nil || got.ScenarioName != "streaming-text" {
		t.Fatalf("catch-all rule not applied: %+v, %v", got, err)
	}
	got, err = h.resolveScenario(control.Registration{Protocol: "codex", Cwd: "/ws/b"})
	if err != nil || got.ScenarioName != "codex-basic" {
		t.Fatalf("claude rule leaked to codex: %+v, %v", got, err)
	}

	// Replacement: same (provider, cwd) selector swaps the rule in place.
	if _, err := h.HarnessSetScenario(HarnessScenarioSpec{Name: "streaming-text", Cwd: "/ws/b"}); err != nil {
		t.Fatalf("HarnessSetScenario replace: %v", err)
	}
	list, err := h.HarnessListScenarios()
	if err != nil {
		t.Fatalf("HarnessListScenarios: %v", err)
	}
	if len(list.Rules) != 2 {
		t.Fatalf("rules = %+v, want 2 (replaced, not appended)", list.Rules)
	}
	if len(list.Library) < 2 {
		t.Fatalf("library = %+v, want the shipped scenarios", list.Library)
	}

	if err := h.HarnessClearScenarios(); err != nil {
		t.Fatalf("HarnessClearScenarios: %v", err)
	}
	got, err = h.resolveScenario(control.Registration{Protocol: "claude", Cwd: "/ws/b"})
	if err != nil || got.ScenarioName != "streaming-text" {
		t.Fatalf("clear did not restore fallback: %+v, %v", got, err)
	}
}

// TestHarnessScenarioRulesScopeBySession pins the specificity ladder the
// per-session selector adds: sessionRef beats cwd beats catch-all, a rule
// naming both beats either alone, and — the part a test author has to know —
// an empty ResumeRef matches no session-scoped rule, because that is what a
// session's FIRST spawn looks like.
func TestHarnessScenarioRulesScopeBySession(t *testing.T) {
	h, _ := newHarnessTestApp(t)

	named := func(name string) json.RawMessage {
		return json.RawMessage(`{
			"version": 1, "name": "` + name + `", "provider": "claude",
			"turns": [{"steps": [{"emit": {"lines": ["{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false}"]}}]}]
		}`)
	}

	for _, spec := range []HarnessScenarioSpec{
		{Scenario: named("any-claude")},
		{Scenario: named("in-ws-a"), Cwd: "/ws/a"},
		{Scenario: named("session-only"), SessionRef: "sess-1"},
		{Scenario: named("session-in-ws-a"), Cwd: "/ws/a", SessionRef: "sess-2"},
	} {
		if _, err := h.HarnessSetScenario(spec); err != nil {
			t.Fatalf("HarnessSetScenario %+v: %v", spec, err)
		}
	}

	cases := []struct {
		name      string
		cwd       string
		resumeRef string
		want      string
	}{
		// A first spawn has no resume ref at all, so it can only reach the
		// path-scoped rules. This is the limitation an e2e test works around
		// by restarting the session.
		{"first spawn in a scoped workspace", "/ws/a", "", "in-ws-a"},
		{"first spawn elsewhere", "/ws/other", "", "any-claude"},
		// A resumed session outranks the workspace it resumed in.
		{"session rule beats cwd rule", "/ws/a", "sess-1", "session-only"},
		{"session rule beats catch-all", "/ws/other", "sess-1", "session-only"},
		// Both selectors present is narrower than either alone.
		{"cwd plus session beats session alone", "/ws/a", "sess-2", "session-in-ws-a"},
		// A rule naming a cwd does not match a different one, even for the
		// session it names.
		{"cwd selector still constrains", "/ws/b", "sess-2", "any-claude"},
		// An unknown session falls back rather than matching arbitrarily.
		{"unknown session falls back to cwd", "/ws/a", "sess-99", "in-ws-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := h.resolveScenario(control.Registration{
				Protocol: "claude", Cwd: tc.cwd, ResumeRef: tc.resumeRef,
			})
			if err != nil {
				t.Fatalf("resolveScenario: %v", err)
			}
			if got.ScenarioName != tc.want {
				t.Errorf("scenario = %q, want %q", got.ScenarioName, tc.want)
			}
		})
	}

	// Replacement keys on all three selectors: re-setting the session-scoped
	// rule swaps it, and leaves the three others alone.
	if _, err := h.HarnessSetScenario(HarnessScenarioSpec{
		Scenario: named("session-only-v2"), SessionRef: "sess-1",
	}); err != nil {
		t.Fatalf("HarnessSetScenario replace: %v", err)
	}
	list, err := h.HarnessListScenarios()
	if err != nil {
		t.Fatalf("HarnessListScenarios: %v", err)
	}
	if len(list.Rules) != 4 {
		t.Fatalf("rules = %+v, want 4 (replaced, not appended)", list.Rules)
	}
	var sawSessionRef bool
	for _, rule := range list.Rules {
		if rule.SessionRef == "sess-1" {
			sawSessionRef = true
			if rule.Name != "session-only-v2" {
				t.Errorf("session rule = %q, want the replacement", rule.Name)
			}
		}
	}
	if !sawSessionRef {
		t.Error("HarnessListScenarios dropped the sessionRef selector")
	}
}

func TestHarnessSetScenarioValidation(t *testing.T) {
	h, _ := newHarnessTestApp(t)
	if _, err := h.HarnessSetScenario(HarnessScenarioSpec{}); err == nil {
		t.Fatal("HarnessSetScenario accepted an empty spec")
	}
	if _, err := h.HarnessSetScenario(HarnessScenarioSpec{Name: "no-such-scenario"}); err == nil {
		t.Fatal("HarnessSetScenario accepted an unknown library name")
	}
	if _, err := h.HarnessSetScenario(HarnessScenarioSpec{Scenario: json.RawMessage(`{"version":1}`)}); err == nil {
		t.Fatal("HarnessSetScenario accepted an invalid scenario document")
	}

	// Fixture files must exist at set time: the mock skips a missing
	// fixture mid-scenario with only a stderr log, so a typo'd path would
	// surface as a test hanging on frames that never stream.
	fixtureScenario := func(path string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(
			`{"version":1,"name":"fx","provider":"claude","turns":[{"steps":[{"fixture":{"path":%q}}]}]}`, path))
	}
	if _, err := h.HarnessSetScenario(HarnessScenarioSpec{Scenario: fixtureScenario("missing.ndjson")}); err == nil {
		t.Fatal("HarnessSetScenario accepted a scenario with a missing fixture file")
	}
	fixturePath := filepath.Join(h.paths.DataRoot, "ok.ndjson")
	if err := os.WriteFile(fixturePath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := h.HarnessSetScenario(HarnessScenarioSpec{Scenario: fixtureScenario("ok.ndjson")}); err != nil {
		t.Fatalf("HarnessSetScenario with an existing fixture: %v", err)
	}
	if _, err := h.HarnessSetScenario(HarnessScenarioSpec{Scenario: fixtureScenario(h.paths.DataRoot)}); err == nil {
		t.Fatal("HarnessSetScenario accepted a fixture path that resolves to a directory")
	}

	// A session-scoped Codex rule can never match (the app-server resumes
	// via the thread/resume method, so the mock's ResumeRef is always
	// empty) — the refusal at set time is what keeps that from surfacing
	// as a test hanging on a rule that silently never binds.
	if _, err := h.HarnessSetScenario(HarnessScenarioSpec{Name: "codex-basic", SessionRef: "thread-1"}); err == nil {
		t.Fatal("HarnessSetScenario accepted sessionRef scoping on a codex scenario")
	}
	if _, err := h.HarnessSetScenario(HarnessScenarioSpec{Name: "streaming-text", SessionRef: "sess-1"}); err != nil {
		t.Fatalf("HarnessSetScenario refused sessionRef scoping on a claude scenario: %v", err)
	}
}

// TestHarnessControlRoundTrip drives the real HTTP control channel end
// to end: client registers (scenario assigned via the Harness resolver),
// reports progress (re-emitted as harness:mock), and receives a live
// command queued through HarnessMockCommand.
func TestHarnessControlRoundTrip(t *testing.T) {
	h, app := newHarnessTestApp(t)

	var mu sync.Mutex
	var mockEvents []harnessMockEvent
	app.testEmitHook = func(name string, data any) {
		if name != "harness:mock" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		mockEvents = append(mockEvents, data.(harnessMockEvent))
	}

	if err := h.startControl(); err != nil {
		t.Fatalf("startControl: %v", err)
	}
	t.Cleanup(h.shutdownControl)

	// startControl scopes the control credentials to provider spawns via
	// providerExtraEnv — assert it never leaks them into the process env,
	// then stage them the way a spawned mock would see them so FromEnv
	// (the mock's acquisition path) is what's under test.
	if addr := os.Getenv(control.EnvAddr); addr != "" {
		t.Fatalf("startControl leaked %s=%q into the process env", control.EnvAddr, addr)
	}
	extra := app.providerExtraEnv
	if extra[control.EnvAddr] == "" || extra[control.EnvToken] == "" {
		t.Fatalf("providerExtraEnv missing control credentials: %+v", extra)
	}
	t.Setenv(control.EnvAddr, extra[control.EnvAddr])
	t.Setenv(control.EnvToken, extra[control.EnvToken])
	client, ok := control.FromEnv()
	if !ok {
		t.Fatal("control.FromEnv rejected the staged spawn env")
	}
	// The pid must be a LIVE one: the control server sweeps registrations
	// whose process is gone and refuses to queue commands for them, so a
	// fabricated pid would read as an already-exited mock. The test process
	// itself stands in for the mock provider.
	resp, err := client.Register(control.Registration{Protocol: "claude", Cwd: "/ws/rt", PID: os.Getpid()})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.MockID == "" || len(resp.Scenario) == 0 {
		t.Fatalf("registration response incomplete: %+v", resp)
	}

	mocks, err := h.HarnessListMocks()
	if err != nil || len(mocks) != 1 || mocks[0].MockID != resp.MockID {
		t.Fatalf("HarnessListMocks = %+v, %v", mocks, err)
	}
	if mocks[0].Scenario != "streaming-text" {
		t.Fatalf("assigned scenario = %q", mocks[0].Scenario)
	}

	// Progress report → harness:mock event (registration already
	// produced one).
	client.Report(control.Report{Kind: control.ReportTurnStarted, Turn: 1})
	waitForCond(t, "turn_started event", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, ev := range mockEvents {
			if ev.Report.Kind == control.ReportTurnStarted && ev.MockID == resp.MockID {
				return true
			}
		}
		return false
	})

	// Live command: queued via RPC, delivered through the long-poll.
	if err := h.HarnessMockCommand(resp.MockID, control.Command{Type: control.CommandAdvance, Name: "gate-1"}); err != nil {
		t.Fatalf("HarnessMockCommand: %v", err)
	}
	cmdCh := make(chan control.Command, 1)
	pollCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go client.Poll(pollCtx, func(cmd control.Command) {
		select {
		case cmdCh <- cmd:
		default:
		}
	})
	select {
	case cmd := <-cmdCh:
		if cmd.Type != control.CommandAdvance || cmd.Name != "gate-1" {
			t.Fatalf("delivered command = %+v", cmd)
		}
	case <-pollCtx.Done():
		t.Fatal("queued command never delivered to the polling client")
	}

	// Command validation.
	if err := h.HarnessMockCommand(resp.MockID, control.Command{Type: "reboot"}); err == nil {
		t.Fatal("HarnessMockCommand accepted an unknown command type")
	}
	if err := h.HarnessMockCommand(resp.MockID, control.Command{Type: control.CommandEmit}); err == nil {
		t.Fatal("HarnessMockCommand accepted an emit with no lines")
	}
	if err := h.HarnessMockCommand("mock-999", control.Command{Type: control.CommandAdvance}); err == nil {
		t.Fatal("HarnessMockCommand accepted an unknown mock id")
	}

	// Reset drops the mock registry along with the rest of the
	// harness-owned state — the next test must not see this test's mock.
	if err := h.HarnessReset(); err != nil {
		t.Fatalf("HarnessReset: %v", err)
	}
	mocks, err = h.HarnessListMocks()
	if err != nil {
		t.Fatalf("HarnessListMocks after reset: %v", err)
	}
	if len(mocks) != 0 {
		t.Fatalf("mock registrations survived reset: %+v", mocks)
	}
}

func TestHarnessMockRPCsWithoutControlServer(t *testing.T) {
	h, _ := newHarnessTestApp(t)
	if _, err := h.HarnessListMocks(); err == nil {
		t.Fatal("HarnessListMocks succeeded without a control server")
	}
	if err := h.HarnessMockCommand("mock-1", control.Command{Type: control.CommandAdvance}); err == nil {
		t.Fatal("HarnessMockCommand succeeded without a control server")
	}
}

func TestHarnessClearThreadProviderCursorRequiresAnIdleResumableThread(t *testing.T) {
	h, app := newHarnessTestApp(t)
	seedHarnessThread(t, app, "thread-cursor")
	if _, err := app.store.UpdateSessionRef("thread-cursor", "provider-session"); err != nil {
		t.Fatal(err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID: "thread-cursor:1", ThreadID: "thread-cursor", TurnIndex: 1,
		StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.HarnessClearThreadProviderCursor("thread-cursor"); err == nil || !strings.Contains(err.Error(), "still in flight") {
		t.Fatalf("clear active cursor = %v, want an in-flight refusal", err)
	}
	if err := app.store.UpdateTurnCompleted("thread-cursor:1", time.Now().UnixMilli(), "end_turn", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.HarnessClearThreadProviderCursor("thread-cursor"); err != nil {
		t.Fatalf("clear idle cursor: %v", err)
	}
	thread, err := app.store.GetThread("thread-cursor")
	if err != nil {
		t.Fatal(err)
	}
	if thread.ResolvedSessionRef() != "" {
		t.Fatalf("provider cursor survived clear: %+v", thread)
	}
	if err := h.HarnessClearThreadProviderCursor("thread-cursor"); err == nil || !strings.Contains(err.Error(), "no provider cursor") {
		t.Fatalf("second clear = %v, want an already-empty refusal", err)
	}
}

func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStartControlAlwaysPinsTheMockTranscriptHome covers the keep-home half
// of the provider-home seam on the MOCK's side of the wire.
//
// The transcript home used to be set only when `paths.HomeDir != ""`, which is
// exactly the field `AO_HARNESS_KEEP_HOME` leaves empty — so a keep-home run
// spawned mocks that wrote no transcript at all, and the cold-resume preflight
// silently stopped being exercised in the one mode most likely to be
// reproducing a resume bug. CredentialHome is `<dataRoot>/home` in BOTH modes
// and is the same home `App.providerHome()` resolves, so what the mock writes
// is what the backend reads.
func TestStartControlAlwaysPinsTheMockTranscriptHome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		homeDir string // empty == AO_HARNESS_KEEP_HOME
	}{
		{name: "isolated home", homeDir: "/harness/root/home"},
		{name: "keep-home opt-out", homeDir: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &App{sessions: make(map[string]session)}
			credentialHome := filepath.Join(t.TempDir(), "home")
			h := newHarness(app, harnessPaths{
				DataRoot:       filepath.Dir(credentialHome),
				HomeDir:        tc.homeDir,
				CredentialHome: credentialHome,
			})
			if err := h.startControl(); err != nil {
				t.Fatalf("startControl: %v", err)
			}
			defer h.shutdownControl()

			got := app.providerExtraEnv[control.EnvTranscriptHome]
			if got != credentialHome {
				t.Fatalf("%s = %q, want the credential home %q",
					control.EnvTranscriptHome, got, credentialHome)
			}
		})
	}
}
