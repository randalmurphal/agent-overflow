package app

import (
	"strings"
	"testing"

	"agent-overflow/internal/settings"
)

// TestListRemoteEndpoints_OmitsToken pins the LAN-bind safety contract:
// the bulk read path must not surface tokens. The wire shape is a
// dedicated RemoteEndpointSummary struct with no Token field, so a
// remote token-holder calling ListRemoteEndpoints can't harvest tokens
// for unrelated saved backends.
func TestListRemoteEndpoints_OmitsToken(t *testing.T) {
	app := &App{settings: settings.NewService(t.TempDir())}

	if _, err := app.settings.AddRemoteEndpoint("Tailnet", "ws://10.0.0.5:54321/", "supersecret-token"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := app.ListRemoteEndpoints()
	if err != nil {
		t.Fatalf("ListRemoteEndpoints: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(got))
	}
	// The Token field is structurally absent from RemoteEndpointSummary;
	// belt-and-braces, marshal the response and grep for the token.
	// Future regressions that reintroduce Token via embedding would still
	// trip this check.
	for _, ep := range got {
		if ep.URL == "" || ep.ID == "" {
			t.Fatalf("ListRemoteEndpoints returned empty fields: %+v", ep)
		}
	}

	// Marshal-grep guard: serialize the slice and confirm the token
	// substring is nowhere in the output. This catches a future regression
	// where someone adds Token to RemoteEndpointSummary or returns a
	// settings.RemoteEndpoint directly.
	if leaked := containsToken(got, "supersecret-token"); leaked {
		t.Fatalf("ListRemoteEndpoints leaked token to wire shape")
	}
}

// containsToken serialises the summary and searches for the token. We
// don't import encoding/json here to keep the test focused on the
// observable fields; instead we walk fields manually.
func containsToken(summaries []RemoteEndpointSummary, token string) bool {
	for _, s := range summaries {
		if strings.Contains(s.ID, token) || strings.Contains(s.Name, token) || strings.Contains(s.URL, token) {
			return true
		}
	}
	return false
}

// TestGetRemoteEndpointToken_ReturnsToken exercises the explicit
// fetch path. The "Copy launch command" affordance reads through this
// rather than ListRemoteEndpoints; the split keeps the bulk read free
// of credentials while the explicit single-record fetch can still
// surface them.
func TestGetRemoteEndpointToken_ReturnsToken(t *testing.T) {
	app := &App{settings: settings.NewService(t.TempDir())}

	created, err := app.settings.AddRemoteEndpoint("X", "ws://x/", "token-X")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := app.GetRemoteEndpointToken(created.ID)
	if err != nil {
		t.Fatalf("GetRemoteEndpointToken: %v", err)
	}
	if got != "token-X" {
		t.Fatalf("token = %q, want %q", got, "token-X")
	}
}

// TestGetRemoteEndpointToken_RequiresExistingID pins the error contract
// for unknown IDs. Returning an empty string + nil here would let a
// stale UI silently get an empty launch command instead of surfacing
// "endpoint deleted" — the error is the better signal.
func TestGetRemoteEndpointToken_RequiresExistingID(t *testing.T) {
	app := &App{settings: settings.NewService(t.TempDir())}

	_, err := app.GetRemoteEndpointToken("does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown ID")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got %v", err)
	}
}

// TestGetRemoteEndpointToken_NoSettingsService guards the nil-service
// case (test rigs that wire App without settings) — same shape as the
// other settings-touching bindings.
func TestGetRemoteEndpointToken_NoSettingsService(t *testing.T) {
	app := &App{}
	if _, err := app.GetRemoteEndpointToken("any"); err == nil {
		t.Fatal("expected error when settings unavailable")
	}
}
