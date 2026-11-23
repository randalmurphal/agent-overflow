package provideraccountapp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// reservedIDs is what root's metadata file holds as providerName's
// reservations.
func reservedIDs(t *testing.T, dir, providerName string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "provider-accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Providers map[string]struct {
			ReservedSlots map[string]bool `json:"reservedSlots"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0)
	for id := range state.Providers[providerName].ReservedSlots {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// startUnsavedSignIn is a sign-in that crashed after writing its slot and
// before saving its account.
func (r *dataRoot) startUnsavedSignIn(t *testing.T, providerName string) string {
	t.Helper()
	accountID, err := r.store.ReserveAccountID(providerName)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.credentials.WriteAccountCredential(providerName, accountID, []byte(`{"unsaved":true}`)); err != nil {
		t.Fatal(err)
	}
	return accountID
}

func slotPresent(t *testing.T, credentials *provideraccounts.Credentials, providerName, accountID string) bool {
	t.Helper()
	_, err := credentials.ReadCredential(providerName, accountID, false)
	if err != nil && !provideraccounts.IsCredentialMissing(err) {
		t.Fatal(err)
	}
	return err == nil
}

// The slot of a sign-in that never saved its account is removed by the root
// that started it, at its next boot, whichever roots boot around it; a
// reservation whose sign-in crashed before writing anything settles too.
func TestBootRemovesTheSlotOfASignInItsRootNeverSaved(t *testing.T) {
	codexName := string(provider.Codex)
	for _, order := range permutations(3) {
		t.Run(fmt.Sprintf("boot order %v", order), func(t *testing.T) {
			userHome := t.TempDir()
			var all []*dataRoot
			for i := 0; i < 3; i++ {
				root := newDataRoot(t, userHome)
				root.saveAccount(t, codexName, fmt.Sprintf("root-%d-account", i))
				all = append(all, root)
			}
			unsaved := all[1].startUnsavedSignIn(t, codexName)
			if _, err := all[1].store.ReserveAccountID(codexName); err != nil {
				t.Fatal(err)
			}
			for _, i := range order {
				if err := all[i].manager.ReconcileProviderHome(all[i].dir, userHome); err != nil {
					t.Fatalf("boot of root %d: %v", i, err)
				}
			}
			if slotPresent(t, all[1].credentials, codexName, unsaved) {
				t.Fatal("the unsaved sign-in's slot survived its root's boot")
			}
			if got := reservedIDs(t, all[1].dir, codexName); len(got) != 0 {
				t.Fatalf("reservations after boot = %v, want none", got)
			}
			for i, root := range all {
				if !slotPresent(t, root.credentials, codexName, fmt.Sprintf("root-%d-account", i)) {
					t.Fatalf("root %d's saved login is gone", i)
				}
			}
		})
	}
}

// A store bound to another provider home settles nothing in this one: it
// neither deletes the slot a reservation names nor forgets the reservation.
func TestBootSettlesNoReservationInAHomeTheStoreIsNotBoundTo(t *testing.T) {
	codexName := string(provider.Codex)
	userHome := t.TempDir()
	root := newDataRoot(t, userHome)
	if _, _, err := root.store.ClaimProviderHome(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	unsaved := root.startUnsavedSignIn(t, codexName)
	if err := root.manager.ReconcileProviderHome(root.dir, userHome); err != nil {
		t.Fatal(err)
	}
	if !slotPresent(t, root.credentials, codexName, unsaved) {
		t.Fatal("a boot against a home the store is not bound to deleted a reserved slot")
	}
	if got := reservedIDs(t, root.dir, codexName); !slices.Equal(got, []string{unsaved}) {
		t.Fatalf("reservations = %v, want %s kept", got, unsaved)
	}
}

// A store that reserved nothing deletes nothing, whatever the home holds and
// whatever it lists: an empty store (a fresh --data-dir), and one an earlier
// release wrote, which carries no reservations and names one of the slots.
func TestBootOfAStoreThatReservedNothingDeletesNoSlot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata string
	}{
		{name: "empty store"},
		{name: "store an earlier release wrote", metadata: `{"version":1,"providerHome":"%s","providers":{` +
			`"claude":{"activeAccountId":"listed","generation":1,"accounts":[{"id":"listed","provider":"claude","addedAt":1}]},` +
			`"codex":{"generation":0,"accounts":[{"id":"listed","provider":"codex","addedAt":1}]}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userHome := t.TempDir()
			dir := t.TempDir()
			if tc.metadata != "" {
				body := fmt.Sprintf(tc.metadata, filepath.Clean(userHome))
				if err := os.WriteFile(filepath.Join(dir, "provider-accounts.json"), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
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
			ids := []string{"listed", "another-roots", "earlier-crash"}
			for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
				for _, id := range ids {
					if err := credentials.WriteAccountCredential(providerName, id, []byte(`{"id":"`+id+`"}`)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := manager.ReconcileProviderHome(dir, userHome); err != nil {
				t.Fatal(err)
			}
			for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
				for _, id := range ids {
					if !slotPresent(t, credentials, providerName, id) {
						t.Errorf("boot deleted %s slot %s", providerName, id)
					}
				}
			}
		})
	}
}

// A boot that cannot remove an unsaved slot fails naming it, keeps the
// reservation, and the next boot removes it.
func TestABootThatCannotRemoveAnUnsavedSlotFailsAndTheNextRetries(t *testing.T) {
	codexName := string(provider.Codex)
	userHome := t.TempDir()
	root := newDataRoot(t, userHome)
	unsaved, err := root.store.ReserveAccountID(codexName)
	if err != nil {
		t.Fatal(err)
	}
	// A regular file where the slot directory belongs: the slot can be
	// neither read nor removed.
	blocker := filepath.Join(userHome, ".codex", "agent-overflow-accounts", unsaved)
	if err := os.MkdirAll(filepath.Dir(blocker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocker, []byte("not a slot"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = root.manager.ReconcileProviderHome(root.dir, userHome)
	if err == nil || !strings.Contains(err.Error(), unsaved) {
		t.Fatalf("boot error = %v, want the unremovable slot %s named", err, unsaved)
	}
	if got := reservedIDs(t, root.dir, codexName); !slices.Equal(got, []string{unsaved}) {
		t.Fatalf("reservations after the failed boot = %v, want %s kept", got, unsaved)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := root.credentials.WriteAccountCredential(codexName, unsaved, []byte(`{"unsaved":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := root.manager.ReconcileProviderHome(root.dir, userHome); err != nil {
		t.Fatal(err)
	}
	if slotPresent(t, root.credentials, codexName, unsaved) {
		t.Fatal("the retrying boot left the unsaved slot")
	}
	if got := reservedIDs(t, root.dir, codexName); len(got) != 0 {
		t.Fatalf("reservations after the retry = %v, want none", got)
	}
}
