package provideraccounts

import (
	"errors"
	"os"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func newResetTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []Account{
		{ID: "one", Provider: "codex", Email: "one@example.com"},
		{ID: "two", Provider: "codex", Email: "two@example.com"},
	} {
		if _, err := store.Add(account); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Activate("codex", "one"); err != nil {
		t.Fatal(err)
	}
	return store, dir
}

func resetOption(id string) UsageResetOption {
	return UsageResetOption{ID: id, Kind: "codexRateLimits", Label: "Full reset", Usable: true}
}

func listed(options ...UsageResetOption) ResetReading {
	return ResetReading{Options: options}
}

func usageResets(t *testing.T, store *Store, accountID string) *UsageResets {
	t.Helper()
	account, ok := store.Get("codex", accountID, time.Now())
	if !ok {
		t.Fatalf("account %s missing", accountID)
	}
	return account.UsageResets
}

func start(key string) PendingReset {
	return PendingReset{ID: "spend-" + key, Key: key, StartedAt: 1}
}

func TestSetUsageResetsMovesTheRevisionOnlyOnAChange(t *testing.T) {
	t.Parallel()
	store, _ := newResetTestStore(t)
	if usageResets(t, store, "one") != nil {
		t.Fatal("a fresh account carries a reset list")
	}
	reading := listed(resetOption("a"), resetOption("b"))
	if _, changed, err := store.SetUsageResets("codex", "one", reading); err != nil || !changed {
		t.Fatalf("first list: changed=%v err=%v", changed, err)
	}
	if got := usageResets(t, store, "one"); got.Revision != 1 || len(got.Options) != 2 {
		t.Fatalf("after first list: %+v", got)
	}
	if _, changed, err := store.SetUsageResets("codex", "one", reading); err != nil || changed {
		t.Fatalf("same list again: changed=%v err=%v", changed, err)
	}
	// A different count of unlisted resets is a different list.
	reading.Unlisted = 3
	if _, changed, err := store.SetUsageResets("codex", "one", reading); err != nil || !changed {
		t.Fatalf("unlisted count changed: changed=%v err=%v", changed, err)
	}
	if got := usageResets(t, store, "one"); got.Revision != 2 || got.Unlisted != 3 {
		t.Fatalf("after the unlisted count changed: %+v", got)
	}
	// An empty list is still a list: the revision does not restart, so a
	// screen that saw revision 1 cannot match a later list by accident.
	if _, _, err := store.SetUsageResets("codex", "one", ResetReading{}); err != nil {
		t.Fatal(err)
	}
	got := usageResets(t, store, "one")
	if got.Revision != 3 || got.Options == nil || len(got.Options) != 0 || got.Unlisted != 0 {
		t.Fatalf("after emptying: %+v", got)
	}
	if usageResets(t, store, "two") != nil {
		t.Fatal("another account's list changed")
	}
}

func TestSetUsageResetsRefusesOptionsItCannotSpendByID(t *testing.T) {
	t.Parallel()
	store, _ := newResetTestStore(t)
	for name, reading := range map[string]ResetReading{
		"shared id":         listed(resetOption("a"), resetOption("a")),
		"no id":             listed(resetOption("")),
		"negative unlisted": {Unlisted: -1},
	} {
		if _, _, err := store.SetUsageResets("codex", "one", reading); !errors.Is(err, errResetReading) {
			t.Fatalf("%s: err = %v, want errResetReading", name, err)
		}
	}
	if usageResets(t, store, "one") != nil {
		t.Fatal("a refused reading was recorded")
	}
}

func TestUsageResetsSurviveEveryOtherRowWriteAndAReload(t *testing.T) {
	t.Parallel()
	store, dir := newResetTestStore(t)
	if _, _, err := store.SetUsageResets("codex", "two", ResetReading{Options: []UsageResetOption{resetOption("a")}, Unlisted: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateMetadata(Account{ID: "two", Provider: "codex", Email: "two@example.com", DisplayName: "Two"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{ID: "two", Provider: "codex", Email: "two@example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RememberRateLimits("codex", "two", provider.RateLimitsSnapshot{Provider: "codex", Limits: []provider.RateLimitEntry{{LimitID: "codex", WindowMins: 300, UsedPercent: 4}}}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*Store{"live": store, "reloaded": reloaded} {
		got := usageResets(t, s, "two")
		if got == nil || got.Revision != 1 || len(got.Options) != 1 || got.Options[0].Kind != "codexRateLimits" || got.Unlisted != 2 {
			t.Fatalf("%s store: resets = %+v", name, got)
		}
	}
}

// Begin records the spend and moves the revision in one save: from then on
// the list it was chosen from cannot spend, and a restart still sees it.
func TestBeginResetSpendRecordsTheSpendAndRetiresItsList(t *testing.T) {
	t.Parallel()
	store, dir := newResetTestStore(t)
	option := resetOption("a")
	option.Label, option.Refills = "Weekly reset", "Refills the week."
	if _, _, err := store.SetUsageResets("codex", "one", listed(option)); err != nil {
		t.Fatal(err)
	}
	began, err := store.BeginResetSpend("codex", "one", 1, "a", start("k1"))
	if err != nil {
		t.Fatal(err)
	}
	if began.CreditID != "a" || began.Label != "Weekly reset" || began.Refills != "Refills the week." || began.ID != "spend-k1" {
		t.Fatalf("began = %+v, want the option's credit and words under the given id", began)
	}
	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := usageResets(t, reloaded, "one"); got.Revision != 2 {
		t.Fatalf("revision after begin = %d, want 2", got.Revision)
	}
	if got, ok := reloaded.PendingReset("codex", "one"); !ok || got != began {
		t.Fatalf("after reload: pending = %+v ok=%v, want %+v", got, ok, began)
	}
	// While it stands no other spend begins, from any list.
	var pendingErr *ResetPendingError
	if _, err := reloaded.BeginResetSpend("codex", "one", 2, "a", start("k2")); !errors.As(err, &pendingErr) || pendingErr.Pending.Key != "k1" {
		t.Fatalf("second begin: err = %v", err)
	}
	if err := reloaded.CheckResetSpend("codex", "one", 2, "a"); !errors.As(err, &pendingErr) {
		t.Fatalf("check while unsettled: err = %v", err)
	}
	// A reading does not end it, even one that drops the credit.
	if _, _, err := reloaded.SetUsageResets("codex", "one", ResetReading{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.PendingReset("codex", "one"); !ok {
		t.Fatal("a reading ended the unsettled spend")
	}
}

// The store owns the rule for whether a spend begins, and CheckResetSpend
// asks the same rule without recording anything.
func TestBeginAndCheckRefuseWhatTheListCannotSpend(t *testing.T) {
	t.Parallel()
	store, _ := newResetTestStore(t)
	redeeming := resetOption("busy")
	redeeming.Usable, redeeming.Reason = false, "Codex is still applying this reset."
	refusals := []struct {
		name     string
		revision uint64
		option   string
		check    func(error) bool
	}{
		{"no list yet", 0, "a", func(err error) bool { return errors.Is(err, ErrResetListStale) }},
	}
	for _, refusal := range refusals {
		if _, err := store.BeginResetSpend("codex", "one", refusal.revision, refusal.option, start("k")); !refusal.check(err) {
			t.Fatalf("begin, %s: err = %v", refusal.name, err)
		}
	}
	if _, _, err := store.SetUsageResets("codex", "one", listed(resetOption("a"), redeeming)); err != nil {
		t.Fatal(err)
	}
	var unusable *ResetUnusableError
	refusals = []struct {
		name     string
		revision uint64
		option   string
		check    func(error) bool
	}{
		{"an old revision", 0, "a", func(err error) bool { return errors.Is(err, ErrResetListStale) }},
		{"an option the list does not hold", 1, "gone", func(err error) bool { return errors.Is(err, ErrResetListStale) }},
		{"an unusable option", 1, "busy", func(err error) bool {
			return errors.As(err, &unusable) && unusable.Option.Reason == redeeming.Reason
		}},
	}
	for _, refusal := range refusals {
		if err := store.CheckResetSpend("codex", "one", refusal.revision, refusal.option); !refusal.check(err) {
			t.Fatalf("check, %s: err = %v", refusal.name, err)
		}
		if _, err := store.BeginResetSpend("codex", "one", refusal.revision, refusal.option, start("k")); !refusal.check(err) {
			t.Fatalf("begin, %s: err = %v", refusal.name, err)
		}
	}
	if err := store.CheckResetSpend("codex", "one", 1, "a"); err != nil {
		t.Fatalf("check of a usable option: %v", err)
	}
	if _, err := store.BeginResetSpend("codex", "one", 1, "a", PendingReset{Key: "k"}); err == nil {
		t.Fatal("a spend without an id began")
	}
	if _, err := store.BeginResetSpend("codex", "one", 1, "a", PendingReset{ID: "s"}); err == nil {
		t.Fatal("a spend without a key began")
	}
	if _, err := store.BeginResetSpend("codex", "missing", 1, "a", start("k")); err == nil {
		t.Fatal("a spend for an unsaved account began")
	}
	if _, ok := store.PendingReset("codex", "one"); ok {
		t.Fatal("a refused begin left a record")
	}
	if got := usageResets(t, store, "one"); got.Revision != 1 {
		t.Fatalf("a refused begin or a check moved the revision to %d", got.Revision)
	}
}

func TestSettleResetSpendEndsOnlyItsOwnSpend(t *testing.T) {
	t.Parallel()
	store, _ := newResetTestStore(t)
	if _, _, err := store.SetUsageResets("codex", "one", listed(resetOption("a"), resetOption("b"))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginResetSpend("codex", "one", 1, "a", start("k1")); err != nil {
		t.Fatal(err)
	}
	if settled, err := store.SettleResetSpend("codex", "one", "other-key", true); err != nil || settled {
		t.Fatalf("another key settled: %v %v", settled, err)
	}
	if _, ok := store.PendingReset("codex", "one"); !ok {
		t.Fatal("another key's settle ended the spend")
	}
	if settled, err := store.SettleResetSpend("codex", "one", "k1", true); err != nil || !settled {
		t.Fatalf("settle: %v %v", settled, err)
	}
	if _, ok := store.PendingReset("codex", "one"); ok {
		t.Fatal("a settled spend kept its record")
	}
}

// An answer that says the credit is gone removes it in the settle's save;
// one that says it is not (nothingToReset) and an unsent spend leave the
// list.
func TestSettleDropsOnlyACreditTheAnswerSaysIsGone(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		creditGone bool
		want       []string
		revision   uint64
	}{
		"credit gone":       {true, []string{"b"}, 3},
		"credit still held": {false, []string{"a", "b"}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store, dir := newResetTestStore(t)
			if _, _, err := store.SetUsageResets("codex", "one", listed(resetOption("a"), resetOption("b"))); err != nil {
				t.Fatal(err)
			}
			if _, err := store.BeginResetSpend("codex", "one", 1, "a", start("k1")); err != nil {
				t.Fatal(err)
			}
			if settled, err := store.SettleResetSpend("codex", "one", "k1", test.creditGone); err != nil || !settled {
				t.Fatalf("settle: %v %v", settled, err)
			}
			reloaded, err := NewStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			got := usageResets(t, reloaded, "one")
			var ids []string
			for _, option := range got.Options {
				ids = append(ids, option.ID)
			}
			if got.Revision != test.revision || len(ids) != len(test.want) || ids[0] != test.want[0] {
				t.Fatalf("list after settle = %+v, want ids %v at revision %d", got, test.want, test.revision)
			}
		})
	}
}

func TestRemoveForgetsTheUnsettledSpend(t *testing.T) {
	t.Parallel()
	store, _ := newResetTestStore(t)
	if _, _, err := store.SetUsageResets("codex", "one", listed(resetOption("a"))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginResetSpend("codex", "one", 1, "a", start("k1")); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove("codex", "one", "two"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add(Account{ID: "one", Provider: "codex", Email: "one@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.PendingReset("codex", "one"); ok {
		t.Fatal("an account added again under a removed id inherited its unsettled spend")
	}
}

// A begin whose save fails leaves nothing recorded, so the caller sends
// nothing.
func TestBeginResetSpendThatFailsToSaveRecordsNothing(t *testing.T) {
	t.Parallel()
	store, dir := newResetTestStore(t)
	if _, _, err := store.SetUsageResets("codex", "one", listed(resetOption("a"))); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := store.BeginResetSpend("codex", "one", 1, "a", start("k1")); err == nil {
		t.Fatal("a begin whose save failed reported success")
	}
	if _, ok := store.PendingReset("codex", "one"); ok {
		t.Fatal("a begin whose save failed left a record")
	}
	if got := usageResets(t, store, "one"); got.Revision != 1 {
		t.Fatalf("a begin whose save failed moved the revision to %d", got.Revision)
	}
}

// Every poll of every account writes its reading's list; an unchanged one
// must cost nothing beyond the comparison.
func TestSetUsageResetsAllocatesNothingForAnUnchangedReading(t *testing.T) {
	store, _ := newResetTestStore(t)
	reading := ResetReading{Options: []UsageResetOption{resetOption("a"), resetOption("b")}, Unlisted: 1}
	if _, _, err := store.SetUsageResets("codex", "one", reading); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		if _, changed, err := store.SetUsageResets("codex", "one", reading); err != nil || changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
	})
	if allocs != 0 {
		t.Fatalf("an unchanged reading allocated %v times", allocs)
	}
}

// A row gets a list only through the reset writers: whatever a caller passes
// when it inserts one is dropped.
func TestInsertedRowsStartWithoutAResetList(t *testing.T) {
	store, _ := newResetTestStore(t)
	carried := &UsageResets{Revision: 9, Options: []UsageResetOption{resetOption("a")}}
	if _, err := store.Add(Account{ID: "three", Provider: "codex", Email: "three@example.com", UsageResets: carried}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{ID: "four", Provider: "codex", Email: "four@example.com", UsageResets: carried}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"three", "four"} {
		if got := usageResets(t, store, id); got != nil {
			t.Fatalf("inserted row %s carries %+v", id, got)
		}
	}
}
