package trackerapp

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// creditRules is one snapshot of credit rules from from (nil: from the
// save).
func creditRules(from *int64, share, need int, actions []string, hours int) CreditRules {
	return CreditRules{From: from, ReviewShare: share, ReviewNeed: need, ReviewActions: actions, HoursPerPoint: hours}
}

// storedCredit is the credit section project id's setup file holds, nil
// for none.
func (m *mirrorFixture) storedCredit(t *testing.T, id string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(m.setupPathOf(t, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var sections map[string]map[string]any
	if err := json.Unmarshal(data, &sections); err != nil {
		t.Fatal(err)
	}
	return sections[sectionCredit]
}

// A project with no credit section reads as off with the default rules,
// and a save that says no more than that leaves no file. A section saved
// is in the file as saved, its actions in their order and its site as the
// connection names it; turned off it keeps its rules and its support
// picks.
func TestCreditSectionSavesAndReads(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	view := must[SetupView](t)(m.svc.ProjectSetup(m.project()))
	if !reflect.DeepEqual(view.Setup.Credit, defaultCredit()) {
		t.Fatalf("no section reads as %+v; want the defaults", view.Setup.Credit)
	}
	if c := view.Setup.Credit.Rules[0]; *c.From != 0 || c.ReviewShare != 10 || c.ReviewNeed != 2 || len(c.ReviewActions) != 3 || c.HoursPerPoint != 4 {
		t.Fatalf("the default rules %+v", c)
	}
	m.saveSetup(t, m.project(), view.Setup)
	if _, err := os.Stat(m.setupPathOf(t, m.project())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a save of the defaults left a file: %v", err)
	}

	on := &Credit{On: true, Rules: []CreditRules{creditRules(new(int64(0)), 15, 1, []string{"logged_time", "commented"}, 6)},
		Support: []SiteSupport{{Tracker: store.TrackerKindJira, Site: jiraSiteIn, Types: []string{"10004"}, Labels: []string{"support"}}}}
	saved := m.saveSetup(t, m.project(), Setup{Credit: on})
	want := &Credit{On: true, Rules: []CreditRules{creditRules(new(int64(0)), 15, 1, []string{"commented", "logged_time"}, 6)},
		Support: []SiteSupport{{Tracker: store.TrackerKindJira, Site: m.conn.Site, Types: []string{"10004"}, Labels: []string{"support"}, Boards: []string{}}}}
	if !reflect.DeepEqual(saved.Stored.Setup.Credit, want) {
		t.Fatalf("stored %+v\nwant %+v", saved.Stored.Setup.Credit, want)
	}
	file := m.storedCredit(t, m.project())
	if file["version"] != 1.0 || file["on"] != true || len(file["rules"].([]any)) != 1 || len(file["support"].([]any)) != 1 {
		t.Fatalf("the file's credit section %v", file)
	}
	again := must[SetupView](t)(m.svc.ProjectSetup(m.project()))
	if !reflect.DeepEqual(again.Setup.Credit, want) {
		t.Fatalf("read again %+v", again.Setup.Credit)
	}

	off := *want
	off.On = false
	stored := m.saveSetup(t, m.project(), Setup{Credit: &off})
	if file := m.storedCredit(t, m.project()); file == nil || file["on"] != false || len(file["support"].([]any)) != 1 {
		t.Fatalf("off kept %v", file)
	}
	if !reflect.DeepEqual(stored.Stored.Setup.Credit, &off) {
		t.Fatalf("off reads as %+v", stored.Stored.Setup.Credit)
	}
}

// A credit section a save gives is checked whole: each refusal names the
// value and why, and nothing is written.
func TestCreditSectionValidates(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	all := []string{"commented", "approved", "logged_time"}
	rules := func(r ...CreditRules) Setup { return Setup{Credit: &Credit{On: true, Rules: r}} }
	support := func(s SiteSupport) Setup {
		return Setup{Credit: &Credit{On: true, Rules: []CreditRules{creditRules(new(int64(0)), 10, 2, all, 4)}, Support: []SiteSupport{s}}}
	}
	labels := make([]string, creditMaxSupport+1)
	for i := range labels {
		labels[i] = strings.Repeat("l", i+1)
	}
	for name, c := range map[string]struct {
		setup Setup
		says  string
	}{
		"no rules":                {Setup{Credit: &Credit{On: true}}, "credit has no rules"},
		"a first rule not at 0":   {rules(creditRules(new(int64(5)), 10, 2, all, 4)), "the first rule of credit takes effect at 5"},
		"a rule out of order":     {rules(creditRules(new(int64(0)), 10, 2, all, 4), creditRules(new(int64(0)), 10, 2, all, 4)), "rule 2 of credit takes effect at 0, not after"},
		"an older rule with none": {rules(creditRules(nil, 10, 2, all, 4), creditRules(new(int64(9)), 10, 2, all, 4)), "rule 1 of credit says no time"},
		"a share over 100":        {rules(creditRules(new(int64(0)), 101, 2, all, 4)), "101% of a ticket's points; the share is 0 to 100"},
		"a negative share":        {rules(creditRules(new(int64(0)), -5, 2, all, 4)), "-5%"},
		"no hours a point":        {rules(creditRules(new(int64(0)), 10, 2, all, 0)), "every 0 hours of support; it is 1 to 16"},
		"17 hours a point":        {rules(creditRules(new(int64(0)), 10, 2, all, 17)), "every 17 hours"},
		"an action of no name":    {rules(creditRules(new(int64(0)), 10, 1, []string{"reacted"}, 4)), `review action "reacted", which is none of commented, approved, logged_time`},
		"an action twice":         {rules(creditRules(new(int64(0)), 10, 1, []string{"approved", "approved"}, 4)), "approved twice"},
		"no action":               {rules(creditRules(new(int64(0)), 10, 1, nil, 4)), "counts no review action"},
		"need over the actions":   {rules(creditRules(new(int64(0)), 10, 3, []string{"approved", "commented"}, 4)), "needs 3 kinds of review action of the 2"},
		"need none":               {rules(creditRules(new(int64(0)), 10, 0, all, 4)), "needs 0 kinds"},
		"a Linear site":           {support(SiteSupport{Tracker: store.TrackerKindLinear, Site: "https://linear.app/example/", Labels: []string{"support"}}), "credit is counted on Jira only"},
		"a label twice":           {support(SiteSupport{Tracker: store.TrackerKindJira, Site: jiraSiteIn, Labels: []string{"support", "support"}}), `lists support label "support" twice`},
		"too many labels":         {support(SiteSupport{Tracker: store.TrackerKindJira, Site: jiraSiteIn, Labels: labels}), "lists 101 support labels, over 100"},
		"an empty type":           {support(SiteSupport{Tracker: store.TrackerKindJira, Site: jiraSiteIn, Types: []string{""}}), "support issue type ID is empty"},
	} {
		_, err := m.svc.SetProjectSetup(m.project(), "", c.setup)
		if !errors.Is(err, ErrInvalidSetup) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v; want it to say %q", name, err, c.says)
		}
	}
	if _, err := os.Stat(m.setupPathOf(t, m.project())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused credit section left a file: %v", err)
	}
	// A support entry that names nothing is dropped.
	saved := m.saveSetup(t, m.project(), support(SiteSupport{Tracker: store.TrackerKindJira, Site: jiraSiteIn}))
	if got := saved.Stored.Setup.Credit.Support; len(got) != 0 {
		t.Fatalf("an empty support entry was kept: %+v", got)
	}
}

// A credit section a newer version saved is kept byte for byte, reads as
// none (no default, so no change is offered) and counts no credit; a
// save of another section keeps it, and one that sets credit is refused.
func TestANewerCreditSectionIsKeptAndCountsNone(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	section := `{"version": 2,  "on": true, "share": "per person"}`
	m.writeSetupFile(t, m.project(), `{"credit": `+section+`}`)
	view := must[SetupView](t)(m.svc.ProjectSetup(m.project()))
	if !slices.Equal(view.Newer, []string{sectionCredit}) || view.Setup.Credit != nil {
		t.Fatalf("a newer credit section reads as %+v, newer %v", view.Setup.Credit, view.Newer)
	}
	p, err := m.st.GetProject(m.project())
	if err != nil {
		t.Fatal(err)
	}
	m.svc.setupMu.Lock()
	held, err := m.svc.heldSetupLocked(p.Slug)
	m.svc.setupMu.Unlock()
	if err != nil || held.credit() != nil {
		t.Fatalf("a newer credit section counts credit: %+v, %v", held, err)
	}
	m.saveSetup(t, m.project(), m.regrouped(map[string]string{"s1": "done"}))
	data, err := os.ReadFile(m.setupPathOf(t, m.project()))
	if err != nil || !strings.Contains(string(data), `"credit": `+section) {
		t.Fatalf("the kept section: %v\n%s", err, data)
	}
	stored := must[SetupView](t)(m.svc.ProjectSetup(m.project()))
	on := &Credit{On: true, Rules: defaultCredit().Rules}
	if _, err := m.svc.SetProjectSetup(m.project(), stored.Revision, Setup{Credit: on}); !errors.Is(err, ErrSetupNewer) {
		t.Fatalf("a save of credit over a newer section: %v", err)
	}
}

// A rule change from now on is stamped by the writer: a section's first
// snapshot takes effect at the start and a later one from the save, and
// one with the newest's values adds nothing. No save replaces or removes
// a snapshot, so a sprint that completed while a save ran, or while the
// app was off, keeps the rule it completed under. A change for every
// sprint rewrites the snapshots and adds none.
func TestCreditRuleChangesStampTheirTime(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	var clock atomic.Int64
	clock.Store(m.now().UnixMilli())
	m.svc.now = func() time.Time { return time.UnixMilli(clock.Load()) }
	all := []string{"commented", "approved", "logged_time"}
	save := func(rules ...CreditRules) []CreditRules {
		t.Helper()
		clock.Add(1000)
		return m.saveSetup(t, m.project(), Setup{Credit: &Credit{On: true, Rules: rules}}).Stored.Setup.Credit.Rules
	}
	got := save(creditRules(nil, 10, 2, all, 4))
	if len(got) != 1 || *got[0].From != 0 {
		t.Fatalf("a first snapshot from the save: %+v", got)
	}
	// The rule a sprint was counted with stays beside each later one,
	// whether the store has read the sprint's close or not.
	got = save(append(got, creditRules(nil, 20, 2, all, 4))...)
	if len(got) != 2 || *got[0].From != 0 || got[0].ReviewShare != 10 || *got[1].From != clock.Load() || got[1].ReviewShare != 20 {
		t.Fatalf("a change: %+v", got)
	}
	got = save(append(got, creditRules(nil, 25, 2, all, 4))...)
	if len(got) != 3 || got[1].ReviewShare != 20 || *got[2].From != clock.Load() || got[2].ReviewShare != 25 {
		t.Fatalf("a second change: %+v", got)
	}
	// The newest's values again, its actions in another order: nothing
	// added.
	kept := slices.Clone(got)
	got = save(append(got, creditRules(nil, 25, 2, []string{"logged_time", "commented", "approved"}, 4))...)
	if !reflect.DeepEqual(got, kept) {
		t.Fatalf("a save that changes no rule: %+v, want %+v", got, kept)
	}
	// Every sprint: one value rewritten in each snapshot, none added.
	every := slices.Clone(got)
	for i := range every {
		every[i].HoursPerPoint = 6
	}
	got = save(every...)
	if len(got) != 3 || got[0].HoursPerPoint != 6 || got[2].HoursPerPoint != 6 || *got[2].From != *kept[2].From {
		t.Fatalf("a change for every sprint: %+v", got)
	}
}

// Over generated saves, in the shapes the Credit group sends (a changed
// newest snapshot without its time, or a value set in every snapshot), no
// save changes or removes a snapshot it was not asked to: a change from
// this sprint keeps every stored snapshot as it was and adds one from the
// save unless its values are the newest's; a change for every sprint
// keeps every snapshot's time and sets the value in each.
func TestCreditSnapshotsKeepEveryRuleOverGeneratedSaves(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	var clock atomic.Int64
	clock.Store(m.now().UnixMilli())
	m.svc.now = func() time.Time { return time.UnixMilli(clock.Load()) }
	r := rand.New(rand.NewPCG(1, 2))
	values := func(x CreditRules) CreditRules {
		switch r.IntN(4) {
		case 0:
			x.ReviewShare = r.IntN(3) * 5
		case 1:
			x.HoursPerPoint = 1 + r.IntN(3)
		case 2:
			x.ReviewActions = slices.DeleteFunc(slices.Clone(creditActions), func(string) bool { return r.IntN(3) == 0 })
			if len(x.ReviewActions) == 0 {
				x.ReviewActions = []string{"approved"}
			}
		}
		x.ReviewNeed = min(x.ReviewNeed, len(x.ReviewActions))
		return x
	}
	stored := m.saveSetup(t, m.project(), Setup{Credit: &Credit{On: true, Rules: []CreditRules{creditRules(nil, 10, 1, creditActions, 4)}}}).Stored.Setup.Credit.Rules
	for step := range 120 {
		clock.Add(1 + r.Int64N(5000))
		before := stored
		if r.IntN(4) == 0 {
			hours := 1 + r.IntN(16)
			every := slices.Clone(before)
			for i := range every {
				every[i].HoursPerPoint = hours
			}
			stored = m.saveSetup(t, m.project(), Setup{Credit: &Credit{On: true, Rules: every}}).Stored.Setup.Credit.Rules
			for i := range stored {
				want := before[i]
				want.HoursPerPoint = hours
				if len(stored) != len(before) || !reflect.DeepEqual(stored[i], want) {
					t.Fatalf("step %d, every sprint at %d hours: %+v, want %+v with the hours", step, hours, stored, before)
				}
			}
			continue
		}
		next := values(before[len(before)-1])
		next.From = nil
		stored = m.saveSetup(t, m.project(), Setup{Credit: &Credit{On: true, Rules: append(slices.Clone(before), next)}}).Stored.Setup.Credit.Rules
		if !reflect.DeepEqual(stored[:len(before)], before) {
			t.Fatalf("step %d: the stored snapshots moved: %+v, were %+v", step, stored, before)
		}
		switch added := stored[len(before):]; {
		case sameCreditValues(next, before[len(before)-1]):
			if len(added) != 0 {
				t.Fatalf("step %d: a save with the newest's values added %+v", step, added)
			}
		case len(added) != 1 || *added[0].From != clock.Load() || !sameCreditValues(added[0], next):
			t.Fatalf("step %d: a change added %+v, want %+v from %d", step, added, next, clock.Load())
		}
	}
	if len(stored) < 10 {
		t.Fatalf("the generated saves left %d snapshots; the generator changes too little", len(stored))
	}
}
