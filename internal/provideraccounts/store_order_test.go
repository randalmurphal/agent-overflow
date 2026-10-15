package provideraccounts

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func listedIDs(store *Store, providerName string) []string {
	var ids []string
	for _, account := range store.List(providerName, time.Now()) {
		ids = append(ids, account.ID)
	}
	return ids
}

// The list is the user's order: making a later account active, or signing an
// earlier one in again, moves neither card.
func TestStoreListKeepsTheSavedOrderAcrossActivation(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two", "three"} {
		if _, err := store.UpsertAndActivate(Account{ID: id, Provider: "claude", Email: id + "@example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Activate("claude", "two"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivateCredential(Account{ID: "one", Provider: "claude", Email: "one@example.com"}); err != nil {
		t.Fatal(err)
	}
	if got, want := listedIDs(store, "claude"), []string{"one", "two", "three"}; !slices.Equal(got, want) {
		t.Fatalf("listed order = %v, want %v", got, want)
	}
}

func TestStoreAddAppendsWithoutChangingTheSelection(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{ID: "one", Provider: "claude", Email: "one@example.com"}); err != nil {
		t.Fatal(err)
	}
	generation := store.Generation("claude")

	added, err := store.Add(Account{ID: "two", Provider: "claude", Email: "two@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if added.AddedAt == 0 {
		t.Fatal("an added account carries no AddedAt")
	}
	if active, ok := store.Active("claude", time.Now()); !ok || active.ID != "one" {
		t.Fatalf("active = %+v, ok=%v, want one", active, ok)
	}
	if got := store.Generation("claude"); got != generation {
		t.Fatalf("generation = %d, want %d unchanged", got, generation)
	}
	if got, want := listedIDs(store, "claude"), []string{"one", "two"}; !slices.Equal(got, want) {
		t.Fatalf("listed order = %v, want %v", got, want)
	}

	if _, err := store.Add(Account{ID: "two", Provider: "claude", Email: "two@example.com"}); err == nil {
		t.Fatal("adding a saved id again succeeded")
	}
	if _, err := store.Add(Account{ID: "three", Provider: "claude", Email: "one@example.com"}); !errors.Is(err, ErrAccountIDMismatch) {
		t.Fatalf("adding a saved identity under a new id = %v, want ErrAccountIDMismatch", err)
	}
	if got := len(store.List("claude", time.Now())); got != 2 {
		t.Fatalf("a refused add changed the list: %d accounts", got)
	}
}

func TestStoreReorderPersistsAndLeavesTheSelectionAlone(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two", "three"} {
		if _, err := store.UpsertAndActivate(Account{ID: id, Provider: "claude", Email: id + "@example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	generation := store.Generation("claude")

	if err := store.Reorder("claude", []string{"three", "one", "two"}); err != nil {
		t.Fatal(err)
	}
	if active, ok := store.Active("claude", time.Now()); !ok || active.ID != "three" {
		t.Fatalf("active = %+v, ok=%v, want three", active, ok)
	}
	if got := store.Generation("claude"); got != generation {
		t.Fatalf("generation = %d, want %d unchanged", got, generation)
	}
	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := listedIDs(reloaded, "claude"), []string{"three", "one", "two"}; !slices.Equal(got, want) {
		t.Fatalf("reloaded order = %v, want %v", got, want)
	}
}

// A reorder from a client holding a stale listing names a different set. It
// is refused whole rather than dropping or duplicating a card.
func TestStoreReorderRefusesAListThatIsNotTheSavedSet(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if _, err := store.UpsertAndActivate(Account{ID: id, Provider: "claude", Email: id + "@example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	for name, ids := range map[string][]string{
		"missing":   {"two"},
		"duplicate": {"two", "two"},
		"unknown":   {"two", "gone"},
		"extra":     {"two", "one", "gone"},
	} {
		if err := store.Reorder("claude", ids); !errors.Is(err, ErrAccountOrderStale) {
			t.Fatalf("%s: Reorder(%v) = %v, want ErrAccountOrderStale", name, ids, err)
		}
	}
	if got, want := listedIDs(store, "claude"), []string{"one", "two"}; !slices.Equal(got, want) {
		t.Fatalf("a refused reorder changed the order: %v", got)
	}
}
