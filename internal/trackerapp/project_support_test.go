package trackerapp

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// supportRows are four backlog tickets: 10001 of the support type and
// 10002 labeled support, neither estimated; 10003 estimated nothing and
// is no support ticket; 10004 is of the support type with an estimate.
func supportRows() []tracker.Row {
	three := 3.0
	typed := row("10001", 1_799_000_000_000, "a")
	typed.Card.Type = tracker.IssueType{ID: "10004"}
	labeled := row("10002", 1_799_000_000_000, "b")
	labeled.Card.Labels = []tracker.Choice{{ID: "support", Name: "support"}}
	estimated := row("10004", 1_799_000_000_000, "d")
	estimated.Card.Type, estimated.Estimate = tracker.IssueType{ID: "10004"}, &three
	return []tracker.Row{typed, labeled, row("10003", 1_799_000_000_000, "c"), estimated}
}

// creditOn is credit on with the default rules and hours hours of support
// a point, whose support tickets on the fixture's site are of type 10004
// or labeled support.
func (m *mirrorFixture) creditOn(on bool, hours int) Setup {
	rules := defaultCredit().Rules
	rules[0].HoursPerPoint = hours
	return Setup{Credit: &Credit{On: on, Rules: rules, Support: []SiteSupport{{Tracker: store.TrackerKindJira, Site: m.conn.Site,
		Types: []string{"10004"}, Labels: []string{"support"}}}}}
}

// supportOf is which rows of rows the board view flags as support.
func supportOf(rows []RowView) []string {
	var out []string
	for _, r := range rows {
		if r.Support {
			out = append(out, r.Card.ID)
		}
	}
	return out
}

// A save that turns credit on with a support rule reads the open board
// again: its support tickets are flagged on their rows and leave the
// backlog's count of tickets without an estimate, and a ticket of a
// support type with an estimate is no support ticket. A save of the hours
// for a point alone reads nothing again; turning credit off, its rule
// kept, restores the count.
func TestASupportRuleLeavesItsTicketsOutOfTheUnestimatedCount(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tracks(supportRows())
	sub := m.subscribeShown(t, m.project())
	if v := m.viewOf(t, sub.ID); v.Backlog.Unestimated != 3 || supportOf(v.Sections[0].Rows) != nil {
		t.Fatalf("before any rule: backlog %+v, support %v", v.Backlog, supportOf(v.Sections[0].Rows))
	}
	for _, step := range []struct {
		name        string
		setup       Setup
		readAgain   bool
		unestimated int
		support     []string
	}{
		{"credit on", m.creditOn(true, 4), true, 1, []string{"10001", "10002"}},
		{"the hours for a point", m.creditOn(true, 6), false, 1, []string{"10001", "10002"}},
		{"credit off", m.creditOn(false, 6), true, 3, nil},
	} {
		n := m.boardEventCount()
		m.saveSetup(t, m.project(), step.setup)
		if m.readAgainSince(n) != step.readAgain {
			t.Fatalf("%s: read the board again %v, want %v", step.name, !step.readAgain, step.readAgain)
		}
		v := m.viewOf(t, sub.ID)
		if got := supportOf(v.Sections[0].Rows); v.Backlog.Unestimated != step.unestimated || !slices.Equal(got, step.support) || v.Backlog.Rows != 4 {
			t.Fatalf("%s: backlog %+v, support %v; want %d without an estimate, support %v", step.name, v.Backlog, got, step.unestimated, step.support)
		}
	}
}

