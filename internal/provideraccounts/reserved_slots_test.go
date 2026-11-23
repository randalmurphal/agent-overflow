package provideraccounts

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// reservedOnDisk is what a later process would load as providerName's
// reservations.
func reservedOnDisk(t *testing.T, dir, providerName string) map[string]ReservedSlot {
	t.Helper()
	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.mu.RLock()
	defer reloaded.mu.RUnlock()
	reserved := make(map[string]ReservedSlot)
	for id, slot := range reloaded.state.Providers[providerName].ReservedSlots {
		reserved[id] = slot
	}
	return reserved
}

func reservedIDs(reserved map[string]ReservedSlot) []string {
	ids := make([]string, 0, len(reserved))
	for id := range reserved {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// A reservation is durable, naming this data root and the provider home,
// before its id is handed out, and inserting the account's row, by either
// insert, ends it in the same write.
func TestAReservationIsDurableUntilItsRowIsInserted(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	added, err := store.ReserveAccountID("claude", home)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := store.ReserveAccountID("claude", home)
	if err != nil {
		t.Fatal(err)
	}
	reserved := reservedOnDisk(t, dir, "claude")
	want := []string{added, selected}
	slices.Sort(want)
	if got := reservedIDs(reserved); !slices.Equal(got, want) {
		t.Fatalf("reserved on disk = %v, want %v", got, want)
	}
	if got := reserved[added]; got != (ReservedSlot{Root: dir, Home: home}) {
		t.Fatalf("reservation = %+v, want root %s and home %s", got, dir, home)
	}
	if _, err := store.Add(Account{ID: added, Provider: "claude", Email: "added@example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := reservedIDs(reservedOnDisk(t, dir, "claude")); !slices.Equal(got, []string{selected}) {
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
	id, err := store.ReserveAccountID("codex", t.TempDir())
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

// reservationsJSON is a metadata file whose providerName holds one listed
// account, "live", and the given reservations.
func reservationsJSON(providerName string, reservations map[string]ReservedSlot) string {
	var entries []string
	for _, id := range reservedIDs(reservations) {
		slot := reservations[id]
		entries = append(entries, `"`+id+`":{"root":"`+slot.Root+`","home":"`+slot.Home+`"}`)
	}
	return `{"version":1,"providers":{"` + providerName + `":{"accounts":[{"id":"live","provider":"` + providerName +
		`","addedAt":1}],"reservedSlots":{` + strings.Join(entries, ",") + `}}}}`
}

func recordingRemove(removed *[]string) func(string) error {
	return func(accountID string) error {
		*removed = append(*removed, accountID)
		return nil
	}
}

// An id that is both reserved and saved names a live login. Settling it
// clears the reservation and never deletes the slot.
func TestSettlingAReservationForASavedAccountNeverDeletesItsSlot(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	writeMetadata(t, dir, reservationsJSON("claude", map[string]ReservedSlot{
		"live": {Root: dir, Home: home}, "unsaved": {Root: dir, Home: home},
	}))
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var removed []string
	settlements, err := store.SettleReservations("claude", home, recordingRemove(&removed))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{"unsaved"}) {
		t.Fatalf("removed slots = %v, want only unsaved", removed)
	}
	want := []Settlement{
		{AccountID: "live", Outcome: SettlementListed, Cleared: true},
		{AccountID: "unsaved", Outcome: SettlementRemoved, Cleared: true},
	}
	if !slices.Equal(settlements, want) {
		t.Fatalf("settlements = %+v, want %+v", settlements, want)
	}
	if got := reservedOnDisk(t, dir, "claude"); len(got) != 0 {
		t.Fatalf("reserved after settling = %v, want none", got)
	}
}

// Only the data root that made a reservation settles it, and only against the
// home it names. A copied metadata file, or a run under another home, leaves
// it exactly as it is and says nothing about it.
func TestAReservationOfAnotherDataRootOrHomeIsLeftAlone(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	otherRoot, otherHome := t.TempDir(), t.TempDir()
	reservations := map[string]ReservedSlot{
		"copied":     {Root: otherRoot, Home: home},
		"other-home": {Root: dir, Home: otherHome},
	}
	writeMetadata(t, dir, reservationsJSON("codex", reservations))
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var removed []string
	settlements, err := store.SettleReservations("codex", home, recordingRemove(&removed))
	if err != nil || len(settlements) != 0 || len(removed) != 0 {
		t.Fatalf("settle = (%+v, %v), removed %v; want nothing done and nothing reported", settlements, err, removed)
	}
	if got := reservedOnDisk(t, dir, "codex"); len(got) != 2 || got["copied"] != reservations["copied"] || got["other-home"] != reservations["other-home"] {
		t.Fatalf("reservations after the settle = %+v, want both unchanged", got)
	}
}

// A reservation that does not say where it was made deletes nothing, stays,
// and is reported.
func TestAMalformedReservationDeletesNothingAndIsReported(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	writeMetadata(t, dir, `{"version":1,"providers":{"codex":{"accounts":[],"reservedSlots":{"blank":{},"relative":{"root":"data","home":"home"}}}}}`)
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var removed []string
	settlements, err := store.SettleReservations("codex", home, recordingRemove(&removed))
	if err == nil || !strings.Contains(err.Error(), "blank") || !strings.Contains(err.Error(), "relative") {
		t.Fatalf("error = %v, want both malformed reservations named", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed %v for malformed reservations", removed)
	}
	want := []Settlement{
		{AccountID: "blank", Outcome: SettlementMalformed},
		{AccountID: "relative", Outcome: SettlementMalformed},
	}
	if !slices.Equal(settlements, want) {
		t.Fatalf("settlements = %+v, want %+v", settlements, want)
	}
	if got := reservedIDs(reservedOnDisk(t, dir, "codex")); !slices.Equal(got, []string{"blank", "relative"}) {
		t.Fatalf("reservations = %v, want both kept", got)
	}
}

// A reservation entry of the wrong JSON type fails the whole load, as a
// wrongly typed field anywhere in the file does.
func TestAWronglyTypedReservationFailsTheLoad(t *testing.T) {
	dir := t.TempDir()
	writeMetadata(t, dir, `{"version":1,"providers":{"codex":{"accounts":[],"reservedSlots":{"x":false}}}}`)
	if _, err := NewStore(dir); err == nil {
		t.Fatal("NewStore loaded a reservation that is not a record")
	}
}

// A slot that cannot be removed keeps its reservation for the next settle,
// which retries it; the others settle.
func TestAReservationWhoseSlotCannotBeRemovedStaysForTheNextSettle(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	writeMetadata(t, dir, reservationsJSON("codex", map[string]ReservedSlot{
		"stuck": {Root: dir, Home: home}, "gone": {Root: dir, Home: home},
	}))
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused")
	settlements, err := store.SettleReservations("codex", home, func(accountID string) error {
		if accountID == "stuck" {
			return refused
		}
		return nil
	})
	if !errors.Is(err, refused) {
		t.Fatalf("error = %v, want the removal's failure", err)
	}
	want := []Settlement{
		{AccountID: "gone", Outcome: SettlementRemoved, Cleared: true},
		{AccountID: "stuck", Outcome: SettlementRemoveFailed},
	}
	if !slices.Equal(settlements, want) {
		t.Fatalf("settlements = %+v, want %+v", settlements, want)
	}
	if got := reservedIDs(reservedOnDisk(t, dir, "codex")); !slices.Equal(got, []string{"stuck"}) {
		t.Fatalf("reserved after a failed removal = %v, want stuck kept", got)
	}
	if _, err := store.SettleReservations("codex", home, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := reservedOnDisk(t, dir, "codex"); len(got) != 0 {
		t.Fatalf("reserved after the retry = %v, want none", got)
	}
}

// A settle whose save fails reports the slot removed and the reservation not
// cleared, which is what is true.
func TestASettleWhoseSaveFailsReportsTheReservationNotCleared(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir, home := t.TempDir(), t.TempDir()
	writeMetadata(t, dir, reservationsJSON("codex", map[string]ReservedSlot{"unsaved": {Root: dir, Home: home}}))
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	settlements, err := store.SettleReservations("codex", home, func(string) error { return nil })
	if err == nil {
		t.Fatal("a settle whose save failed reported no error")
	}
	want := []Settlement{{AccountID: "unsaved", Outcome: SettlementRemoved, Cleared: false}}
	if !slices.Equal(settlements, want) {
		t.Fatalf("settlements = %+v, want %+v", settlements, want)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := reservedIDs(reservedOnDisk(t, dir, "codex")); !slices.Equal(got, []string{"unsaved"}) {
		t.Fatalf("reserved on disk = %v, want unsaved kept", got)
	}
}

// SettleReservation settles the one id a failed operation reserved and
// leaves the others to their own operations and the boot.
func TestSettlingOneReservationLeavesTheOthers(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	writeMetadata(t, dir, reservationsJSON("claude", map[string]ReservedSlot{
		"failed": {Root: dir, Home: home}, "in-flight": {Root: dir, Home: home},
	}))
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var removed []string
	if _, err := store.SettleReservation("claude", "failed", home, recordingRemove(&removed)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{"failed"}) {
		t.Fatalf("removed = %v, want only failed", removed)
	}
	if got := reservedIDs(reservedOnDisk(t, dir, "claude")); !slices.Equal(got, []string{"in-flight"}) {
		t.Fatalf("reserved = %v, want in-flight kept", got)
	}
}
