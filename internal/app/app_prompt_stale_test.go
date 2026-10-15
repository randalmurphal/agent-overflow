package app

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerstatus"
	"agent-overflow/internal/testutil/mockexec"
)

// recordProviderStatuses installs a provider:status recorder and returns the
// function that takes what was recorded since the last take.
func recordProviderStatuses(app *App) func() []providerstatus.Event {
	var mu sync.Mutex
	var statuses []providerstatus.Event
	app.testEmitHook = func(name string, data any) {
		if name != "provider:status" {
			return
		}
		if event, ok := data.(providerstatus.Event); ok {
			mu.Lock()
			statuses = append(statuses, event)
			mu.Unlock()
		}
	}
	return func() []providerstatus.Event {
		mu.Lock()
		defer mu.Unlock()
		taken := statuses
		statuses = nil
		return taken
	}
}

// registerPromptStaleSession registers a live session for the thread with
// the options its spawn would use today, without a provider process: the
// prompt change check reads only the provider, token and launch options.
func registerPromptStaleSession(t *testing.T, app *App, threadID, providerName, token string) {
	t.Helper()
	app.sessionManager().put(threadID, session{
		Provider:      providerName,
		Token:         token,
		LaunchOptions: promptOverrideOptions(t, app, threadID),
		Liveness:      newSessionLiveness(time.Now()),
	})
}

func expectOnePromptStale(t *testing.T, events []providerstatus.Event, threadID, providerName string) providerstatus.Event {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("provider:status emissions = %+v, want one prompt_stale", events)
	}
	event := events[0]
	if event.Kind != providerstatus.KindPromptStale || event.ThreadID != threadID || event.Provider != providerName {
		t.Fatalf("event = %+v, want prompt_stale for %s on %s", event, threadID, providerName)
	}
	if !event.Actionable || event.Message != promptStaleMessage || event.Revision <= 0 {
		t.Fatalf("event = %+v, want an actionable banner with its message and a revision", event)
	}
	return event
}

func expectOneWithdraw(t *testing.T, events []providerstatus.Event, threadID, kind string) {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("provider:status emissions = %+v, want one %s withdraw", events, kind)
	}
	event := events[0]
	if event.Status != "ready" || event.Kind != "" || event.ThreadID != threadID || event.WithdrawKind != kind {
		t.Fatalf("event = %+v, want a thread-scoped ready withdrawing %s", event, kind)
	}
}

func expectLivePromptStale(t *testing.T, app *App, threadID string, want bool, revision int) {
	t.Helper()
	live, err := app.GetThreadLiveState(threadID)
	if err != nil {
		t.Fatalf("GetThreadLiveState: %v", err)
	}
	if live.PromptStale != want || live.PromptStaleRevision != revision {
		t.Fatalf("live state prompt stale = (%v, %d), want (%v, %d)", live.PromptStale, live.PromptStaleRevision, want, revision)
	}
}

// Every kind of change to the override a live thread's next start would
// use raises the banner, on every provider: an edit, an enable, a disable,
// a removal.
func TestPromptChangeFlagsTheLiveThreadOnEveryProvider(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ providerName, model, key string }{
		{string(provider.Claude), "claude-opus-5", "claudePromptOverrides"},
		{string(provider.ClaudeTUI), "claude-opus-5", "claudePromptOverrides"},
		{string(provider.Codex), "gpt-5.4", "codexPromptOverrides"},
	} {
		for _, change := range promptOverrideTransitions(tc.model) {
			t.Run(tc.providerName+"/"+change.name, func(t *testing.T) {
				t.Parallel()
				app := newTestAppWithStore(t)
				id, _ := seedPromptOverrideThread(t, app, "thread-stale-"+tc.providerName+"-"+change.name, tc.providerName, tc.model)
				setPromptOverrides(t, app, tc.key, change.before...)
				registerPromptStaleSession(t, app, id, tc.providerName, "token-1")
				take := recordProviderStatuses(app)

				setPromptOverrides(t, app, tc.key, change.after...)
				event := expectOnePromptStale(t, take(), id, tc.providerName)
				expectLivePromptStale(t, app, id, true, event.Revision)
			})
		}
	}
}

