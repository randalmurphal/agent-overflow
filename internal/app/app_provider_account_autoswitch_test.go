package app

import (
	"testing"

	"agent-overflow/internal/provider"
)

func TestSetProviderAccountAutoSwitchRoundTripsThroughTheListing(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	installProviderUsageTestAccounts(t, app, string(provider.Codex),
		usageTestAccount{"other", []byte(`{"tokens":{"access_token":"other"}}`)},
		usageTestAccount{"selected", []byte(`{"tokens":{"access_token":"selected"}}`)},
	)

	inPool := func() map[string]bool {
		t.Helper()
		listed, err := app.ListProviderAccounts()
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, account := range listed {
			out[account.ID] = account.AutoSwitch
		}
		return out
	}

	if got := inPool(); !got["other"] || !got["selected"] {
		t.Fatalf("pool before any change = %v, want every saved account", got)
	}
	if err := app.SetProviderAccountAutoSwitch(string(provider.Codex), "other", false); err != nil {
		t.Fatal(err)
	}
	if got := inPool(); got["other"] || !got["selected"] {
		t.Fatalf("pool after taking other out = %v", got)
	}
	if err := app.SetProviderAccountAutoSwitch(string(provider.Codex), "other", true); err != nil {
		t.Fatal(err)
	}
	if got := inPool(); !got["other"] {
		t.Fatalf("pool after putting other back = %v", got)
	}
	if err := app.SetProviderAccountAutoSwitch(string(provider.Codex), "missing", false); err == nil {
		t.Fatal("an unsaved account was taken out of the pool")
	}
}
