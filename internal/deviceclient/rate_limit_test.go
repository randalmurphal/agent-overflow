//go:build !noremote

package deviceclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/servercert"
)

// The auth rate limit answers a redemption in plain text before any
// credential is judged. A person pairing a computer is told to wait, for the
// time the backend named, and never that the pairing was refused.
func TestPairTellsAPersonToWaitWhenTheBackendLimitsPairing(t *testing.T) {
	t.Parallel()
	for retryAfter, want := range map[string]string{
		"30":  "this computer is limiting pairing attempts; wait 30 seconds, then try again",
		"120": "this computer is limiting pairing attempts; wait 2 minutes, then try again",
		"":    "this computer is limiting pairing attempts; wait a moment, then try again",
	} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			http.Error(w, "too many requests", http.StatusTooManyRequests)
		}))
		link := Link{BackendID: "backend-a", Endpoint: server.URL, CertFingerprint: servercert.Fingerprint(server.Certificate().Raw), Token: "test-token"}
		_, _, err := Pair(context.Background(), t.TempDir(), link, "test", "test")
		server.Close()
		var limited *RateLimited
		if !errors.As(err, &limited) {
			t.Fatalf("Retry-After %q: Pair = %v, want a RateLimited answer", retryAfter, err)
		}
		var refused *Refusal
		if errors.As(err, &refused) {
			t.Fatalf("Retry-After %q: the rate limit read as a refusal: %v", retryAfter, err)
		}
		if _, shown, ok := errorsx.PublicDetails(err); !ok || shown != want || err.Error() != want {
			t.Fatalf("Retry-After %q: a person reads %q (public %v, error %q), want %q", retryAfter, shown, ok, err, want)
		}
	}
}
