package transport

import (
	"context"
	"testing"
)

func TestCallerScopeContextAndPredicates(t *testing.T) {
	if _, ok := CallerScopeFrom(context.Background()); ok {
		t.Fatal("background context carried a CallerScope")
	}

	phase := CallerScope{
		Kind:      ScopeKindPhase,
		ThreadID:  "thread-1",
		ProjectID: "project-1",
		ItemID:    "item-1",
		PhaseID:   "build",
		Grants:    []string{"remote-commands"},
		Session:   "session-1",
	}
	ctx := WithCallerScope(context.Background(), phase)
	got, ok := CallerScopeFrom(ctx)
	if !ok {
		t.Fatal("CallerScopeFrom did not return attached scope")
	}
	if !got.IsPhase() || got.IsInteractive() {
		t.Fatalf("phase scope predicates = (phase=%v, interactive=%v)", got.IsPhase(), got.IsInteractive())
	}
	if !got.HasGrant("remote-commands") || got.HasGrant("start-run") {
		t.Fatalf("HasGrant mismatch on %+v", got.Grants)
	}

	interactive := CallerScope{Kind: ScopeKindInteractive, ThreadID: "thread-2"}
	if !interactive.IsInteractive() || interactive.IsPhase() {
		t.Fatalf("interactive scope predicates = (phase=%v, interactive=%v)", interactive.IsPhase(), interactive.IsInteractive())
	}
}
