package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// The provider a thread that needs the tracker tools starts on is the one
// a new thread starts on when its agents get them and this computer offers
// it, else Claude, else Codex; with neither, the list says why Ask cannot
// start a thread and the Ask refuses, making nothing.
func TestTrackerToolsProviderPicksOneWhoseAgentsGetTheTools(t *testing.T) {
	t.Parallel()
	f := newTicketThreadFixture(t)
	claude, codex := createMockBinary(t, "2.1.284 (Claude Code)"), createMockBinary(t, "codex-cli 0.160.0")
	var at int64
	latest := func(name, model string) {
		t.Helper()
		at += 1000
		if err := f.a.store.UpsertChatModelProfile(store.ChatModelProfile{Provider: name, Model: model, UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
		if seeded := f.a.seedChatModelProfile(f.ctx, "", "").Provider; seeded != name {
			t.Fatalf("a new thread starts on %q, not the last used %q", seeded, name)
		}
	}
	cases := []struct {
		name   string
		patch  map[string]any
		latest func()
		want   string
	}{
		{"the last used, Codex", map[string]any{"claudeBinaryPath": claude, "codexBinaryPath": codex, "agyBinaryPath": claude}, func() { latest("codex", "gpt-5.5") }, "codex"},
		{"the last used without the tools, Antigravity", nil, func() { latest("agy", "gemini-3-pro") }, "claude"},
		{"Claude turned off", map[string]any{"claudeEnabled": false}, nil, "codex"},
		{"Codex not installed either", map[string]any{"codexBinaryPath": filepath.Join(t.TempDir(), "no-codex")}, nil, ""},
	}
	for _, c := range cases {
		if c.patch != nil {
			if _, err := f.a.settings.Update(c.patch); err != nil {
				t.Fatal(err)
			}
		}
		if c.latest != nil {
			c.latest()
		}
		if got := f.a.trackerToolsProvider(f.ctx); got != c.want {
			t.Errorf("%s: provider %q, want %q", c.name, got, c.want)
		}
	}
	list, err := f.a.TrackerDashboards(f.ctx, f.sub)
	if err != nil || list.Ask != noTrackerToolsProvider || len(list.Dashboards) == 0 {
		t.Fatalf("the list with no provider: ask %q, %d dashboards, %v", list.Ask, len(list.Dashboards), err)
	}
	if _, err := f.a.StartTrackerDashboardThread(f.ctx, f.sub); err == nil || !strings.Contains(err.Error(), noTrackerToolsProvider) {
		t.Fatalf("an Ask with no provider: %v", err)
	}
	if threads, err := f.a.store.ListThreadsByProject(f.project); err != nil || len(threads) != 0 {
		t.Fatalf("a refused Ask made threads %+v, %v", threads, err)
	}
	if _, err := f.a.settings.Update(map[string]any{"claudeEnabled": true}); err != nil {
		t.Fatal(err)
	}
	if list, err := f.a.TrackerDashboards(f.ctx, f.sub); err != nil || list.Ask != "" {
		t.Fatalf("the list with Claude: ask %q, %v", list.Ask, err)
	}
}

// Ask your agent makes a thread in the board subscription's project, in
// its checkout, on a provider whose agents get the tracker tools, with its
// draft, unsent, naming the board by its name and site/ID; a draft that
// cannot be saved takes the thread back.
func TestDashboardAskMakesADraftNamingTheBoard(t *testing.T) {
	t.Parallel()
	f := newTicketThreadFixture(t)
	if _, err := f.a.settings.Update(map[string]any{"claudeBinaryPath": createMockBinary(t, "2.1.284 (Claude Code)"), "codexEnabled": false}); err != nil {
		t.Fatal(err)
	}
	made, err := f.a.StartTrackerDashboardThread(f.ctx, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	if made.ProjectID != f.project || made.Provider != "claude" || made.WorktreePath != "" {
		t.Fatalf("the Ask's thread = %+v", made)
	}
	draft, err := f.a.GetDraft(made.ID)
	if err != nil || draft.Content != "About the dashboards of the board WEB board (example.atlassian.net/1): " || len(draft.AttachmentIDs) != 0 {
		t.Fatalf("the Ask's draft = %+v, %v", draft, err)
	}

	execOnStore(t, f.dbPath, `CREATE TRIGGER refuse BEFORE INSERT ON thread_drafts BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	if _, err := f.a.StartTrackerDashboardThread(f.ctx, f.sub); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("an Ask whose draft fails = %v", err)
	}
	if threads, err := f.a.store.ListThreadsByProject(f.project); err != nil || len(threads) != 1 || threads[0].ID != made.ID {
		t.Fatalf("threads after a failed Ask = %+v, %v; want the first alone", threads, err)
	}
}

// Ask your agent reads the board it names, so a session without
// tracker:read is refused before anything is read or made.
func TestDashboardAskNeedsTrackerRead(t *testing.T) {
	t.Parallel()
	app := identityApp(t)
	limited := pairSessionWithScopes(t, app, "thumb-ask", []identity.Scope{identity.ScopeThreadsRead, identity.ScopeThreadsOperate})
	_, err := app.StartTrackerDashboardThread(callFrom(limited.ID, false), "no-such-subscription")
	wantScopeRefusal(t, err, transport.ScopeTrackerRead)
	if _, err := app.StartTrackerDashboardThread(context.Background(), "no-such-subscription"); err == nil {
		t.Fatal("an Ask of no subscription was admitted")
	}
}