// The banner's whole life on one live thread: a change within stale is a
// new revision, a dismissal hides it until the next change, an unchanged
// save stays hidden, a revert to the running prompt withdraws it, and a
// change after the revert raises a revision no earlier one used.
func TestPromptStaleRevisionsAndDismissal(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	const key, model = "claudePromptOverrides", "claude-opus-5"
	claude := string(provider.Claude)
	id, _ := seedPromptOverrideThread(t, app, "thread-stale-life", claude, model)
	entry := func(prompt string) testPromptEntry {
		return testPromptEntry{Enabled: true, Models: []string{model}, Prompt: prompt}
	}
	setPromptOverrides(t, app, key, entry("v1"))
	registerPromptStaleSession(t, app, id, claude, "token-1")
	take := recordProviderStatuses(app)

	// A save that changes nothing about this thread raises nothing.
	setPromptOverrides(t, app, key, entry("v1"))
	if events := take(); len(events) != 0 {
		t.Fatalf("an unchanged save emitted %+v", events)
	}
	expectLivePromptStale(t, app, id, false, 0)

	setPromptOverrides(t, app, key, entry("v2"))
	first := expectOnePromptStale(t, take(), id, claude)
	expectLivePromptStale(t, app, id, true, first.Revision)

	setPromptOverrides(t, app, key, entry("v3"))
	second := expectOnePromptStale(t, take(), id, claude)
	if second.Revision <= first.Revision {
		t.Fatalf("revision within stale = %d, want above %d", second.Revision, first.Revision)
	}
	expectLivePromptStale(t, app, id, true, second.Revision)

	if err := app.DismissSessionBanner(id, providerstatus.KindPromptStale); err != nil {
		t.Fatalf("DismissSessionBanner: %v", err)
	}
	expectOneWithdraw(t, take(), id, providerstatus.KindPromptStale)
	expectLivePromptStale(t, app, id, false, 0)
	if err := app.DismissSessionBanner(id, providerstatus.KindPromptStale); err != nil {
		t.Fatalf("DismissSessionBanner again: %v", err)
	}
	if events := take(); len(events) != 0 {
		t.Fatalf("a second dismissal emitted %+v", events)
	}

	setPromptOverrides(t, app, key, entry("v3"))
	if events := take(); len(events) != 0 {
		t.Fatalf("an unchanged save after a dismissal emitted %+v", events)
	}
	expectLivePromptStale(t, app, id, false, 0)

	setPromptOverrides(t, app, key, entry("v4"))
	third := expectOnePromptStale(t, take(), id, claude)
	if third.Revision <= second.Revision {
		t.Fatalf("revision after a dismissal = %d, want above %d", third.Revision, second.Revision)
	}
	expectLivePromptStale(t, app, id, true, third.Revision)

	setPromptOverrides(t, app, key, entry("v1"))
	expectOneWithdraw(t, take(), id, providerstatus.KindPromptStale)
	expectLivePromptStale(t, app, id, false, 0)

	setPromptOverrides(t, app, key, entry("v2"))
	fourth := expectOnePromptStale(t, take(), id, claude)
	if fourth.Revision <= third.Revision {
		t.Fatalf("revision after a revert = %d, want above %d", fourth.Revision, third.Revision)
	}
}

// The flag is a claim about the session that earned it. Once another
// session holds the thread, hydration answers nothing before any trigger
// runs, and the next recompute withdraws the flag the new session does not
// carry.
func TestPromptStaleIsPinnedToTheSessionThatEarnedIt(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	const key, model = "claudePromptOverrides", "claude-opus-5"
	claude := string(provider.Claude)
	id, _ := seedPromptOverrideThread(t, app, "thread-stale-token", claude, model)
	setPromptOverrides(t, app, key, testPromptEntry{Enabled: true, Models: []string{model}, Prompt: "v1"})
	registerPromptStaleSession(t, app, id, claude, "token-1")
	take := recordProviderStatuses(app)
	setPromptOverrides(t, app, key, testPromptEntry{Enabled: true, Models: []string{model}, Prompt: "v2"})
	event := expectOnePromptStale(t, take(), id, claude)
	expectLivePromptStale(t, app, id, true, event.Revision)

	// A replacement registered with today's prompt, without the session
	// leaving through the disconnect path first.
	registerPromptStaleSession(t, app, id, claude, "token-2")
	expectLivePromptStale(t, app, id, false, 0)
	app.reconcileStalePromptSessions()
	expectOneWithdraw(t, take(), id, providerstatus.KindPromptStale)
}

