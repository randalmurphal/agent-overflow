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
	userHome    string
	store       *provideraccounts.Store
	credentials *provideraccounts.Credentials
	manager     *Manager
}

// openDataRoot opens the data root in dir against userHome, as a process
// starting there would.
func openDataRoot(t *testing.T, dir, userHome string) *dataRoot {
	t.Helper()
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
	return &dataRoot{dir: dir, userHome: userHome, store: store, credentials: credentials, manager: manager}
}

func newDataRoot(t *testing.T, userHome string) *dataRoot {
	t.Helper()
	return openDataRoot(t, t.TempDir(), userHome)
}

// boot runs the account steps of a boot in the app's order: the settle of
// unfinished sign-ins, then the home stamp and the ephemeral sweep.
func (r *dataRoot) boot(t *testing.T) error {
	t.Helper()
	settleErr := r.manager.SettleReservedSlots()
	if err := r.manager.ReconcileProviderHome(r.userHome); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return settleErr
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

// startUnsavedSignIn is a sign-in that crashed after writing its slot and
// before saving its account.
func (r *dataRoot) startUnsavedSignIn(t *testing.T, providerName string) string {
	t.Helper()
	reservation, err := r.manager.reserveAccount(providerName)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.credentials.WriteAccountCredential(providerName, reservation.ID(), []byte(`{"unsaved":true}`)); err != nil {
		t.Fatal(err)
	}
	return reservation.ID()
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

func slotPresent(t *testing.T, credentials *provideraccounts.Credentials, providerName, accountID string) bool {
	t.Helper()
	_, err := credentials.ReadCredential(providerName, accountID, false)
	if err != nil && !provideraccounts.IsCredentialMissing(err) {
		t.Fatal(err)
	}
	return err == nil
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
			ReservedSlots map[string]json.RawMessage `json:"reservedSlots"`
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

// Every data root on a machine shares the provider home and its saved
// slots. A root's boot must never delete a slot that another root's
// account list names, whichever root boots first; each boot is checked.
func TestBootKeepsTheSlotsOfEveryDataRootSharingTheHome(t *testing.T) {
	providers := []string{string(provider.Claude), string(provider.Codex)}
	for _, roots := range []int{2, 3} {
		for _, order := range permutations(roots) {
			t.Run(fmt.Sprintf("%d roots boot in order %v", roots, order), func(t *testing.T) {
				userHome := t.TempDir()
				var all []*dataRoot
				for i := 0; i < roots; i++ {
					root := newDataRoot(t, userHome)
					for _, providerName := range providers {
						root.saveAccount(t, providerName, fmt.Sprintf("root-%d-account", i))
					}
					all = append(all, root)
				}
				for _, booting := range order {
					if err := all[booting].boot(t); err != nil {
						t.Fatalf("boot of root %d: %v", booting, err)
					}
					for i, root := range all {
						for _, providerName := range providers {
							accountID := fmt.Sprintf("root-%d-account", i)
							if !slotPresent(t, root.credentials, providerName, accountID) {
								t.Fatalf("root %d's boot deleted root %d's saved %s login %s", booting, i, providerName, accountID)
							}
						}
					}
				}
			})
		}
	}
}

// The slot of a sign-in that never saved its account is removed by the root
// that started it and by no other, whichever roots boot around it; a
// reservation whose sign-in crashed before writing anything settles too.
func TestBootRemovesTheSlotOfASignInOnlyFromItsOwnRoot(t *testing.T) {
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
			if _, err := all[1].manager.reserveAccount(codexName); err != nil {
				t.Fatal(err)
			}
			ownerBooted := false
			for _, booting := range order {
				if err := all[booting].boot(t); err != nil {
					t.Fatalf("boot of root %d: %v", booting, err)
				}
				ownerBooted = ownerBooted || booting == 1
				if present := slotPresent(t, all[1].credentials, codexName, unsaved); present == ownerBooted {
					t.Fatalf("after root %d's boot the unsaved slot present = %v, want %v", booting, present, !ownerBooted)
				}
				for i, root := range all {
					if !slotPresent(t, root.credentials, codexName, fmt.Sprintf("root-%d-account", i)) {
						t.Fatalf("root %d's boot deleted root %d's saved login", booting, i)
					}
				}
			}
			if got := reservedIDs(t, all[1].dir, codexName); len(got) != 0 {
				t.Fatalf("reservations after boot = %v, want none", got)
			}
		})
	}
}

