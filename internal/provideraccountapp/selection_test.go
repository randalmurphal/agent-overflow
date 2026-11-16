package provideraccountapp

import (
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// recordingGateway records each selection change the manager reports.
type recordingGateway struct {
	moves []selectionChange
}

type selectionChange struct {
	provider, from, to string
}

func (g *recordingGateway) ApplySelection(providerName, previousAccountID string, selection Selection) {
	g.moves = append(g.moves, selectionChange{providerName, previousAccountID, selection.AccountID})
}

// selectionRig has "current" selected and "other" saved, with the canonical
// fingerprint remembered, and a recording gateway.
func selectionRig(t *testing.T) (*Manager, *recordingGateway, *recordingAccountSink) {
	t.Helper()
	manager, store, credentials, sink := signInManager(t)
	soon := time.Now().Add(20 * 24 * time.Hour)
	seedClaudeAccounts(t, manager, store, credentials,
		claudeLoginAs("current@example.com", "current", soon),
		"other", claudeLoginAs("other@example.com", "other", soon))
	gateway := &recordingGateway{}
	manager.deps.Sessions = gateway
	return manager, gateway, sink
}

func wantMoves(t *testing.T, gateway *recordingGateway, want ...selectionChange) {
	t.Helper()
	if !slices.Equal(gateway.moves, want) {
		t.Fatalf("selection changes = %+v, want %+v", gateway.moves, want)
	}
}

func wantChangedFor(t *testing.T, sink *recordingAccountSink, providerName string) {
	t.Helper()
	if len(sink.changed) == 0 {
		t.Fatal("no listing nudge")
	}
	for _, got := range sink.changed {
		if got != providerName {
			t.Fatalf("listing nudges = %v, want only %s", sink.changed, providerName)
		}
	}
}

// Auto-switch charges a refusal to the account a running turn sent on, so
// every route that moves the selection says where it moved from.
func TestEverySelectionChangeReportsTheAccountItLeft(t *testing.T) {
	claudeName := string(provider.Claude)
	externalLogin := func(t *testing.T, manager *Manager) {
		t.Helper()
		if err := manager.credentials.WriteNativeCredentialForTest(
			claudeName,
			claudeLoginAs("other@example.com", "external", time.Now().Add(30*24*time.Hour)),
		); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("switch", func(t *testing.T) {
		manager, gateway, sink := selectionRig(t)
		if _, err := manager.SwitchProviderAccount(claudeName, "other"); err != nil {
			t.Fatal(err)
		}
		wantMoves(t, gateway, selectionChange{claudeName, "current", "other"})
		wantChangedFor(t, sink, claudeName)
	})

	t.Run("removing the selected account", func(t *testing.T) {
		manager, gateway, sink := selectionRig(t)
		if err := manager.RemoveProviderAccount(claudeName, "current"); err != nil {
			t.Fatal(err)
		}
		if err := manager.RemoveProviderAccount(claudeName, "other"); err != nil {
			t.Fatal(err)
		}
		wantMoves(t, gateway,
			selectionChange{claudeName, "current", "other"},
			selectionChange{claudeName, "other", ""},
		)
		wantChangedFor(t, sink, claudeName)
	})

	t.Run("an outside login", func(t *testing.T) {
		manager, gateway, sink := selectionRig(t)
		externalLogin(t, manager)
		if err := manager.ReconcileExternalProviderAccount(claudeName); err != nil {
			t.Fatal(err)
		}
		wantMoves(t, gateway, selectionChange{claudeName, "current", "other"})
		wantChangedFor(t, sink, claudeName)
	})

	t.Run("a switch that first finds an outside login", func(t *testing.T) {
		manager, gateway, _ := selectionRig(t)
		externalLogin(t, manager)
		if _, err := manager.SwitchProviderAccount(claudeName, "current"); err != nil {
			t.Fatal(err)
		}
		wantMoves(t, gateway,
			selectionChange{claudeName, "current", "other"},
			selectionChange{claudeName, "other", "current"},
		)
	})

	t.Run("a probe that finds an outside login", func(t *testing.T) {
		manager, gateway, sink := selectionRig(t)
		externalLogin(t, manager)
		if _, err := manager.RunAccountProbe(ProbeRequest{
			ProviderName: claudeName,
			Probe:        manager.canonicalProviderAccountProbe(claudeName, manager.providerBinaryPath(claudeName)),
		}); err != nil {
			t.Fatal(err)
		}
		wantMoves(t, gateway, selectionChange{claudeName, "current", "other"})
		wantChangedFor(t, sink, claudeName)
	})

	t.Run("a probe of the selected login", func(t *testing.T) {
		manager, gateway, _ := selectionRig(t)
		if err := manager.credentials.WriteNativeCredentialForTest(
			claudeName,
			claudeLoginAs("current@example.com", "rotated", time.Now().Add(30*24*time.Hour)),
		); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.RunAccountProbe(ProbeRequest{
			ProviderName: claudeName,
			Probe:        manager.canonicalProviderAccountProbe(claudeName, manager.providerBinaryPath(claudeName)),
		}); err != nil {
			t.Fatal(err)
		}
		wantMoves(t, gateway)
	})
}

// The listing nudge names the provider whose set moved, so auto-switch
// forgets only that provider's sign-in results.
func TestTheListingNudgeNamesItsProvider(t *testing.T) {
	manager, _, _ := newTestManager(t)
	sink := &recordingAccountSink{}
	manager.deps.Accounts = sink
	codexName := string(provider.Codex)
	if _, err := manager.store.UpsertAndActivate(AccountFromInfo("codex-a", codexName, provider.AccountInfo{Email: "a@example.com"})); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetProviderAccountAutoSwitch(codexName, "codex-a", true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sink.changed, []string{codexName}) {
		t.Fatalf("listing nudges = %v, want [%s]", sink.changed, codexName)
	}
}
