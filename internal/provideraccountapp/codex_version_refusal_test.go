package provideraccountapp

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

// A Switch or Remove on a Codex below the minimum is refused by the
// identity handshake of its reconciliation, and a refused change changes
// nothing: the selection, the generation and the running credential uses
// stay as they were. The canonical login differs from the recorded one, so
// the change must ask Codex who is signed in before it can go on.
func TestACodexBelowTheMinimumRefusesSwitchAndRemoveWithoutEndingUses(t *testing.T) {
	codexName := string(provider.Codex)
	for _, tc := range []struct {
		name   string
		change func(*Manager) error
	}{
		{"switch", func(m *Manager) error { _, err := m.SwitchProviderAccount(codexName, "acct-b"); return err }},
		{"remove the selected account", func(m *Manager) error { return m.RemoveProviderAccount(codexName, "acct-a") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, store, credentials, _, _, _ := codexOutsideLoginRig(t, "b@example.com")
			old := mockexec.Write(t, filepath.Join(t.TempDir(), "codex"), "#!/bin/bash\n"+
				"read -r _ || true\n"+
				`printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"userAgent":"agent_overflow/0.153.4 (Linux 6.6.0; x86_64) dumb (agent_overflow; 0.1.0)"}}'`+"\n"+
				"read -r _ || true\n")
			manager.deps.ProviderBinary = func(string) string { return old }
			outside := codexWorkspaceLogin(t, "b@example.com", "ws-b", "rt-b1", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
			if err := credentials.WriteNativeCredentialForTest(codexName, outside); err != nil {
				t.Fatal(err)
			}
			// The use exits when it is ended, as a CLI does, so a change that
			// ends it goes on and this test reads what it did.
			ended := make(chan struct{})
			var release func()
			release, err := manager.BeginCredentialUse(t.Context(), codexName, manager.Generation(codexName), func() {
				close(ended)
				go release()
			})
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			before := manager.Generation(codexName)

			err = tc.change(manager)
			if !errors.Is(err, provider.ErrCodexVersionUnsupported) {
				t.Fatalf("change = %v, want the version refusal", err)
			}
			select {
			case <-ended:
				t.Fatal("the refused change ended a running credential use")
			default:
			}
			if after := manager.Generation(codexName); after != before {
				t.Fatalf("generation = %d, want %d unchanged", after, before)
			}
			if active, ok := store.Active(codexName, time.Now()); !ok || active.ID != "acct-a" {
				t.Fatalf("selected = %+v, %v; want acct-a kept", active, ok)
			}
			if accounts := store.List(codexName, time.Now()); len(accounts) != 2 {
				t.Fatalf("saved accounts = %d, want both kept", len(accounts))
			}
		})
	}
}
