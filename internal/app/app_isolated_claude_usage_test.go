package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/claudefake"
)

// An isolated boot answers Claude usage for an identity the harness set a
// reading for, through the real probe and provider:usage, and still refuses
// the endpoint for any identity it holds nothing for.
func TestIsolatedBootAnswersClaudeUsageOnlyForASetIdentity(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	isolated := &App{}
	ConfigureIsolation(isolated, IsolationConfig{})
	app.rateLimitProbeClientOverride = isolated.rateLimitProbeClientOverride
	app.claudeUsageFake = isolated.claudeUsageFake
	if app.claudeUsageFake == nil {
		t.Fatal("ConfigureIsolation installed no Claude usage fake")
	}

	const email = "usage@agent-overflow.test"
	credsDir := filepath.Join(testProviderHome(t, app), ".claude")
	if err := os.MkdirAll(credsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credential := `{"claudeAiOauth":{"accessToken":"` + claudefake.AccessToken(email, "org", "1") + `"}}`
	if err := os.WriteFile(filepath.Join(credsDir, ".credentials.json"), []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}
	readings := make(chan provider.RateLimitsSnapshot, 4)
	app.testEmitHook = func(name string, data any) {
		if usage, ok := data.(provider.UsageEvent); ok && name == "provider:usage" && usage.RateLimits != nil {
			readings <- *usage.RateLimits
		}
	}

	if err := app.probeClaudeRateLimits(context.Background()); !errors.Is(err, errIsolatedNetwork) {
		t.Fatalf("probe with no reading set = %v, want the isolated refusal", err)
	}

	resetsAt := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	if err := app.claudeUsageFake.Set(email, []claudefake.UsageLimit{
		{Kind: "session", Group: "session", Percent: 10, ResetsAt: resetsAt},
		{Kind: "weekly_all", Group: "weekly", Percent: 42, ResetsAt: resetsAt},
		{Kind: "weekly_scoped", Group: "weekly", Percent: 90, ResetsAt: resetsAt, Model: "Fable"},
	}); err != nil {
		t.Fatal(err)
	}
	// The harness RPC's path: a requested reading of the selected account.
	(&harnessHost{app: app}).RequestClaudeUsageReading()
	var snapshot provider.RateLimitsSnapshot
	select {
	case snapshot = <-readings:
	case <-time.After(10 * time.Second):
		t.Fatal("a requested reading never reached provider:usage")
	}
	var ids []string
	for _, limit := range snapshot.Limits {
		ids = append(ids, limit.LimitID)
	}
	slices.Sort(ids)
	if want := []string{"session", "weekly_all", "weekly_scoped:fable"}; !slices.Equal(ids, want) {
		t.Fatalf("limit ids = %v, want %v", ids, want)
	}
	cached := app.providerLifecycleService().Snapshots()
	if len(cached) != 1 || len(cached[0].Limits) != 3 {
		t.Fatalf("cached snapshots = %+v", cached)
	}

	app.claudeUsageFake.Clear(email)
	if err := app.probeClaudeRateLimits(context.Background()); !errors.Is(err, errIsolatedNetwork) {
		t.Fatalf("probe after clearing = %v, want the isolated refusal", err)
	}
}
