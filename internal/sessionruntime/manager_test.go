package sessionruntime

import (
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/transport"
)

func TestStartStateDeduplicatesByThread(t *testing.T) {
	manager := New()
	first, leader := manager.BeginStart("thread")
	if !leader {
		t.Fatal("first start did not lead")
	}
	second, leader := manager.BeginStart("thread")
	if leader || second != first {
		t.Fatal("second start did not join the existing state")
	}
	manager.FinishStart("thread", first)
	if _, ok := manager.StartState("thread"); ok {
		t.Fatal("finished start remained registered")
	}
}

func TestNormalizeCodexBinaryFoldsTheUnsetSetting(t *testing.T) {
	if normalizeCodexBinary("") != "codex" || normalizeCodexBinary("  ") != "codex" {
		t.Fatal("empty binary must fold onto the NewSession default")
	}
	if normalizeCodexBinary(" /usr/bin/codex ") != "/usr/bin/codex" {
		t.Fatal("a configured binary must be trimmed, not rewritten")
	}
}

func TestRecordActivityRejectsAStaleSessionToken(t *testing.T) {
	now := time.Unix(100, 0)
	manager := New()
	manager.Put("thread", Entry{Token: "current", Liveness: NewLiveness(now)})

	manager.RecordActivity("thread", "stale", provider.EventTurnStart, "", now.Add(time.Minute))
	entry, _ := manager.Get("thread")
	if got := entry.Liveness.ActiveTurns.Load(); got != 0 {
		t.Fatalf("active turns after stale token = %d, want 0", got)
	}
	manager.RecordActivity("thread", "current", provider.EventTurnStart, "", now.Add(time.Minute))
	if got := entry.Liveness.ActiveTurns.Load(); got != 1 {
		t.Fatalf("active turns after current token = %d, want 1", got)
	}
}

func TestPutAndRemovalOwnTokenAuthorityAtomically(t *testing.T) {
	manager := New()
	scope := transport.CallerScope{ThreadID: "thread", ProjectID: "project"}
	manager.Put("thread", Entry{Token: "session", AOToken: "ao-token", AOScope: scope})

	if got, ok := manager.ResolveAOToken("ao-token"); !ok || got.ThreadID != scope.ThreadID {
		t.Fatalf("live token = (%+v, %v), want registered scope", got, ok)
	}
	manager.Take("thread")
	if _, ok := manager.ResolveAOToken("ao-token"); ok {
		t.Fatal("token authority survived session removal")
	}
}

func TestUnregisterStaleTokenPreservesReplacementAndAuthority(t *testing.T) {
	manager := New()
	manager.Put("thread", Entry{Token: "fresh", AOToken: "fresh-ao", AOScope: transport.CallerScope{ThreadID: "thread"}})

	if _, ok := manager.Unregister("thread", "stale"); ok {
		t.Fatal("stale unregister removed a replacement")
	}
	entry, ok := manager.Get("thread")
	if !ok || entry.Token != "fresh" {
		t.Fatalf("replacement = (%+v, %v), want fresh session", entry, ok)
	}
	if _, ok := manager.ResolveAOToken("fresh-ao"); !ok {
		t.Fatal("stale unregister revoked the replacement authority")
	}
}

func TestThreadIDsForProviderOrStartingCoversHandoffOnce(t *testing.T) {
	manager := New()
	start, leader := manager.BeginStart("handoff")
	if !leader {
		t.Fatal("first start did not lead")
	}
	manager.Put("handoff", Entry{Provider: string(provider.Claude)})

	ids := manager.ThreadIDsForProviderOrStarting(string(provider.Claude))
	if len(ids) != 1 || ids[0] != "handoff" {
		t.Fatalf("handoff ids = %v, want exactly one", ids)
	}
	manager.FinishStart("handoff", start)
}