// A copy of a data root's metadata taken while one of its sign-ins was in
// flight carries that reservation. The original finishes the sign-in; the
// copy's boot must not delete the login it saved.
func TestACopiedDataRootLeavesTheOriginalsSignInAlone(t *testing.T) {
	codexName := string(provider.Codex)
	userHome := t.TempDir()
	original := newDataRoot(t, userHome)
	reservation, err := original.manager.reserveAccount(codexName)
	if err != nil {
		t.Fatal(err)
	}
	copyDir := t.TempDir()
	data, err := os.ReadFile(filepath.Join(original.dir, "provider-accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copyDir, "provider-accounts.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	original.saveAccount(t, codexName, reservation.ID())

	copied := openDataRoot(t, copyDir, userHome)
	if err := copied.boot(t); err != nil {
		t.Fatal(err)
	}
	if !slotPresent(t, original.credentials, codexName, reservation.ID()) {
		t.Fatal("the copy's boot deleted the login the original saved")
	}
	if got := reservedIDs(t, copyDir, codexName); !slices.Equal(got, []string{reservation.ID()}) {
		t.Fatalf("the copy's reservations = %v, want the original's left as they were", got)
	}
}

// A reservation is settled only under the home its slot was written in. A
// boot under another home neither deletes nor clears it; a boot back under
// its own home settles it.
func TestAReservationSettlesOnlyUnderItsOwnHome(t *testing.T) {
	codexName := string(provider.Codex)
	dir := t.TempDir()
	firstHome, secondHome := t.TempDir(), t.TempDir()
	underSecond := openDataRoot(t, dir, secondHome)
	unsaved := underSecond.startUnsavedSignIn(t, codexName)

	if err := openDataRoot(t, dir, firstHome).boot(t); err != nil {
		t.Fatal(err)
	}
	if !slotPresent(t, underSecond.credentials, codexName, unsaved) {
		t.Fatal("a boot under another home deleted the reserved slot")
	}
	if got := reservedIDs(t, dir, codexName); !slices.Equal(got, []string{unsaved}) {
		t.Fatalf("reservations after a boot under another home = %v, want %s kept", got, unsaved)
	}

	if err := openDataRoot(t, dir, secondHome).boot(t); err != nil {
		t.Fatal(err)
	}
	if slotPresent(t, underSecond.credentials, codexName, unsaved) {
		t.Fatal("a boot under the reservation's own home left its slot")
	}
	if got := reservedIDs(t, dir, codexName); len(got) != 0 {
		t.Fatalf("reservations after a boot under their home = %v, want none", got)
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
			root := openDataRoot(t, dir, userHome)
			ids := []string{"listed", "another-roots", "earlier-crash"}
			for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
				for _, id := range ids {
					if err := root.credentials.WriteAccountCredential(providerName, id, []byte(`{"id":"`+id+`"}`)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := root.boot(t); err != nil {
				t.Fatal(err)
			}
			for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
				for _, id := range ids {
					if !slotPresent(t, root.credentials, providerName, id) {
						t.Errorf("boot deleted %s slot %s", providerName, id)
					}
				}
			}
		})
	}
}

// A slot the boot cannot remove fails the settle naming it and keeps its
// reservation; the other provider still settles, and the next boot removes
// it.
func TestASlotTheBootCannotRemoveIsReportedAndTheNextBootRetries(t *testing.T) {
	claudeName, codexName := string(provider.Claude), string(provider.Codex)
	userHome := t.TempDir()
	root := newDataRoot(t, userHome)
	stuck, err := root.manager.reserveAccount(claudeName)
	if err != nil {
		t.Fatal(err)
	}
	unsaved := root.startUnsavedSignIn(t, codexName)
	// A regular file where the slot directory belongs: the slot can be
	// neither read nor removed.
	blocker := filepath.Join(userHome, ".claude", "agent-overflow-accounts", stuck.ID())
	if err := os.MkdirAll(filepath.Dir(blocker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocker, []byte("not a slot"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = root.boot(t)
	if err == nil || !strings.Contains(err.Error(), stuck.ID()) || !strings.Contains(err.Error(), "claude") {
		t.Fatalf("boot error = %v, want the claude slot %s named", err, stuck.ID())
	}
	if got := reservedIDs(t, root.dir, claudeName); !slices.Equal(got, []string{stuck.ID()}) {
		t.Fatalf("claude reservations after the failed boot = %v, want %s kept", got, stuck.ID())
	}
	if slotPresent(t, root.credentials, codexName, unsaved) || len(reservedIDs(t, root.dir, codexName)) != 0 {
		t.Fatal("a claude removal failure stopped the codex settle")
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := root.boot(t); err != nil {
		t.Fatal(err)
	}
	if got := reservedIDs(t, root.dir, claudeName); len(got) != 0 {
		t.Fatalf("claude reservations after the retry = %v, want none", got)
	}
}