// The Time screen's line: the support tickets among the issues asked
// about, the newest hours for a point and the unit the board counts in;
// none with credit off, on a board estimated in time, through a Linear
// connection, or for an issue the mirror does not hold; a board the
// project does not list is refused.
func TestProjectSupportNamesTheSupportTicketsOfABoard(t *testing.T) {
	t.Parallel()
	asked := []string{"10001", "10002", "10003", "10004", "99999"}
	m := newMirrorFixture(t)
	// A sprint completed before the first save: a change of the hours
	// after it is a second snapshot, and the line follows the newest.
	completed := time.UnixMilli(1_799_500_000_000).UTC()
	m.p.set(func(p *scripted) {
		p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
			st := webStructure(b)
			st.Sprints = append(st.Sprints, tracker.Sprint{ID: "0", Board: b, Name: "Sprint 0", State: tracker.SprintClosed, End: completed, Completed: completed})
			return st, nil
		}
	})
	m.tracks(supportRows())
	m.subscribeShown(t, m.project())
	none := SupportView{Issues: []string{}}
	support := func() SupportView {
		t.Helper()
		return must[SupportView](t)(m.svc.ProjectSupport(m.project(), m.scope(), asked))
	}
	if got := support(); !reflect.DeepEqual(got, none) {
		t.Fatalf("no credit section: %+v", got)
	}
	m.saveSetup(t, m.project(), m.creditOn(false, 4))
	if got := support(); !reflect.DeepEqual(got, none) {
		t.Fatalf("credit off: %+v", got)
	}
	m.saveSetup(t, m.project(), m.creditOn(true, 4))
	got := support()
	slices.Sort(got.Issues)
	if !reflect.DeepEqual(got, SupportView{Issues: []string{"10001", "10002"}, HoursPerPoint: 4, Counts: "points"}) {
		t.Fatalf("credit on: %+v", got)
	}
	stored := must[SetupView](t)(m.svc.ProjectSetup(m.project())).Setup
	stored.Credit.Rules = append(stored.Credit.Rules, CreditRules{ReviewShare: 10, ReviewNeed: 2, ReviewActions: stored.Credit.Rules[0].ReviewActions, HoursPerPoint: 8})
	if rules := m.saveSetup(t, m.project(), stored).Stored.Setup.Credit.Rules; len(rules) != 2 {
		t.Fatalf("the change after a sprint completed kept %d snapshots", len(rules))
	}
	if got := support(); got.HoursPerPoint != 8 {
		t.Fatalf("after a second snapshot the line takes %d hours a point, want 8", got.HoursPerPoint)
	}
	// Counting tickets names the unit.
	stored = must[SetupView](t)(m.svc.ProjectSetup(m.project())).Setup
	stored.Estimates = &Estimates{Count: "tickets"}
	m.saveSetup(t, m.project(), stored)
	if got := support(); got.Counts != "none" || len(got.Issues) != 2 {
		t.Fatalf("counting tickets: %+v", got)
	}
	if got := must[SupportView](t)(m.svc.ProjectSupport(m.project(), m.scope(), []string{"99999"})); !reflect.DeepEqual(got, none) {
		t.Fatalf("an issue the mirror does not hold: %+v", got)
	}
	other := m.scope()
	other.Board = "999"
	if _, err := m.svc.ProjectSupport(m.project(), other, asked); !errors.Is(err, ErrNotListed) {
		t.Fatalf("a board the project does not list: %v", err)
	}
	if _, err := m.svc.ProjectSupport(m.project(), m.scope(), make([]string, maxSupportIssues+1)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("over the bound of issues: %v", err)
	}
}

// A board estimated in time shows no line: its hours are the estimate.
func TestProjectSupportOnABoardEstimatedInTimeIsNone(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.set(func(p *scripted) {
		p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
			st := webStructure(b)
			st.Estimate = tracker.Estimate{Unit: tracker.UnitTime}
			return st, nil
		}
	})
	m.tracks(supportRows())
	m.subscribeShown(t, m.project())
	m.saveSetup(t, m.project(), m.creditOn(true, 4))
	got := must[SupportView](t)(m.svc.ProjectSupport(m.project(), m.scope(), []string{"10001", "10002"}))
	if !reflect.DeepEqual(got, SupportView{Issues: []string{}}) {
		t.Fatalf("a board estimated in time: %+v", got)
	}
}

// A Linear board shows no line, whatever its project's setup holds.
func TestProjectSupportOnLinearIsNone(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	linear := m.addLinear()
	m.listBoards(linear.ID, "team")
	id := "boards-" + linear.ID
	setup := m.creditOn(true, 4)
	setup.Credit.Support = nil
	m.saveSetup(t, id, setup)
	got := must[SupportView](t)(m.svc.ProjectSupport(id, Scope{Connection: linear.ID, Board: "team"}, []string{"10001"}))
	if !reflect.DeepEqual(got, SupportView{Issues: []string{}}) {
		t.Fatalf("a Linear board: %+v", got)
	}
}