func TestThreadIDsForProviderOrStartingIncludesStartingOnly(t *testing.T) {
	manager := New()
	manager.Put("live-claude", Entry{Provider: string(provider.Claude)})
	manager.Put("live-codex", Entry{Provider: string(provider.Codex)})
	start, leader := manager.BeginStart("starting-only")
	if !leader {
		t.Fatal("fresh start did not lead")
	}
	defer manager.FinishStart("starting-only", start)

	ids := manager.ThreadIDsForProviderOrStarting(string(provider.Claude))
	want := map[string]bool{"live-claude": true, "starting-only": true}
	if len(ids) != len(want) {
		t.Fatalf("thread ids = %v, want Claude session and starting thread", ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Fatalf("unexpected thread %q in %v", id, ids)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatalf("missing thread ids: %v", want)
	}
}

func TestSnapshotAndClearRemovesAllEntries(t *testing.T) {
	manager := New()
	manager.Put("claude", Entry{Provider: string(provider.Claude)})
	manager.Put("codex", Entry{Provider: string(provider.Codex)})

	if got := len(manager.SnapshotAndClear()); got != 2 {
		t.Fatalf("snapshot size = %d, want 2", got)
	}
	if got := len(manager.Snapshot()); got != 0 {
		t.Fatalf("remaining sessions = %d, want 0", got)
	}
}

func TestMCPSnapshotsRespectProviderAndWorkspaceOwnership(t *testing.T) {
	manager := New()
	manager.Put("claude-root", Entry{
		Provider:      string(provider.Claude),
		Claude:        &claude.Session{},
		LaunchOptions: provider.SessionOptions{WorkDir: "/repo"},
	})
	manager.Put("claude-worktree", Entry{
		Provider:      string(provider.Claude),
		Claude:        &claude.Session{},
		LaunchOptions: provider.SessionOptions{WorkDir: "/repo/.wt/a"},
	})
	manager.Put("codex", Entry{Provider: string(provider.Codex), Codex: &codex.Session{}})

	claudeSessions := manager.ClaudeMCPSessions("/repo")
	if len(claudeSessions) != 1 || claudeSessions[0].ThreadID != "claude-root" {
		t.Fatalf("Claude MCP sessions = %+v, want matching workspace only", claudeSessions)
	}
	codexSessions := manager.CodexMCPSessions()
	if len(codexSessions) != 1 || codexSessions[0].ThreadID != "codex" {
		t.Fatalf("Codex MCP sessions = %+v, want every Codex process", codexSessions)
	}
}

func TestThreadSystemPromptsNormalizeAndClear(t *testing.T) {
	manager := New()
	if got := manager.ThreadSystemPrompt("missing"); got != "" {
		t.Fatalf("missing prompt = %q, want empty", got)
	}
	manager.SetThreadSystemPrompt(" thread ", " prompt ")
	if got := manager.ThreadSystemPrompt("thread"); got != "prompt" {
		t.Fatalf("stored prompt = %q, want trimmed prompt", got)
	}
	manager.SetThreadSystemPrompt("thread", " \t ")
	if got := manager.ThreadSystemPrompt("thread"); got != "prompt" {
		t.Fatalf("blank update replaced prompt with %q", got)
	}
	manager.ClearThreadSystemPrompt(" thread ")
	if got := manager.ThreadSystemPrompt("thread"); got != "" {
		t.Fatalf("cleared prompt = %q, want empty", got)
	}
}

func TestRemovalPurgesClaudeApplyDegradationAndPromptMemo(t *testing.T) {
	manager := New()
	manager.Put("thread", Entry{Token: "session"})
	manager.RegisterClaudeLiveApplies(map[string]ClaudeLiveApply{
		"command": {SessionToken: "session", Axis: "effort", SentAt: time.Now()},
	}, time.Now(), time.Hour)
	manager.MarkClaudeLiveApplyDegraded("session", "effort")
	manager.PutPromptRender("session", PromptRender{Rendered: "memo"})

	manager.Take("thread")
	if _, ok := manager.TakeClaudeLiveApply("command"); ok {
		t.Fatal("pending live apply survived session removal")
	}
	if manager.ClaudeLiveApplyIsDegraded("session", claude.LiveUpdate{Effort: "low"}, "effort", "fast") {
		t.Fatal("degraded mark survived session removal")
	}
	if _, ok := manager.PromptRender("session"); ok {
		t.Fatal("prompt memo survived session removal")
	}
}

func TestConcurrentActivityAndSnapshot(t *testing.T) {
	manager := New()
	manager.Put("thread", Entry{Token: "token", Liveness: NewLiveness(time.Now())})
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 100 {
				manager.RecordActivity("thread", "token", provider.EventTurnStart, "", time.Now())
				manager.RecordActivity("thread", "token", provider.EventTurnComplete, "", time.Now())
				manager.Snapshot()
			}
		}()
	}
	wait.Wait()
	entry, _ := manager.Get("thread")
	if got := entry.Liveness.ActiveTurns.Load(); got != 0 {
		t.Fatalf("active turns = %d, want 0", got)
	}
}
