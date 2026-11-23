package provideraccounts

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// reservedOnDisk is what a later process would load as providerName's
// reservations.
func reservedOnDisk(t *testing.T, dir, providerName string) []string {
	t.Helper()
	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.mu.RLock()
	defer reloaded.mu.RUnlock()
	ids := make([]string, 0)
	for id := range reloaded.state.Providers[providerName].ReservedSlots {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// A reservation is durable before its id is handed out, and inserting the
// account's row, by either insert, ends it in the same write.
func TestAReservationIsDurableUntilItsRowIsInserted(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	added, err := store.ReserveAccountID("claude")
	if err != nil {
		t.Fatal(err)
	}
	selected, err := store.ReserveAccountID("claude")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{added, selected}
	slices.Sort(want)
	if got := reservedOnDisk(t, dir, "claude"); !slices.Equal(got, want) {
		t.Fatalf("reserved on disk = %v, want %v", got, want)
	}
	if _, err := store.Add(Account{ID: added, Provider: "claude", Email: "added@example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := reservedOnDisk(t, dir, "claude"); !slices.Equal(got, []string{selected}) {
		t.Fatalf("reserved after Add = %v, want only %s", got, selected)
	}
	if _, err := store.UpsertAndActivate(Account{ID: selected, Provider: "claude", Email: "selected@example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := reservedOnDisk(t, dir, "claude"); len(got) != 0 {
		t.Fatalf("reserved after UpsertAndActivate = %v, want none", got)
	}
}

// A reservation that cannot be saved hands out no id, so no slot can be
// written for one that a crash would leave unowned.
func TestAReservationThatCannotBeSavedHandsOutNoID(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	id, err := store.ReserveAccountID("codex")
	if err == nil || id != "" {
		t.Fatalf("ReserveAccountID = (%q, %v), want no id and an error", id, err)
	}
	store.mu.RLock()
	inMemory := len(store.state.Providers["codex"].ReservedSlots)
	store.mu.RUnlock()
	if inMemory != 0 {
		t.Fatalf("a failed reservation stayed in memory (%d)", inMemory)
	}
}

// writeMetadata is a provider-accounts.json as some process left it.
func writeMetadata(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, stateFilename), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An id that is both reserved and saved names a live login. Settling it
// clears the reservation and never deletes the slot.
func TestSettlingAReservationForASavedAccountNeverDeletesItsSlot(t *testing.T) {
	dir := t.TempDir()
	writeMetadata(t, dir, `{"version":1,"providers":{"claude":{"accounts":[{"id":"live","provider":"claude","addedAt":1}],`+
		`"reservedSlots":{"live":true,"unsaved":true}}}}`)
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var removed []string
	release, err := store.ReleaseReservedSlots("claude", func(accountID string) error {
		removed = append(removed, accountID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{"unsaved"}) {
		t.Fatalf("removed slots = %v, want only unsaved", removed)
	}
	if !slices.Equal(release.Listed, []string{"live"}) || !slices.Equal(release.Removed, []string{"unsaved"}) {
		t.Fatalf("release = %+v, want live listed and unsaved removed", release)
	}
	if got := reservedOnDisk(t, dir, "claude"); len(got) != 0 {
		t.Fatalf("reserved after settling = %v, want none", got)
	}
}

// A slot that cannot be removed keeps its reservation for the next boot,
// which retries it; the others settle.
func TestAReservationWhoseSlotCannotBeRemovedStaysForTheNextBoot(t *testing.T) {
	dir := t.TempDir()
	writeMetadata(t, dir, `{"version":1,"providers":{"codex":{"accounts":[],"reservedSlots":{"stuck":true,"gone":true}}}}`)
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused")
	release, err := store.ReleaseReservedSlots("codex", func(accountID string) error {
		if accountID == "stuck" {
			return refused
		}
		return nil
	})
	if !errors.Is(err, refused) {
		t.Fatalf("error = %v, want the removal's failure", err)
	}
	if !slices.Equal(release.Removed, []string{"gone"}) {
		t.Fatalf("removed = %v, want only gone", release.Removed)
	}
	if got := reservedOnDisk(t, dir, "codex"); !slices.Equal(got, []string{"stuck"}) {
		t.Fatalf("reserved after a failed removal = %v, want stuck kept", got)
	}
	if _, err := store.ReleaseReservedSlots("codex", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := reservedOnDisk(t, dir, "codex"); len(got) != 0 {
		t.Fatalf("reserved after the retry = %v, want none", got)
	}
}
