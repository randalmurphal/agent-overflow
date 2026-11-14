package claudefake

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"agent-overflow/internal/provider/claude"
)

var errRefused = errors.New("refused")

type usageTransport struct{ usage *Usage }

func (t usageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if resp, ok, err := t.usage.Serve(req); ok {
		return resp, err
	}
	return nil, errRefused
}

func probeAs(t *testing.T, usage *Usage, email string) ([]string, error) {
	t.Helper()
	credential := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q}}`, AccessToken(email, "org", "1"))
	snapshot, err := claude.ProbeRateLimitsFromCredentialData(
		context.Background(), &http.Client{Transport: usageTransport{usage}}, []byte(credential),
	)
	if err != nil {
		return nil, err
	}
	if !snapshot.Complete {
		t.Errorf("a whole fake answer read as partial: %+v", snapshot)
	}
	ids := make([]string, 0, len(snapshot.Limits))
	for _, limit := range snapshot.Limits {
		ids = append(ids, limit.LimitID)
	}
	slices.Sort(ids)
	return ids, nil
}

func TestUsageAnswersOnlyTheIdentityItHolds(t *testing.T) {
	t.Parallel()
	usage := NewUsage()
	if _, err := probeAs(t, usage, "a@example.test"); !errors.Is(err, errRefused) {
		t.Fatalf("probe with nothing set = %v, want refused", err)
	}
	if err := usage.Set("a@example.test", []UsageLimit{
		{Kind: "session", Group: "session", Percent: 12, ResetsAt: "2026-11-13T18:00:00Z"},
		{Kind: "weekly_all", Group: "weekly", Percent: 40, ResetsAt: "2026-11-18T18:00:00Z"},
		{Kind: "weekly_scoped", Group: "weekly", Percent: 90, ResetsAt: "2026-11-18T18:00:00Z", Model: "Fable"},
	}); err != nil {
		t.Fatal(err)
	}
	ids, err := probeAs(t, usage, "a@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"session", "weekly_all", "weekly_scoped:fable"}; !slices.Equal(ids, want) {
		t.Fatalf("limit ids = %v, want %v", ids, want)
	}
	if _, err := probeAs(t, usage, "b@example.test"); !errors.Is(err, errRefused) {
		t.Fatalf("probe for another identity = %v, want refused", err)
	}
	usage.Clear("a@example.test")
	if _, err := probeAs(t, usage, "a@example.test"); !errors.Is(err, errRefused) {
		t.Fatalf("probe after clear = %v, want refused", err)
	}
	var none *Usage
	if _, ok, _ := none.Serve(&http.Request{}); ok {
		t.Fatal("a nil Usage answered")
	}
}

func TestUsageSetRejectsAnswersTheEndpointCannotGive(t *testing.T) {
	t.Parallel()
	usage := NewUsage()
	for name, limits := range map[string][]UsageLimit{
		"no limits": nil,
		"no kind":   {{Group: "weekly", Percent: 1}},
		"bad reset": {{Kind: "weekly_all", Group: "weekly", ResetsAt: "tomorrow"}},
	} {
		if err := usage.Set("a@example.test", limits); err == nil {
			t.Errorf("%s: Set accepted %+v", name, limits)
		}
	}
	if err := usage.Set("", []UsageLimit{{Kind: "session"}}); err == nil {
		t.Error("Set accepted an empty email")
	}
}
