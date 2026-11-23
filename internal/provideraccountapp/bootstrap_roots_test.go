package provideraccountapp

import (
	"fmt"
	"testing"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

// dataRoot is one data directory's account store and manager over a
// provider home that other data roots share.
type dataRoot struct {
	dir         string
	store       *provideraccounts.Store
	credentials *provideraccounts.Credentials
	manager     *Manager
}

func newDataRoot(t *testing.T, userHome string) *dataRoot {
	t.Helper()
	dir := t.TempDir()
	store, err := provideraccounts.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := provideraccounts.NewCredentialsWithFileKeychain(userHome, CredentialPolicy())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(Deps{Reaper: procutiltest.Reaper()})
	if err := manager.Attach(store, credentials, ""); err != nil {
		t.Fatal(err)
	}
	return &dataRoot{dir: dir, store: store, credentials: credentials, manager: manager}
}

// saveAccount saves an account the way a sign-in does: the slot first,
// then the row.
func (r *dataRoot) saveAccount(t *testing.T, providerName, accountID string) {
	t.Helper()
	if err := r.credentials.WriteAccountCredential(providerName, accountID, []byte(`{"account":"`+accountID+`"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.Add(provideraccounts.Account{ID: accountID, Provider: providerName}); err != nil {
		t.Fatal(err)
	}
}

func permutations(n int) [][]int {
	if n == 1 {
		return [][]int{{0}}
	}
	var out [][]int
	for _, rest := range permutations(n - 1) {
		for at := 0; at <= len(rest); at++ {
			order := append(append(append([]int{}, rest[:at]...), n-1), rest[at:]...)
			out = append(out, order)
		}
	}
	return out
}

// Every data root on a machine shares the provider home and its saved
// slots. A root's boot must never delete a slot that another root's
// account list names, whichever root boots first.
func TestBootKeepsTheSlotsOfEveryDataRootSharingTheHome(t *testing.T) {
	for _, roots := range []int{2, 3} {
		for _, order := range permutations(roots) {
			t.Run(fmt.Sprintf("%d roots boot in order %v", roots, order), func(t *testing.T) {
				userHome := t.TempDir()
				var all []*dataRoot
				for i := 0; i < roots; i++ {
					root := newDataRoot(t, userHome)
					for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
						root.saveAccount(t, providerName, fmt.Sprintf("root-%d-account", i))
					}
					all = append(all, root)
				}
				for _, i := range order {
					if err := all[i].manager.ReconcileProviderHome(all[i].dir, userHome); err != nil {
						t.Fatalf("boot of root %d: %v", i, err)
					}
				}
				for i, root := range all {
					for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
						accountID := fmt.Sprintf("root-%d-account", i)
						if _, err := root.credentials.ReadCredential(providerName, accountID, false); err != nil {
							t.Errorf("root %d's saved %s login %s is gone after the boots: %v", i, providerName, accountID, err)
						}
					}
				}
			})
		}
	}
}
