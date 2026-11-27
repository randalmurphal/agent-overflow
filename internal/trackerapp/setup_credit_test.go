package trackerapp

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
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
// snapshot takes effect at the start; a later one replaces the newest
// while no sprint of the project's boards completed after it took
// effect, taking its time, and otherwise takes effect from the save. A
// change for every sprint rewrites the snapshots and adds none.
func TestCreditRuleChangesStampTheirTime(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	all := []string{"commented", "approved", "logged_time"}
	save := func(rules ...CreditRules) []CreditRules {
		t.Helper()
		return m.saveSetup(t, m.project(), Setup{Credit: &Credit{On: true, Rules: rules}}).Stored.Setup.Credit.Rules
	}
	shares := func(rules []CreditRules) string {
		var out []string
		for _, r := range rules {
			out = append(out, time.UnixMilli(*r.From).UTC().Format(time.RFC3339)+"="+string(rune('0'+r.ReviewShare/5)))
		}
		return strings.Join(out, " ")
	}
	got := save(creditRules(nil, 10, 2, all, 4))
	if len(got) != 1 || *got[0].From != 0 {
		t.Fatalf("a first snapshot from the save: %+v", got)
	}
	// No sprint has completed: the change replaces the snapshot.
	got = save(append(got, creditRules(nil, 20, 2, all, 4))...)
	if len(got) != 1 || *got[0].From != 0 || got[0].ReviewShare != 20 {
		t.Fatalf("a change with no sprint completed: %s", shares(got))
	}
	// A sprint completes after it: the next change takes effect from the
	// save.
	completed := time.UnixMilli(1_799_500_000_000).UTC()
	m.p.set(func(p *scripted) {
		p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
			st := webStructure(b)
			st.Sprints = append(st.Sprints, tracker.Sprint{ID: "0", Board: b, Name: "Sprint 0", State: tracker.SprintClosed,
				Start: completed.Add(-14 * 24 * time.Hour), End: completed, Completed: completed})
			return st, nil
		}
	})
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
	m.subscribeShown(t, m.project())
	got = save(append(got, creditRules(nil, 25, 2, all, 4))...)
	if len(got) != 2 || *got[0].From != 0 || got[0].ReviewShare != 20 || *got[1].From != millis(m.now()) || got[1].ReviewShare != 25 {
		t.Fatalf("a change after a sprint completed: %s", shares(got))
	}
	from := *got[1].From
	// No sprint completed after that: the next change replaces it.
	got = save(append(got, creditRules(nil, 30, 2, all, 4))...)
	if len(got) != 2 || *got[1].From != from || got[1].ReviewShare != 30 || got[0].ReviewShare != 20 {
		t.Fatalf("a second change in the same sprint: %s", shares(got))
	}
	// Every sprint: one value rewritten in each snapshot, none added.
	every := slices.Clone(got)
	for i := range every {
		every[i].HoursPerPoint = 6
	}
	got = save(every...)
	if len(got) != 2 || got[0].HoursPerPoint != 6 || got[1].HoursPerPoint != 6 || *got[1].From != from {
		t.Fatalf("a change for every sprint: %+v", got)
	}
}
