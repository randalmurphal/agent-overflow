package provideraccountapp

import (
	"context"
	"testing"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

type recordingAccountSink struct {
	events  []string
	signIns []SignIn
	// changed is the provider of each PublishAccountsChanged.
	changed []string
	// listings is the provider of each PublishListingChanged.
	listings []string
	// limitsReset is the account of each PublishLimitsReset.
	limitsReset []string
	// onListing, when set, runs at each PublishListingChanged.
	onListing func()
}

func (s *recordingAccountSink) PublishAccount(string, provideraccounts.Account, provider.AccountInfo, uint64) {
	s.events = append(s.events, "account")
}
func (s *recordingAccountSink) PublishCleared(string, uint64) { s.events = append(s.events, "cleared") }
func (s *recordingAccountSink) PublishAccountsChanged(providerName string) {
	s.events = append(s.events, "changed")
	s.changed = append(s.changed, providerName)
}
func (s *recordingAccountSink) PublishListingChanged(providerName string) {
	s.events = append(s.events, "listing")
	s.listings = append(s.listings, providerName)
	if s.onListing != nil {
		s.onListing()
	}
}
func (s *recordingAccountSink) PublishLimitsReset(_, accountID string) {
	s.events = append(s.events, "limits-reset")
	s.limitsReset = append(s.limitsReset, accountID)
}
func (s *recordingAccountSink) PublishUsageError(string, string, error) {
	s.events = append(s.events, "usage-error")
}
func (s *recordingAccountSink) PublishLogin(LoginState) { s.events = append(s.events, "login") }
func (s *recordingAccountSink) PublishSignIn(signIn SignIn) {
	s.events = append(s.events, "signin")
	s.signIns = append(s.signIns, signIn)
}

// A client refreshes the model catalog on `provider:account`. AfterAdopt is
// where Claude commits that catalog, so it must land before the emit.
func TestRunAccountProbeRunsAfterAdoptBeforeTheAccountEmit(t *testing.T) {
	sink := &recordingAccountSink{}
	manager := NewManager(Deps{Reaper: procutiltest.Reaper(), Accounts: sink})

	_, err := manager.RunAccountProbe(ProbeRequest{
		ProviderName: string(provider.Claude),
		Probe: func(context.Context) (provider.AccountInfo, error) {
			return provider.AccountInfo{SubscriptionType: "max"}, nil
		},
		AfterAdopt: func(provideraccounts.Account) { sink.events = append(sink.events, "after-adopt") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 2 || sink.events[0] != "after-adopt" || sink.events[1] != "account" {
		t.Fatalf("events = %v, want [after-adopt account]", sink.events)
	}
}