// Through the real session lifecycle: a restart withdraws the banner and
// the replacement, launched with today's prompt, raises none; a stopped
// session withdraws it at once and a later change raises nothing for it.
func TestPromptStaleIsWithdrawnOnRestartAndOnDeath(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	const key, model = "claudePromptOverrides", "claude-opus-5"
	claude := string(provider.Claude)
	id, _ := startOverrideSession(t, app, "thread-stale-restart", provider.Claude, model, "v1")
	take := recordProviderStatuses(app)

	setPromptOverrides(t, app, key, testPromptEntry{Enabled: true, Models: []string{model}, Prompt: "v2"})
	expectOnePromptStale(t, take(), id, claude)

	if err := app.ReconnectSession(id); err != nil {
		t.Fatalf("ReconnectSession: %v", err)
	}
	expectOneWithdraw(t, take(), id, providerstatus.KindPromptStale)
	expectLivePromptStale(t, app, id, false, 0)
	live, ok := app.sessionManager().get(id)
	if !ok || live.LaunchOptions.SystemPromptOverrideSource != "v2" {
		t.Fatalf("restarted session launch source = %q (live %v), want the edited prompt", live.LaunchOptions.SystemPromptOverrideSource, ok)
	}

	setPromptOverrides(t, app, key, testPromptEntry{Enabled: true, Models: []string{model}, Prompt: "v3"})
	expectOnePromptStale(t, take(), id, claude)
	if err := app.StopSession(id); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	expectOneWithdraw(t, take(), id, providerstatus.KindPromptStale)
	expectLivePromptStale(t, app, id, false, 0)

	setPromptOverrides(t, app, key, testPromptEntry{Enabled: true, Models: []string{model}, Prompt: "v4"})
	if events := take(); len(events) != 0 {
		t.Fatalf("a change after the session stopped emitted %+v", events)
	}
}

// The override that matches is chosen by model, so a model change can move
// a live thread's next prompt with no settings change at all.
func TestAModelChangeCanFlagTheLiveThread(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	claude := string(provider.Claude)
	id, workspace := seedPromptOverrideThread(t, app, "thread-stale-model", claude, "claude-opus-5")
	setPromptOverrides(t, app, "claudePromptOverrides",
		testPromptEntry{Enabled: true, Models: []string{"claude-opus-5"}, Prompt: "opus prompt"},
		testPromptEntry{Enabled: true, Models: []string{"claude-fable-5"}, Prompt: "fable prompt"},
	)
	registerLiveClaudeCaptureSession(t, app, id, workspace, promptOverrideOptions(t, app, id))
	take := recordProviderStatuses(app)

	if _, err := app.UpdateThreadModel(id, "claude-fable-5"); err != nil {
		t.Fatalf("UpdateThreadModel: %v", err)
	}
	expectOnePromptStale(t, take(), id, claude)
}

// A dismissal names its kind; anything else is refused rather than
// silently ignored.
func TestDismissSessionBannerRefusesAnUnknownKind(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	if err := app.DismissSessionBanner("thread-x", "provider_down"); err == nil || !strings.Contains(err.Error(), "provider_down") {
		t.Fatalf("DismissSessionBanner(unknown kind) = %v, want an error naming the kind", err)
	}
	if err := app.DismissSessionBanner(" ", providerstatus.KindPromptStale); err == nil {
		t.Fatal("DismissSessionBanner(empty thread) = nil, want an error")
	}
}

