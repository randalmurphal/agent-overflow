package app

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// bearerRecorder records the bearer of every request and answers none.
type bearerRecorder struct {
	mu      sync.Mutex
	bearers []string
}

func (r *bearerRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.bearers = append(r.bearers, req.Header.Get("Authorization"))
	r.mu.Unlock()
	return nil, errors.New("usage endpoint not served in this test")
}

func (r *bearerRecorder) saw(bearer string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, got := range r.bearers {
		if got == bearer {
			return true
		}
	}
	return false
}

// A switch a person makes is followed by a usage read of the account it
// selected, so the meters and auto-switch do not hold that account's last
// reading until the next poll.
func TestSwitchProviderAccountReadsTheNewAccountsUsage(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	live := time.Now().Add(time.Hour)
	installProviderUsageTestAccounts(t, app, string(provider.Claude),
		usageTestAccount{"first", claudeCredentialExpiringAt("first-token", live)},
		usageTestAccount{"second", claudeCredentialExpiringAt("second-token", live)},
	)
	app.switchedAccountProbeFn = func() error { return nil }
	recorder := &bearerRecorder{}
	app.rateLimitProbeClientOverride = &http.Client{Transport: recorder}

	if _, err := app.SwitchProviderAccount(string(provider.Claude), "first"); err != nil {
		t.Fatalf("SwitchProviderAccount() error = %v", err)
	}
	waitForCondition(t, "a usage read of the newly selected account", func() bool {
		return recorder.saw("Bearer first-token")
	})
}
