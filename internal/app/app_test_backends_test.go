package app

import (
	"testing"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/testutil"
)

// newTestBackends is attachedbackends.New for a test. Each credential
// exchange gets the shared test budget (testutil.WaitBudget) in place of the
// product's 10 s: the backend it pairs with runs in this test process, where
// a crowded machine can hold a pairing past 10 s. The bound itself is
// deviceclient's to test.
func newTestBackends(t *testing.T, dir, label, platform string) (*attachedbackends.Manager, error) {
	t.Helper()
	manager, err := attachedbackends.New(dir, label, platform)
	if err != nil {
		return nil, err
	}
	manager.SetExchangeTimeout(testutil.WaitBudget(t))
	return manager, nil
}