// The stale-binary banner dismisses the same way: hidden for the installed
// version it named, shown again when the installed version moves, and its
// withdrawals name their kind.
func TestBinaryStaleDismissalHoldsUntilTheVersionMoves(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	claude := string(provider.Claude)
	id, workspace := seedPromptOverrideThread(t, app, "thread-binary-dismiss", claude, "claude-opus-5")
	registerLiveClaudeCaptureSession(t, app, id, workspace, promptOverrideOptions(t, app, id))
	take := recordProviderStatuses(app)

	app.providerBinaries.storeInstalled(claude, providerBinaryVersion{version: "2.1.300"})
	app.reconcileStaleProviderSessions()
	if events := take(); len(events) != 1 || events[0].Kind != providerstatus.KindBinaryStale || events[0].InstalledVersion != "2.1.300" {
		t.Fatalf("emissions = %+v, want one binary_stale for 2.1.300", events)
	}

	if err := app.DismissSessionBanner(id, providerstatus.KindBinaryStale); err != nil {
		t.Fatalf("DismissSessionBanner: %v", err)
	}
	expectOneWithdraw(t, take(), id, providerstatus.KindBinaryStale)
	if session, installed := app.staleProviderBinaryVersions(id); session != "" || installed != "" {
		t.Fatalf("hydration after a dismissal = (%q, %q), want nothing", session, installed)
	}
	app.reconcileStaleProviderSessions()
	if events := take(); len(events) != 0 {
		t.Fatalf("an unchanged tick after a dismissal emitted %+v", events)
	}
	if session, installed := app.staleProviderBinaryVersions(id); session != "" || installed != "" {
		t.Fatalf("hydration after an unchanged tick = (%q, %q), want the dismissal kept", session, installed)
	}

	app.providerBinaries.storeInstalled(claude, providerBinaryVersion{version: "2.1.400"})
	app.reconcileStaleProviderSessions()
	if events := take(); len(events) != 1 || events[0].Kind != providerstatus.KindBinaryStale || events[0].InstalledVersion != "2.1.400" {
		t.Fatalf("emissions = %+v, want binary_stale shown again for 2.1.400", events)
	}
	if _, installed := app.staleProviderBinaryVersions(id); installed != "2.1.400" {
		t.Fatalf("hydrated installed version = %q, want 2.1.400", installed)
	}

	app.providerBinaries.storeInstalled(claude, providerBinaryVersion{version: "2.1.237"})
	app.reconcileStaleProviderSessions()
	expectOneWithdraw(t, take(), id, providerstatus.KindBinaryStale)
}

// A prompt edit that lands while a session spawns reaches no flag through
// the settings trigger: the session is not registered yet. The start
// recomputes for the session it registers, so it is flagged. The fake Codex
// rewrites the override's file while it answers initialize, inside the
// spawn and after the spawn read the prompt.
func TestASessionStartedAcrossAPromptEditIsFlagged(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	codex := string(provider.Codex)
	id, _ := seedPromptOverrideThread(t, app, "thread-stale-spawn", codex, "gpt-5.4")
	entries := setPromptOverrides(t, app, "codexPromptOverrides", testPromptEntry{Enabled: true, Models: []string{"gpt-5.4"}, Prompt: "v1"})
	script := `#!/bin/bash
edited=0
while IFS= read -r line; do
  id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
  if [ -z "$id" ]; then continue; fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
    if [ "$edited" = 0 ]; then printf 'edited during the spawn\n' > ` + shellQuote(entries[0].Path) + `; edited=1; fi
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"; continue
  fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"codex-stale-spawn"}}}\n' "$id"; continue
  fi
  printf '{"jsonrpc":"2.0","id":%s,"result":{"config":{},"origins":{}}}\n' "$id"
done
`
	binary := filepath.Join(t.TempDir(), "mock-codex")
	mockexec.Write(t, binary, script)
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("set codex binary: %v", err)
	}
	take := recordProviderStatuses(app)

	if err := app.StartSession(id); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = app.StopSession(id) })
	live, ok := app.sessionManager().get(id)
	if !ok || live.LaunchOptions.SystemPromptOverrideSource != "v1" {
		t.Fatalf("launch source = %q (live %v), want the prompt the spawn read", live.LaunchOptions.SystemPromptOverrideSource, ok)
	}
	expectOnePromptStale(t, take(), id, codex)
}
