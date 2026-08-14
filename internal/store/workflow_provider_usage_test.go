package store

import (
	"fmt"
	"testing"
)

func TestWorkflowProviderUsageScopeAndAttentionTransitions(t *testing.T) {
	s := newTestStore(t)
	scopeID, err := s.OpenWorkflowProviderUsageScope("claude", "acct-a", 7, 10)
	if err != nil {
		t.Fatal(err)
	}
	sameID, err := s.OpenWorkflowProviderUsageScope("claude", "acct-a", 7, 20)
	if err != nil {
		t.Fatal(err)
	}
	if sameID != scopeID {
		t.Fatalf("same provider account generation produced scopes %d and %d", scopeID, sameID)
	}
	if _, err := s.OpenWorkflowProviderUsageScope("claude", "acct-a", 7, 15); err != nil {
		t.Fatal(err)
	}
	scope, err := s.GetWorkflowProviderUsageScope(scopeID)
	if err != nil {
		t.Fatal(err)
	}
	if scope.FirstSeenAt != 10 || scope.LastSeenAt != 20 || scope.Provider != "claude" ||
		scope.AccountID != "acct-a" || scope.CredentialGeneration != 7 {
		t.Fatalf("scope = %+v", scope)
	}
	if _, _, err := s.ClaimWorkflowProviderUsageAttention(scopeID, "watcher", "", "token", 20); err == nil {
		t.Fatal("attention claim accepted an empty source run")
	}

	first, claimed, err := s.ClaimWorkflowProviderUsageAttention(scopeID, "watcher", "run-a", "token-a", 21)
	if err != nil || !claimed {
		t.Fatalf("first claim = %+v claimed=%v err=%v", first, claimed, err)
	}
	if _, claimed, err := s.ClaimWorkflowProviderUsageAttention(scopeID, "watcher", "run-b", "token-b", 22); err != nil || claimed {
		t.Fatalf("parallel claim claimed=%v err=%v, want queued suppression", claimed, err)
	}
	if promoted, err := s.PromoteWorkflowProviderUsageAttention(first, 23); err != nil || !promoted {
		t.Fatalf("promote first = %v err=%v", promoted, err)
	}
	if _, claimed, err := s.ClaimWorkflowProviderUsageAttention(scopeID, "watcher", "run-c", "token-c", 24); err != nil || claimed {
		t.Fatalf("post-delivery claim claimed=%v err=%v, want delivered suppression", claimed, err)
	}

	if err := s.RearmWorkflowProviderUsageAttention("watcher", 25); err != nil {
		t.Fatal(err)
	}
	// A callback from the pre-action message cannot mark the new attention
	// generation delivered.
	if promoted, err := s.PromoteWorkflowProviderUsageAttention(first, 26); err != nil || promoted {
		t.Fatalf("stale promotion = %v err=%v", promoted, err)
	}
	second, claimed, err := s.ClaimWorkflowProviderUsageAttention(scopeID, "watcher", "run-late", "token-late", 27)
	if err != nil || !claimed || second.Generation != first.Generation+1 {
		t.Fatalf("rearmed claim = %+v claimed=%v err=%v", second, claimed, err)
	}
	if released, err := s.ReleaseWorkflowProviderUsageAttention(second, 28); err != nil || !released {
		t.Fatalf("release = %v err=%v", released, err)
	}
	if _, claimed, err := s.ClaimWorkflowProviderUsageAttention(scopeID, "watcher", "run-retry", "token-retry", 29); err != nil || !claimed {
		t.Fatalf("claim after failed delivery = %v err=%v", claimed, err)
	}

	// Neither another watching conversation nor another account shares the
	// claim. Provider-wide correlation never crosses an account boundary.
	if _, claimed, err := s.ClaimWorkflowProviderUsageAttention(scopeID, "other-watcher", "run-d", "token-d", 30); err != nil || !claimed {
		t.Fatalf("other watcher claim = %v err=%v", claimed, err)
	}
	otherScope, err := s.OpenWorkflowProviderUsageScope("claude", "acct-b", 8, 30)
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimWorkflowProviderUsageAttention(otherScope, "watcher", "run-e", "token-e", 31); err != nil || !claimed {
		t.Fatalf("other account claim = %v err=%v", claimed, err)
	}
	newCredentialScope, err := s.OpenWorkflowProviderUsageScope("claude", "acct-a", 8, 31)
	if err != nil {
		t.Fatal(err)
	}
	if newCredentialScope == scopeID {
		t.Fatal("a replaced credential reused the old provider usage scope")
	}
	if _, claimed, err := s.ClaimWorkflowProviderUsageAttention(newCredentialScope, "watcher", "run-f", "token-f", 32); err != nil || !claimed {
		t.Fatalf("replacement credential claim = %v err=%v, want independent attention", claimed, err)
	}
}

