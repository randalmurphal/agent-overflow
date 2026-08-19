package provideraccounts

import (
	"errors"
	"strings"
	"testing"
)

func newIdentityTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func mustUpsert(t *testing.T, store *Store, account Account) Account {
	t.Helper()
	saved, err := store.UpsertAndActivate(account)
	if err != nil {
		t.Fatalf("UpsertAndActivate(%+v): %v", account, err)
	}
	return saved
}

func TestContradicts(t *testing.T) {
	cases := []struct {
		name string
		a, b Identity
		want bool
	}{
		{"all blank", Identity{}, Identity{}, false},
		{"same email", Identity{Email: "a@x.com"}, Identity{Email: "A@X.com"}, false},
		{"different email", Identity{Email: "a@x.com"}, Identity{Email: "b@x.com"}, true},
		{"blank email never contradicts", Identity{Email: ""}, Identity{Email: "a@x.com"}, false},
		{"same email different org id", Identity{Email: "a@x.com", OrgID: "org-1"}, Identity{Email: "a@x.com", OrgID: "org-2"}, true},
		{"blank org id never contradicts", Identity{Email: "a@x.com", OrgID: ""}, Identity{Email: "a@x.com", OrgID: "org-2"}, false},
		// OrgName is display state from two independent sources (probe wire
		// vs oauthAccount record) and changes on a rename; it must never be
		// evidence of a different login — a name-axis contradiction on the
		// usage-refresh path would drop a just-spent token rotation.
		{"org names never contradict", Identity{Email: "a@x.com", OrgName: "Alpha"}, Identity{Email: "a@x.com", OrgName: "Beta"}, false},
		{"rename under equal ids is not a contradiction", Identity{OrgID: "org-1", OrgName: "Alpha"}, Identity{OrgID: "org-1", OrgName: "Beta"}, false},
		{"names with one blank id do not contradict", Identity{OrgID: "org-1", OrgName: "Alpha"}, Identity{OrgName: "Beta"}, false},
	}
	for _, tc := range cases {
		if got := tc.a.Contradicts(tc.b); got != tc.want {
			t.Errorf("%s: Contradicts = %v, want %v", tc.name, got, tc.want)
		}
		if got := tc.b.Contradicts(tc.a); got != tc.want {
			t.Errorf("%s (reversed): Contradicts = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestConfirms(t *testing.T) {
	cases := []struct {
		name string
		a, b Identity
		want bool
	}{
		{"all blank is not a confirmation", Identity{}, Identity{}, false},
		{"one blank email is not a confirmation", Identity{Email: "a@x.com"}, Identity{}, false},
		{"equal emails confirm", Identity{Email: "a@x.com"}, Identity{Email: "A@X.com"}, true},
		{"different org id defeats equal emails", Identity{Email: "a@x.com", OrgID: "org-1"}, Identity{Email: "a@x.com", OrgID: "org-2"}, false},
		{"blank org id still confirms", Identity{Email: "a@x.com", OrgID: "org-1"}, Identity{Email: "a@x.com"}, true},
		{"name difference does not defeat", Identity{Email: "a@x.com", OrgName: "Alpha"}, Identity{Email: "a@x.com", OrgName: "Beta"}, true},
	}
	for _, tc := range cases {
		if got := tc.a.Confirms(tc.b); got != tc.want {
			t.Errorf("%s: Confirms = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFindByIdentityLattice(t *testing.T) {
	store := newIdentityTestStore(t)
	legacy := mustUpsert(t, store, Account{ID: "legacy", Provider: "claude", Email: "solo@x.com"})
	mustUpsert(t, store, Account{ID: "a", Provider: "claude", Email: "multi@x.com", OrgID: "org-a", OrgName: "Alpha"})
	orgB := mustUpsert(t, store, Account{ID: "b", Provider: "claude", Email: "multi@x.com", OrgID: "org-b", OrgName: "Beta"})

	cases := []struct {
		name      string
		observed  Identity
		wantID    string
		wantFound bool
		wantErr   error
	}{
		{"blank email never matches", Identity{}, "", false, nil},
		{"unknown email", Identity{Email: "nobody@x.com"}, "", false, nil},
		{"sole legacy blank-org account matches by email", Identity{Email: "Solo@X.com", OrgID: "org-new"}, legacy.ID, true, nil},
		{"org id picks between same-email accounts", Identity{Email: "multi@x.com", OrgID: "org-b"}, orgB.ID, true, nil},
		{"padded observed values still match", Identity{Email: " multi@x.com ", OrgID: " org-b "}, orgB.ID, true, nil},
		{"unknown org id is a new login", Identity{Email: "multi@x.com", OrgID: "org-c"}, "", false, nil},
		{"email-only over several orgs is ambiguous", Identity{Email: "multi@x.com"}, "", false, ErrAmbiguousIdentity},
		// A name is display state and never decides — an observation naming
		// one of the saved orgs but carrying no id is exactly as ambiguous
		// as one naming nothing, because a rename or a source-formatting
		// difference would otherwise silently pick the wrong account.
		{"an observed name does not disambiguate", Identity{Email: "multi@x.com", OrgName: "Alpha"}, "", false, ErrAmbiguousIdentity},
	}
	for _, tc := range cases {
		got, found, err := store.FindByIdentity("claude", tc.observed)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if found != tc.wantFound || (found && got.ID != tc.wantID) {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tc.name, got.ID, found, tc.wantID, tc.wantFound)
		}
	}
}

func TestFindByIdentityEmailOnlySingleCandidateStillMatches(t *testing.T) {
	store := newIdentityTestStore(t)
	saved := mustUpsert(t, store, Account{ID: "one", Provider: "codex", Email: "only@x.com", OrgID: "org-1"})
	got, found, err := store.FindByIdentity("codex", Identity{Email: "only@x.com"})
	if err != nil || !found || got.ID != saved.ID {
		t.Fatalf("got (%q, %v, %v), want (%q, true, nil)", got.ID, found, err, saved.ID)
	}
}

func TestSameEmailAccountsNeedDistinctOrgIDs(t *testing.T) {
	store := newIdentityTestStore(t)
	mustUpsert(t, store, Account{ID: "a", Provider: "claude", Email: "dup@x.com", OrgID: "org-a"})

	// Distinct org: allowed.
	mustUpsert(t, store, Account{ID: "b", Provider: "claude", Email: "dup@x.com", OrgID: "org-b"})

	// Same org under a new ID: refused.
	if _, err := store.UpsertAndActivate(Account{ID: "c", Provider: "claude", Email: "dup@x.com", OrgID: "org-a"}); !errors.Is(err, ErrAccountIDMismatch) {
		t.Fatalf("same org duplicate: err = %v, want ErrAccountIDMismatch", err)
	}
	// Blank org under a new ID: refused — unknown must resolve via matching.
	if _, err := store.UpsertAndActivate(Account{ID: "d", Provider: "claude", Email: "dup@x.com"}); !errors.Is(err, ErrAccountIDMismatch) {
		t.Fatalf("blank org duplicate: err = %v, want ErrAccountIDMismatch", err)
	}
}

func TestUpsertEnrichesBlankOrgAndPreservesKnownOrg(t *testing.T) {
	store := newIdentityTestStore(t)
	mustUpsert(t, store, Account{ID: "a", Provider: "claude", Email: "e@x.com"})

	// Enrichment: first observation carrying the org lands it.
	enriched := mustUpsert(t, store, Account{ID: "a", Provider: "claude", Email: "e@x.com", OrgID: "org-1", OrgName: "Alpha"})
	if enriched.OrgID != "org-1" || enriched.OrgName != "Alpha" {
		t.Fatalf("enrichment failed: %+v", enriched)
	}

	// A later blank observation must not erase knowledge.
	preserved := mustUpsert(t, store, Account{ID: "a", Provider: "claude", Email: "e@x.com"})
	if preserved.OrgID != "org-1" || preserved.OrgName != "Alpha" {
		t.Fatalf("blank write erased org fields: %+v", preserved)
	}

	// A rename with the same id updates the display name.
	renamed := mustUpsert(t, store, Account{ID: "a", Provider: "claude", Email: "e@x.com", OrgID: "org-1", OrgName: "Alpha Renamed"})
	if renamed.OrgName != "Alpha Renamed" {
		t.Fatalf("rename not applied: %+v", renamed)
	}
}

func TestUpsertRefusesOrgRebind(t *testing.T) {
	store := newIdentityTestStore(t)
	mustUpsert(t, store, Account{ID: "a", Provider: "codex", Email: "e@x.com", OrgID: "org-1"})
	_, err := store.UpsertAndActivate(Account{ID: "a", Provider: "codex", Email: "e@x.com", OrgID: "org-2"})
	if err == nil || !strings.Contains(err.Error(), "refusing to rebind") {
		t.Fatalf("org rebind: err = %v, want rebind refusal", err)
	}
}

func TestUpdateMetadataEnforcesTheSameOrgRules(t *testing.T) {
	store := newIdentityTestStore(t)
	mustUpsert(t, store, Account{ID: "a", Provider: "claude", Email: "e@x.com", OrgID: "org-1", OrgName: "Alpha"})
	mustUpsert(t, store, Account{ID: "b", Provider: "claude", Email: "e@x.com", OrgID: "org-2"})

	// Blank write preserves.
	updated, err := store.UpdateMetadata(Account{ID: "a", Provider: "claude", Email: "e@x.com"})
	if err != nil || updated.OrgID != "org-1" || updated.OrgName != "Alpha" {
		t.Fatalf("UpdateMetadata blank org: %+v, %v", updated, err)
	}
	// Rebind refused.
	if _, err := store.UpdateMetadata(Account{ID: "a", Provider: "claude", Email: "e@x.com", OrgID: "org-2"}); err == nil {
		t.Fatal("UpdateMetadata rebind: expected error")
	}
	// An account id the store does not know is refused — UpdateMetadata
	// never creates rows.
	if _, err := store.UpdateMetadata(Account{ID: "c", Provider: "claude", Email: "e@x.com"}); err == nil {
		t.Fatal("UpdateMetadata unknown account: expected error")
	}
}

// Every match FindByIdentity reports must be accepted when written back
// under the matched account's id — a finder answer the write layer then
// refuses would hot-loop the reconciler (adopt → refused → refetch).
func TestFindByIdentityMatchesAreWritable(t *testing.T) {
	cases := []struct {
		name     string
		saved    []Account
		observed Identity
	}{
		{
			"sole blank-org enrichment",
			[]Account{{ID: "legacy", Provider: "claude", Email: "e@x.com"}},
			Identity{Email: "e@x.com", OrgID: "org-1", OrgName: "Alpha"},
		},
		{
			"org id match among siblings",
			[]Account{
				{ID: "a", Provider: "claude", Email: "e@x.com", OrgID: "org-1"},
				{ID: "b", Provider: "claude", Email: "e@x.com", OrgID: "org-2"},
			},
			Identity{Email: "e@x.com", OrgID: "org-2"},
		},
		{
			"email-only single candidate",
			[]Account{{ID: "a", Provider: "claude", Email: "e@x.com", OrgID: "org-1"}},
			Identity{Email: "e@x.com"},
		},
	}
	for _, tc := range cases {
		store := newIdentityTestStore(t)
		for _, account := range tc.saved {
			mustUpsert(t, store, account)
		}
		target, found, err := store.FindByIdentity("claude", tc.observed)
		if err != nil || !found {
			t.Errorf("%s: FindByIdentity = (%v, %v), want a match", tc.name, found, err)
			continue
		}
		write := Account{
			ID: target.ID, Provider: "claude",
			Email: tc.observed.Email, OrgID: tc.observed.OrgID, OrgName: tc.observed.OrgName,
		}
		if _, err := store.UpsertAndActivate(write); err != nil {
			t.Errorf("%s: matched account %s refused the write-back: %v", tc.name, target.ID, err)
		}
	}
}
