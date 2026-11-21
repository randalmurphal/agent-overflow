package providerlifecycleapp

import (
	"context"
	"testing"

	"agent-overflow/internal/provider"
)

// A usage read of the selected account hands its caller's context to both
// account stages, so the caller's bound covers the wait for the account lock
// in each.
func TestUsageReadPassesItsContextToBothAccountStages(t *testing.T) {
	type callerKey struct{}
	ctx := context.WithValue(context.Background(), callerKey{}, "caller")
	for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
		t.Run(providerName, func(t *testing.T) {
			var reconciled, refreshed bool
			service := New(Deps{Accounts: AccountDeps{
				Reconcile: func(got context.Context, _ string) error {
					reconciled = got.Value(callerKey{}) == "caller"
					return nil
				},
				Selection: func(string) Selection { return Selection{AccountID: "selected"} },
				RefreshUsage: func(got context.Context, _, _ string) error {
					refreshed = got.Value(callerKey{}) == "caller"
					return nil
				},
			}})
			probe := service.ProbeClaudeRateLimits
			if providerName == string(provider.Codex) {
				probe = service.ProbeCodexRateLimits
			}
			if err := probe(ctx); err != nil {
				t.Fatal(err)
			}
			if !reconciled || !refreshed {
				t.Fatalf("caller's context reached reconcile %v, refresh %v; want both", reconciled, refreshed)
			}
		})
	}
}