func TestReclaimQueuedWorkflowProviderUsageAttentionTransfersWithoutClearing(t *testing.T) {
	s := newTestStore(t)
	first, err := s.OpenWorkflowProviderUsageScope("claude", "acct", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.OpenWorkflowProviderUsageScope("codex", "acct", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	for scopeID, source := range map[WorkflowProviderUsageScopeID]string{first: "run-a", second: "run-b"} {
		if _, claimed, err := s.ClaimWorkflowProviderUsageAttention(scopeID, "watcher", source, source+"-token", 2); err != nil || !claimed {
			t.Fatalf("claim %s = %v err=%v", source, claimed, err)
		}
	}
	recoveries, err := s.ReclaimQueuedWorkflowProviderUsageAttention("boot-one", 3)
	if err != nil {
		t.Fatal(err)
	}
	wantRecoveries := []WorkflowProviderUsageAttentionRecovery{
		{Claim: WorkflowProviderUsageAttentionClaim{
			ScopeID: first, ThreadID: "watcher", Generation: 1, Token: "boot-one:" + fmt.Sprint(first) + ":watcher",
		}, SourceItemID: "run-a"},
		{Claim: WorkflowProviderUsageAttentionClaim{
			ScopeID: second, ThreadID: "watcher", Generation: 1, Token: "boot-one:" + fmt.Sprint(second) + ":watcher",
		}, SourceItemID: "run-b"},
	}
	if len(recoveries) != len(wantRecoveries) {
		t.Fatalf("reclaimed recoveries = %+v", recoveries)
	}
	for index := range recoveries {
		if recoveries[index] != wantRecoveries[index] {
			t.Fatalf("reclaimed recovery %d = %+v, want %+v", index, recoveries[index], wantRecoveries[index])
		}
	}
	if _, claimed, err := s.ClaimWorkflowProviderUsageAttention(first, "watcher", "suppressed", "suppressed-token", 4); err != nil || claimed {
		t.Fatalf("reclaimed durable reservation did not suppress: claimed=%v err=%v", claimed, err)
	}
	// A second process crash transfers the still-queued claims again rather than
	// finding an empty reset gap.
	secondRecovery, err := s.ReclaimQueuedWorkflowProviderUsageAttention("boot-two", 5)
	if err != nil || len(secondRecovery) != 2 {
		t.Fatalf("second recovery = %+v err=%v", secondRecovery, err)
	}
	for index := range secondRecovery {
		if secondRecovery[index].Claim.Token == recoveries[index].Claim.Token {
			t.Fatalf("claim %d was not transferred: %+v", index, secondRecovery[index])
		}
		if released, err := s.ReleaseWorkflowProviderUsageAttention(recoveries[index].Claim, 6); err != nil || released {
			t.Fatalf("stale recovery %d released=%v err=%v", index, released, err)
		}
		if released, err := s.ReleaseWorkflowProviderUsageAttention(secondRecovery[index].Claim, 7); err != nil || !released {
			t.Fatalf("current recovery %d released=%v err=%v", index, released, err)
		}
	}
	for scopeID, token := range map[WorkflowProviderUsageScopeID]string{first: "retry-a", second: "retry-b"} {
		if _, claimed, err := s.ClaimWorkflowProviderUsageAttention(scopeID, "watcher", "run", token, 8); err != nil || !claimed {
			t.Fatalf("claim scope %d after release = %v err=%v", scopeID, claimed, err)
		}
	}
}
