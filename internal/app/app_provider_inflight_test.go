package app

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// openClaudeTurn puts thread into the triage state a running Claude turn
// leaves: an acknowledged user message after system/init. Claude never moves
// the runtime turn counter, so this is the only in-flight signal it gives.
func openClaudeTurn(t *testing.T, app *App, threadID string) {
	t.Helper()
	if _, err := app.store.UpsertItem(store.Item{ID: "user:0", ThreadID: threadID, TurnIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: "go"}, nil); err != nil {
		t.Fatal(err)
	}
	userItem := "user-" + threadID
	app.triage.RegisterPendingSendWithExpectation(threadID, "user:0", 0, triage.PendingSendExpectation{ProviderItemID: userItem})
	frames, err := claude.ParseLine(threadID, []byte(`{"type":"system","subtype":"init","session_id":"session-`+threadID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range frames {
		if err := app.triage.Handle(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.triage.Handle(provider.ProviderEvent{Kind: provider.EventUserText, ThreadID: threadID, ItemID: userItem, Content: "go", Meta: json.RawMessage(`{"provider_item_id":"` + userItem + `"}`), Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if !app.triage.HasInFlightTurnOrRound(threadID) {
		t.Fatal("fixture must leave a Claude turn in flight")
	}
}

func openCodexTurn(t *testing.T, app *App, threadID string) {
	t.Helper()
	if err := app.triage.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: threadID, TurnID: "turn-" + threadID, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if !app.triage.HasInFlightTurnOrRound(threadID) {
		t.Fatal("fixture must leave a Codex turn in flight")
	}
}

func putLiveSession(t *testing.T, app *App, threadID, providerName, launchModel, storedModel string) {
	t.Helper()
	thread := testThread(threadID)
	thread.Provider = providerName
	thread.Model = storedModel
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	app.sessionManager().put(threadID, session{
		Provider: providerName, Token: "token-" + threadID,
		LaunchOptions: provider.SessionOptions{Model: launchModel},
		Liveness:      newSessionLiveness(time.Now()),
	})
}

func TestProviderTurnsInFlightGroupsByUsageFamily(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()

	putLiveSession(t, app, "claude-running", string(provider.Claude), "claude-opus-5", "claude-opus-5")
	openClaudeTurn(t, app, "claude-running")
	putLiveSession(t, app, "tui-running", string(provider.ClaudeTUI), "claude-sonnet-5", "claude-sonnet-5")
	openClaudeTurn(t, app, "tui-running")
	putLiveSession(t, app, "claude-idle", string(provider.Claude), "claude-opus-5", "claude-opus-5")
	putLiveSession(t, app, "codex-running", string(provider.Codex), "gpt-5.4", "gpt-5.4")
	openCodexTurn(t, app, "codex-running")
	putLiveSession(t, app, "codex-idle", string(provider.Codex), "gpt-5.4", "gpt-5.4")

	sorted := func(turns []providerInFlightTurn) []providerInFlightTurn {
		slices.SortFunc(turns, func(a, b providerInFlightTurn) int {
			if a.ThreadID < b.ThreadID {
				return -1
			}
			if a.ThreadID > b.ThreadID {
				return 1
			}
			return 0
		})
		return turns
	}
	wantClaude := []providerInFlightTurn{
		{ThreadID: "claude-running", Model: "claude-opus-5"},
		{ThreadID: "tui-running", Model: "claude-sonnet-5"},
	}
	for _, family := range []string{string(provider.Claude), string(provider.ClaudeTUI)} {
		if got := sorted(app.providerTurnsInFlight(family)); !slices.Equal(got, wantClaude) {
			t.Fatalf("%s turns in flight = %+v, want %+v", family, got, wantClaude)
		}
	}
	wantCodex := []providerInFlightTurn{{ThreadID: "codex-running", Model: "gpt-5.4"}}
	if got := app.providerTurnsInFlight(string(provider.Codex)); !slices.Equal(got, wantCodex) {
		t.Fatalf("codex turns in flight = %+v, want %+v", got, wantCodex)
	}
	if !app.providerHasTurnInFlight(string(provider.Claude)) || !app.providerHasTurnInFlight(string(provider.Codex)) {
		t.Fatal("providerHasTurnInFlight disagrees with providerTurnsInFlight")
	}
	if app.providerHasTurnInFlight("other") {
		t.Fatal("an unknown provider reported a turn in flight")
	}
}

func TestRunningModelPrecedence(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()

	putLiveSession(t, app, "fallback", string(provider.Claude), "claude-opus-5", "claude-opus-5")
	openClaudeTurn(t, app, "fallback")
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventModelFallback, ThreadID: "fallback", ItemID: "model-fallback:1",
		Meta: json.RawMessage(`{"originalModel":"claude-opus-5","fallbackModel":"claude-sonnet-5"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if got := app.providerTurnsInFlight(string(provider.Claude)); len(got) != 1 || got[0].Model != "claude-sonnet-5" {
		t.Fatalf("turns in flight = %+v, want the fallback model", got)
	}
	thread, err := app.store.GetThread("fallback")
	if err != nil {
		t.Fatal(err)
	}
	if got := app.threadRunningModel(thread); got != "claude-sonnet-5" {
		t.Fatalf("running model with a fallback = %q", got)
	}

	// The launch model wins over a stored model a later change rewrote.
	putLiveSession(t, app, "launched", string(provider.Claude), "claude-opus-5", "claude-haiku-5")
	launched, err := app.store.GetThread("launched")
	if err != nil {
		t.Fatal(err)
	}
	if got := app.threadRunningModel(launched); got != "claude-opus-5" {
		t.Fatalf("running model of a live session = %q, want its launch model", got)
	}

	// No live session: the stored model.
	stored := testThread("stored")
	stored.Model = "gpt-5.4"
	if got := app.threadRunningModel(stored); got != "gpt-5.4" {
		t.Fatalf("running model with no session = %q, want the stored model", got)
	}
}

// A launch or stored alias is read through the alias map the Claude CLI
// reported, so a limit scoped to the model it names applies to the thread.
// An alias the CLI did not report, and any Codex model, is returned as
// written. Serial: it replaces the process-wide model catalog.
func TestRunningModelResolvesReportedClaudeAlias(t *testing.T) {
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()
	claudecatalog.Reset()
	t.Cleanup(claudecatalog.Reset)
	capture := claudecatalog.ModelCapture{}
	capture.Capture([]claude.WireModel{{Value: "opus[1m]", ResolvedModel: "claude-fable-5-2[1m]"}}, nil)
	capture.Store(app.claudeProbeModelKey())

	for _, tc := range []struct{ provider, model, want string }{
		{string(provider.Claude), "opus", "claude-fable-5-2"},
		{string(provider.Claude), "opus[1m]", "claude-fable-5-2"},
		{string(provider.ClaudeTUI), "opus", "claude-fable-5-2"},
		{string(provider.Claude), "sonnet", "sonnet"},
		{string(provider.Claude), "claude-opus-5", "claude-opus-5"},
		{string(provider.Codex), "opus", "opus"},
	} {
		stored := testThread("stored")
		stored.Provider, stored.Model = tc.provider, tc.model
		if got := app.threadRunningModel(stored); got != tc.want {
			t.Errorf("running model of %s %q = %q, want %q", tc.provider, tc.model, got, tc.want)
		}
	}

	putLiveSession(t, app, "launched", string(provider.Claude), "opus", "claude-haiku-5")
	entry, _ := app.sessionManager().get("launched")
	if got := app.sessionRunningModel("launched", entry); got != "claude-fable-5-2" {
		t.Fatalf("running model of a session launched on an alias = %q", got)
	}
}

// A claude-tui session runs the claude binary on the Claude account, so its
// completed turn is Claude activity for the usage poll.
func TestClaudeTUITurnCompleteRecordsClaudeActivity(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	mark := time.Now().Add(-time.Millisecond)
	handler := app.sessionEventHandler("thread-tui", "tok-tui", string(provider.ClaudeTUI))
	handler(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "thread-tui", Timestamp: time.Now()})
	waitProviderEvents(t, app, "thread-tui")
	if !app.providerLifecycleService().TurnCompletedSince(string(provider.Claude), mark) {
		t.Fatal("a claude-tui turn did not record Claude activity")
	}
	if app.providerLifecycleService().TurnCompletedSince(string(provider.Codex), mark) {
		t.Fatal("a claude-tui turn recorded Codex activity")
	}
}
