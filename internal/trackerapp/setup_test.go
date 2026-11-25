package trackerapp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/project"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// setupPathOf is the setup file of project id.
func (m *mirrorFixture) setupPathOf(t *testing.T, id string) string {
	t.Helper()
	p, err := m.st.GetProject(id)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(project.ConfigDir(m.svc.configDir, p.Slug), setupFileName)
}

// writeSetupFile puts data in project id's setup file, as a person
// editing it by hand would.
func (m *mirrorFixture) writeSetupFile(t *testing.T, id, data string) string {
	t.Helper()
	path := m.setupPathOf(t, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// regrouped is a setup that puts the fixture's site's statuses in groups.
func (m *mirrorFixture) regrouped(groups map[string]string) Setup {
	return Setup{StatusGroups: []SiteStatusGroups{{Tracker: store.TrackerKindJira, Site: m.conn.Site, Statuses: groups}}}
}

// saveSetup saves setup for project id over what it stores now.
func (m *mirrorFixture) saveSetup(t *testing.T, id string, setup Setup) SetupSave {
	t.Helper()
	stored, err := m.svc.ProjectSetup(id)
	if err != nil {
		t.Fatal(err)
	}
	save, err := m.svc.SetProjectSetup(id, stored.Revision, setup)
	if err != nil {
		t.Fatal(err)
	}
	if !save.Saved {
		t.Fatalf("the save was refused: %s", save.Refused)
	}
	return save
}

// listIn makes id a second app project that lists the fixture's board.
func (m *mirrorFixture) listIn(t *testing.T, id string) {
	t.Helper()
	if _, err := m.st.CreateProject(store.Project{ID: id, Path: "/tmp/" + id, Name: id, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.st.ReplaceProjectTrackerBoards(id, nil, []store.ProjectTrackerBoard{{ConnectionID: m.conn.ID, BoardID: string(testBoard)}}); err != nil {
		t.Fatal(err)
	}
}

// subscribeShown subscribes project id to the fixture's board, showing
// the backlog, waits for the board's first read when the mirror held
// none, and hides it, so the pump of hidden handles ticks no more and
// every later event is one a test caused.
func (m *mirrorFixture) subscribeShown(t *testing.T, id string) BoardSubscription {
	t.Helper()
	sub, err := m.svc.SubscribeBoard(id, m.scope(), []Section{{Kind: SectionBacklog}}, true, untied)
	if err != nil {
		t.Fatal(err)
	}
	if !sub.Loaded && loadedOrEnded(t, m, sub.Seq) {
		t.Fatal("the board ended at its first read")
	}
	if err := m.svc.SetSubscriptionVisible(sub.ID, false); err != nil {
		t.Fatal(err)
	}
	return sub
}

// readAgainSince reports whether a board event of the fixture's board
// after the first n events says to read it again.
func (m *mirrorFixture) readAgainSince(n int) bool {
	boards, _, _ := m.events.snapshot()
	return slices.ContainsFunc(boards[n:], func(e BoardEvent) bool { return e.Scope == m.scope() && e.ReadAgain })
}

func (m *mirrorFixture) boardEventCount() int {
	boards, _, _ := m.events.snapshot()
	return len(boards)
}

// viewOf is the board as subscription sub reads it, with the backlog.
func (m *mirrorFixture) viewOf(t *testing.T, sub string) BoardView {
	t.Helper()
	scope, groups, err := m.svc.SubscriptionBoard(sub)
	if err != nil {
		t.Fatal(err)
	}
	return must[BoardView](t)(m.svc.Board(scope, groups, []Section{{Kind: SectionBacklog}}))
}

// A setup is read, saved over the revision it was read at and removed
// when emptied: the file is the setup's bytes, 0600, and its revision is
// their SHA-256; a save over another revision writes nothing and answers
// what is stored; a file edited by hand is what the next refused save
// shows; and an empty setup leaves no file. A list set to empty stays
// set, and one not set stays absent.
func TestProjectSetupSavesReadsAndRemoves(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	path := m.setupPathOf(t, m.project())
	empty, err := m.svc.ProjectSetup(m.project())
	if err != nil {
		t.Fatal(err)
	}
	if empty.Revision != "" || empty.StatusGroups == nil || len(empty.StatusGroups) != 0 || empty.TicketPins == nil || len(empty.TicketPins) != 0 {
		t.Fatalf("a project with no setup file: %+v", empty)
	}
	setup := m.regrouped(map[string]string{"s1": "done"})
	setup.TicketPins = []SitePins{{Tracker: store.TrackerKindJira, Site: m.conn.Site, Sections: &[]string{}}}
	save, err := m.svc.SetProjectSetup(m.project(), "", setup)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if !save.Saved || save.Refused != "" || save.Stored.Revision != hex.EncodeToString(sum[:]) {
		t.Fatalf("the first save %+v of the file %s", save, data)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the setup file's mode: %v, %v", info, err)
	}
	if !strings.Contains(string(data), `"sections": []`) || strings.Contains(string(data), `"fields"`) {
		t.Fatalf("the pinned lists set to empty and not set, as written:\n%s", data)
	}
	pins := save.Stored.TicketPins
	if len(pins) != 1 || pins[0].Fields != nil || pins[0].Sections == nil || len(*pins[0].Sections) != 0 {
		t.Fatalf("pins as stored: %+v", pins)
	}
	if got := must[SetupView](t)(m.svc.ProjectSetup(m.project())); got.Revision != save.Stored.Revision || len(got.StatusGroups) != 1 {
		t.Fatalf("the setup read after the save: %+v", got)
	}
	if n := m.events.setups.Load(); n != 1 {
		t.Fatalf("a save was told %d times, want 1", n)
	}

	// A save over the revision before the first is refused with what is
	// stored, and writes nothing.
	stale, err := m.svc.SetProjectSetup(m.project(), "", Setup{})
	if err != nil {
		t.Fatal(err)
	}
	if stale.Saved || stale.Refused != setupChangedSentence || stale.Stored.Revision != save.Stored.Revision {
		t.Fatalf("a save over a moved revision: %+v", stale)
	}
	if after, _ := os.ReadFile(path); string(after) != string(data) {
		t.Fatalf("a refused save wrote the file:\n%s", after)
	}
	if n := m.events.setups.Load(); n != 1 {
		t.Fatalf("a refused save that moved nothing was told: %d", n)
	}

	// A file edited by hand is what the next refused save answers, and
	// the change of what is held is told.
	edited := `{"status_groups": {"version": 1, "sites": [{"tracker": "jira", "site": "` + m.conn.Site + `", "statuses": {"s2": "left_out"}}]}}`
	m.writeSetupFile(t, m.project(), edited)
	refused := must[SetupSave](t)(m.svc.SetProjectSetup(m.project(), save.Stored.Revision, Setup{}))
	editedSum := sha256.Sum256([]byte(edited))
	if refused.Saved || refused.Stored.Revision != hex.EncodeToString(editedSum[:]) ||
		len(refused.Stored.StatusGroups) != 1 || refused.Stored.StatusGroups[0].Statuses["s2"] != "left_out" || len(refused.Stored.TicketPins) != 0 {
		t.Fatalf("a save over a file edited by hand: %+v", refused)
	}
	if n := m.events.setups.Load(); n != 2 {
		t.Fatalf("the file edited by hand was told %d times, want once more", n)
	}

	// An empty setup removes the file.
	cleared := must[SetupSave](t)(m.svc.SetProjectSetup(m.project(), refused.Stored.Revision, Setup{}))
	if !cleared.Saved || cleared.Stored.Revision != "" {
		t.Fatalf("an empty setup: %+v", cleared)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an empty setup left its file: %v", err)
	}
	if _, err := m.svc.SetProjectSetup("no-such-project", "", Setup{}); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("a save for no project: %v", err)
	}
}

// The setup is the file's: a restarted service reads it at its first use.
func TestProjectSetupSurvivesARestart(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	save := m.saveSetup(t, m.project(), m.regrouped(map[string]string{"s1": "done"}))
	m.restart(t)
	got := must[SetupView](t)(m.svc.ProjectSetup(m.project()))
	if got.Revision != save.Stored.Revision || len(got.StatusGroups) != 1 || got.StatusGroups[0].Statuses["s1"] != "done" {
		t.Fatalf("the setup after a restart: %+v", got)
	}
}

// A setup file that cannot be read is refused as what it is: its read,
// its save and its project's board subscribe fail with the path and why,
// the file is not overwritten, and another project listing the same
// board is not affected. Removing the file mends it at the next use.
func TestAnUnreadableSetupRefusesItsProjectOnly(t *testing.T) {
	t.Parallel()
	for name, data := range map[string]string{
		"not JSON":             "{not json",
		"not an object":        `[1, 2]`,
		"a group of no name":   `{"status_groups": {"version": 1, "sites": [{"tracker": "jira", "site": "x", "statuses": {"s1": "blocked"}}]}}`,
		"a field of no name":   `{"ticket_pins": {"version": 1, "sites": [], "extra": true}}`,
		"a version before one": `{"status_groups": {"version": 0, "sites": []}}`,
		"over the bound":       `{"x": "` + strings.Repeat("a", setupMaxBytes) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
			m.listIn(t, "other")
			path := m.writeSetupFile(t, m.project(), data)
			if _, err := m.svc.ProjectSetup(m.project()); !errors.Is(err, ErrSetupUnreadable) || !strings.Contains(err.Error(), path) {
				t.Fatalf("the read of an unreadable setup: %v", err)
			}
			if _, err := m.svc.SetProjectSetup(m.project(), "", Setup{}); !errors.Is(err, ErrSetupUnreadable) {
				t.Fatalf("a save over an unreadable setup: %v", err)
			}
			if after, _ := os.ReadFile(path); string(after) != data {
				t.Fatal("a save overwrote the unreadable setup file")
			}
			if _, err := m.svc.SubscribeBoard(m.project(), m.scope(), nil, true, untied); !errors.Is(err, ErrSetupUnreadable) {
				t.Fatalf("a subscribe of the project with the unreadable setup: %v", err)
			}
			if boards, _, _, _ := m.svc.SubscriptionCountsForTesting(); boards != 0 {
				t.Fatalf("a refused subscribe left %d board handles", boards)
			}
			other := m.subscribeShown(t, "other")
			if v := m.viewOf(t, other.ID); len(v.Sections[0].Rows) != 1 {
				t.Fatalf("the other project's board: %+v", v)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			m.subscribeShown(t, m.project())
		})
	}
}

// A section this build does not know, and one at a newer version, are
// kept byte for byte by a save and named in Newer; a save that sets a
// section a newer version saved is refused and writes nothing, and an
// empty save keeps the file for the sections it keeps.
func TestProjectSetupKeepsWhatANewerVersionSaved(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	marks := `{"version": 1,   "marks": [{"issue": "WEB-1"}]}`
	groups := `{"version": 2, "rules": ["any"]}`
	m.writeSetupFile(t, m.project(), `{"card_marks": `+marks+`, "status_groups": `+groups+`}`)
	stored := must[SetupView](t)(m.svc.ProjectSetup(m.project()))
	if !slices.Equal(stored.Newer, []string{"card_marks", "status_groups"}) || len(stored.StatusGroups) != 0 {
		t.Fatalf("a setup a newer version saved: %+v", stored)
	}
	pins := Setup{TicketPins: []SitePins{{Tracker: store.TrackerKindJira, Site: m.conn.Site, Fields: &[]string{"summary"}}}}
	save := m.saveSetup(t, m.project(), pins)
	data, err := os.ReadFile(m.setupPathOf(t, m.project()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"card_marks": `+marks) || !strings.Contains(string(data), `"status_groups": `+groups) ||
		!strings.Contains(string(data), `"ticket_pins"`) || !slices.Equal(save.Stored.Newer, stored.Newer) {
		t.Fatalf("the kept sections as written:\n%s", data)
	}
	if _, err := m.svc.SetProjectSetup(m.project(), save.Stored.Revision, m.regrouped(map[string]string{"s1": "done"})); !errors.Is(err, ErrSetupNewer) {
		t.Fatalf("a save of a section a newer version saved: %v", err)
	}
	if after, _ := os.ReadFile(m.setupPathOf(t, m.project())); string(after) != string(data) {
		t.Fatal("a refused save of a newer section wrote the file")
	}
	kept := m.saveSetup(t, m.project(), Setup{})
	after, err := os.ReadFile(m.setupPathOf(t, m.project()))
	if err != nil || kept.Stored.Revision == "" || !strings.Contains(string(after), `"card_marks": `+marks) || strings.Contains(string(after), "ticket_pins") {
		t.Fatalf("an empty save over kept sections: %v\n%s", err, after)
	}
}

// A save is checked whole before anything is written: each refusal names
// why, and no file is left.
func TestProjectSetupValidates(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	site := m.conn.Site
	jira := func(statuses map[string]string) Setup { return m.regrouped(statuses) }
	pinned := func(tracker string, fields, sections *[]string) Setup {
		return Setup{TicketPins: []SitePins{{Tracker: tracker, Site: site, Fields: fields, Sections: sections}}}
	}
	many := func(n int) map[string]string {
		out := map[string]string{}
		for i := range n {
			out[fmt.Sprint("s", i)] = "done"
		}
		return out
	}
	list := func(n int) *[]string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprint("f", i)
		}
		return &out
	}
	for name, c := range map[string]struct {
		setup Setup
		says  string
	}{
		"a tracker of no kind": {Setup{StatusGroups: []SiteStatusGroups{{Tracker: "tempo", Site: site, Statuses: map[string]string{"s1": "done"}}}}, "neither jira nor linear"},
		"an empty site":        {Setup{StatusGroups: []SiteStatusGroups{{Tracker: "jira", Site: " ", Statuses: map[string]string{"s1": "done"}}}}, "empty or over"},
		"a site twice": {Setup{StatusGroups: []SiteStatusGroups{
			{Tracker: "jira", Site: site, Statuses: map[string]string{"s1": "done"}}, {Tracker: "jira", Site: site, Statuses: map[string]string{"s2": "done"}},
		}}, "twice"},
		"an empty status":       {jira(map[string]string{"": "done"}), "status ID is empty"},
		"a group of no name":    {jira(map[string]string{"s1": "blocked"}), `group "blocked"`},
		"too many statuses":     {jira(many(setupMaxStatuses + 1)), "over 1000"},
		"Linear fields":         {pinned(store.TrackerKindLinear, &[]string{"priority"}, nil), "fixed"},
		"a field pinned twice":  {pinned(store.TrackerKindJira, &[]string{"summary", "summary"}, nil), "twice"},
		"too many fields":       {pinned(store.TrackerKindJira, list(setupMaxFields+1), nil), "over 200"},
		"too many sections":     {pinned(store.TrackerKindJira, nil, list(setupMaxSections+1)), "over 100"},
		"a field ID over bound": {pinned(store.TrackerKindJira, &[]string{strings.Repeat("f", setupMaxID+1)}, nil), "over 255 bytes"},
	} {
		_, err := m.svc.SetProjectSetup(m.project(), "", c.setup)
		if !errors.Is(err, ErrInvalidSetup) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(m.setupPathOf(t, m.project())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused setup left a file: %v", err)
	}
	// A site that sets nothing is dropped, and sites are kept in order.
	save := m.saveSetup(t, m.project(), Setup{StatusGroups: []SiteStatusGroups{
		{Tracker: "linear", Site: "https://linear.app/example/", Statuses: map[string]string{"x": "todo"}},
		{Tracker: "jira", Site: "b.example.com", Statuses: map[string]string{}},
		{Tracker: "jira", Site: "a.example.com", Statuses: map[string]string{"y": "canceled"}},
	}})
	if got := save.Stored.StatusGroups; len(got) != 2 || got[0].Site != "a.example.com" || got[1].Tracker != "linear" {
		t.Fatalf("the sites as stored: %+v", got)
	}
}

// A save that moves the status groups of the board's site reads the
// board again under the new groups: the groups set, changed and cleared
// each send ReadAgain and change what the backlog and the totals hold. A
// save that leaves the groups as they were (pins only) sends none.
func TestStatusGroupsSetChangedAndCleared(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	three := 3.0
	inSprint := row("10001", 1_799_000_000_000, "a")
	inSprint.Card.Sprints, inSprint.Estimate = []tracker.SprintID{"1"}, &three
	done := row("10003", 1_799_000_000_000, "c")
	done.Card.Status = "s3"
	m.tracks([]tracker.Row{inSprint, row("10002", 1_799_000_000_000, "b"), done, row("10004", 1_799_000_000_000, "d")})
	sub := m.subscribeShown(t, m.project())
	before := m.viewOf(t, sub.ID)
	if got := rowIssues(before.Sections[0].Rows); !slices.Equal(got, []string{"10002", "10004"}) || before.Groups != "" || before.All.Rows != 4 {
		t.Fatalf("the board before any groups: %v, %+v", got, before)
	}
	for _, step := range []struct {
		name    string
		groups  map[string]string
		backlog []string
		all     int
		leftOut []string
	}{
		// To Do counts as done: no To Do row is in the backlog.
		{"set", map[string]string{"s1": "done"}, nil, 4, nil},
		// To Do is left out: its rows are in no backlog and no total.
		{"changed", map[string]string{"s1": "left_out"}, nil, 1, []string{"s1"}},
		{"cleared", nil, []string{"10002", "10004"}, 4, nil},
	} {
		n := m.boardEventCount()
		setup := Setup{}
		if step.groups != nil {
			setup = m.regrouped(step.groups)
		}
		m.saveSetup(t, m.project(), setup)
		if !m.readAgainSince(n) {
			t.Fatalf("%s: the save sent the board no ReadAgain", step.name)
		}
		v := m.viewOf(t, sub.ID)
		if got := rowIssues(v.Sections[0].Rows); !slices.Equal(got, step.backlog) || v.All.Rows != step.all || v.Backlog.Rows != len(step.backlog) {
			t.Fatalf("%s: backlog %v, all %+v, backlog total %+v", step.name, got, v.All, v.Backlog)
		}
		if (v.Groups == "") != (step.groups == nil) || !slices.Equal(v.LeftOut, step.leftOut) {
			t.Fatalf("%s: groups version %q, left out %v", step.name, v.Groups, v.LeftOut)
		}
	}
	n := m.boardEventCount()
	m.saveSetup(t, m.project(), Setup{TicketPins: []SitePins{{Tracker: store.TrackerKindJira, Site: m.conn.Site, Fields: &[]string{"summary"}}}})
	if m.readAgainSince(n) {
		t.Fatal("a save of pins alone read the board again")
	}
}

// The worked example: four tickets of 3 points in a sprint, one of them
// in a status left out, show four cards, and the sprint, its column and
// the board count three tickets and 9 points.
func TestALeftOutStatusShowsItsCardsAndCountsNothing(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	three := 3.0
	var rows []tracker.Row
	for i, id := range []string{"10001", "10002", "10003", "10004"} {
		r := row(id, 1_799_000_000_000, id)
		r.Card.Sprints, r.Estimate = []tracker.SprintID{"1"}, &three
		if i == 3 {
			r.Card.Status = "s2"
		}
		rows = append(rows, r)
	}
	m.tracks(rows)
	m.saveSetup(t, m.project(), m.regrouped(map[string]string{"s2": "left_out"}))
	sub := m.subscribeShown(t, m.project())
	scope, groups, err := m.svc.SubscriptionBoard(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := must[BoardView](t)(m.svc.Board(scope, groups, []Section{{Kind: SectionSprint, Sprint: "1"}, {Kind: SectionColumn, Column: 1}}))
	nine := SectionTotal{Rows: 3, Estimate: 9}
	if len(v.Sections[0].Rows) != 4 || v.Sprints[0].Total != nine || v.All != nine || v.Columns[0].Total != nine {
		t.Fatalf("the sprint shows %d cards; sprint %+v, all %+v, columns %+v", len(v.Sections[0].Rows), v.Sprints[0].Total, v.All, v.Columns)
	}
	if !slices.Equal(v.LeftOut, []string{"s2"}) {
		t.Fatalf("the view names %v as left out, want s2", v.LeftOut)
	}
	if got := rowIssues(v.Sections[1].Rows); !slices.Equal(got, []string{"10004"}) || v.Columns[1].Total != (SectionTotal{}) {
		t.Fatalf("the left-out column shows %v and counts %+v", got, v.Columns[1].Total)
	}
}

// A save between two pages of one section refuses the second page and a
// patch of the sections the client holds: neither may mix two versions
// of the project's status groups. Read again, both carry the new version.
func TestASaveRefusesPagesReadWithOtherGroups(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	var rows []tracker.Row
	for i := range 150 {
		id := fmt.Sprintf("%05d", 40000+i)
		rows = append(rows, row(id, 1_799_000_000_000, id))
	}
	m.tracks(rows)
	sub := m.subscribeShown(t, m.project())
	backlog := Section{Kind: SectionBacklog}
	scope, groups, err := m.svc.SubscriptionBoard(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := must[BoardView](t)(m.svc.Board(scope, groups, []Section{backlog}))
	first := v.Sections[0]
	if first.Next == nil || first.Next.Groups != v.Groups {
		t.Fatalf("the first page's cursor %+v", first.Next)
	}
	held := TotalsAt{Lifetime: v.Lifetime, Revision: v.TotalsRevision, Groups: v.Groups}
	m.saveSetup(t, m.project(), m.regrouped(map[string]string{"s1": "in_progress"}))
	_, now, err := m.svc.SubscriptionBoard(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.svc.SectionRows(scope, now, backlog, first.Next); !errors.Is(err, ErrStalePage) {
		t.Fatalf("a page after a save of the groups: %v", err)
	}
	if _, err := m.svc.BoardPatch(scope, now, []Section{backlog}, ids("40000"), held); !errors.Is(err, ErrStalePage) {
		t.Fatalf("a patch after a save of the groups: %v", err)
	}
	again := must[BoardView](t)(m.svc.Board(scope, now, []Section{backlog}))
	if again.Groups == v.Groups || again.Sections[0].Next.Groups != again.Groups {
		t.Fatalf("the board read again: groups %q, cursor %+v", again.Groups, again.Sections[0].Next)
	}
	must[SectionPage](t)(m.svc.SectionRows(scope, now, backlog, again.Sections[0].Next))
	must[BoardPatch](t)(m.svc.BoardPatch(scope, now, []Section{backlog}, ids("40000"),
		TotalsAt{Lifetime: again.Lifetime, Revision: again.TotalsRevision, Groups: again.Groups}))
}

// Two projects that list one board read it with their own groups: one
// project's save changes only its own subscription's reads.
func TestTwoProjectsRegroupOneBoardApart(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a"), row("10002", 1_799_000_000_000, "b")})
	m.listIn(t, "other")
	mine := m.subscribeShown(t, m.project())
	theirs := m.subscribeShown(t, "other")
	m.saveSetup(t, m.project(), m.regrouped(map[string]string{"s1": "done"}))
	if v := m.viewOf(t, mine.ID); len(v.Sections[0].Rows) != 0 || v.Groups == "" {
		t.Fatalf("the regrouped project's backlog: %+v", v.Sections[0])
	}
	if v := m.viewOf(t, theirs.ID); len(v.Sections[0].Rows) != 2 || v.Groups != "" {
		t.Fatalf("the other project's backlog: %+v", v.Sections[0])
	}
}

// A board subscribe reads its project once, whether or not the project
// has a setup: the read that finds the board listed names the project's
// slug, which the setup is found by.
func TestASubscribeReadsItsProjectOnce(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	cs := newCountingStore(m.st)
	m.svc.store = cs
	for _, step := range []string{"no setup", "a setup"} {
		cs.take()
		sub, err := m.svc.SubscribeBoard(m.project(), m.scope(), nil, false, untied)
		if err != nil {
			t.Fatal(err)
		}
		calls := cs.take()
		if reads := calls["GetProject"] + calls["ListProjectTrackerBoards"] + calls["ProjectTrackerBoardListed"]; reads != 1 {
			t.Errorf("%s: a subscribe read its project %d times (%v); want once", step, reads, calls)
		}
		m.svc.ReleaseSubscription(sub.ID)
		m.saveSetup(t, m.project(), m.regrouped(map[string]string{"1": "done"}))
	}
}

// unsyncableDir makes dir refuse to be opened, so a sync of it fails,
// while a file in it can still be made, renamed and removed (write and
// search permission without read), until the test ends.
func unsyncableDir(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a directory is not synced on Windows")
	}
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if f, err := os.Open(dir); err == nil {
		_ = f.Close()
		t.Skip("this user opens a directory it may not read")
	}
}

// Clearing the setup syncs the directory the file was removed from, as a
// write does, so the removal survives a power loss: a sync that fails
// fails the save.
func TestClearingTheSetupSyncsItsDirectory(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	save := m.saveSetup(t, m.project(), m.regrouped(map[string]string{"s1": "done"}))
	path := m.setupPathOf(t, m.project())
	unsyncableDir(t, filepath.Dir(path))
	cleared, err := m.svc.SetProjectSetup(m.project(), save.Stored.Revision, Setup{})
	if err == nil || !strings.Contains(err.Error(), "sync dir") {
		t.Fatalf("a clear whose directory sync failed = %+v, %v; want the sync's error", cleared, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the clear left its file: %v", err)
	}
}

// fileSetupJSON is a setup file that puts the fixture's site's status s1
// in group.
func (m *mirrorFixture) fileSetupJSON(group string) string {
	return `{"status_groups": {"version": 1, "sites": [{"tracker": "jira", "site": "` + m.conn.Site + `", "statuses": {"s1": "` + group + `"}}]}}`
}

// A save that finds the file unreadable holds no setup, so every later use
// reads the file again: an open board's read and the setup's read fail as
// a first use does, and once the file is mended they take what it holds.
// The project's boards read again and the change is told.
func TestASaveThatFindsTheFileUnreadableHoldsNoSetup(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tracks([]tracker.Row{row("10002", 1_799_000_000_000, "b")})
	save := m.saveSetup(t, m.project(), m.regrouped(map[string]string{"s1": "done"}))
	sub := m.subscribeShown(t, m.project())
	held := m.viewOf(t, sub.ID).Groups
	m.writeSetupFile(t, m.project(), "{not json")
	n, told := m.boardEventCount(), m.events.setups.Load()
	if _, err := m.svc.SetProjectSetup(m.project(), save.Stored.Revision, m.regrouped(map[string]string{"s1": "todo"})); !errors.Is(err, ErrSetupUnreadable) {
		t.Fatalf("a save over an unreadable file = %v", err)
	}
	if !m.readAgainSince(n) || m.events.setups.Load() != told+1 {
		t.Fatalf("the save that found the file unreadable: read again %v, told %d times", m.readAgainSince(n), m.events.setups.Load()-told)
	}
	if _, err := m.svc.ProjectSetup(m.project()); !errors.Is(err, ErrSetupUnreadable) {
		t.Fatalf("the setup's read after = %v; want the file unreadable", err)
	}
	if _, _, err := m.svc.SubscriptionBoard(sub.ID); !errors.Is(err, ErrSetupUnreadable) {
		t.Fatalf("the open board's read after = %v; want the file unreadable", err)
	}

	mended := m.fileSetupJSON("left_out")
	m.writeSetupFile(t, m.project(), mended)
	sum := sha256.Sum256([]byte(mended))
	if got := must[SetupView](t)(m.svc.ProjectSetup(m.project())); got.Revision != hex.EncodeToString(sum[:]) {
		t.Fatalf("the mended file's setup = %+v", got)
	}
	if v := m.viewOf(t, sub.ID); v.Groups == "" || v.Groups == held || !slices.Equal(v.LeftOut, []string{"s1"}) {
		t.Fatalf("the open board after the file was mended: groups %q (held %q), left out %v", v.Groups, held, v.LeftOut)
	}
}

// A save whose write may have happened (its directory sync failed after
// the rename) fails saying so, and holds no setup: the next use reads the
// file, which here holds the save's setup. The project's boards read
// again and the change is told; a clear is the same.
func TestASaveWhoseSyncFailedHoldsWhatTheFileHolds(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tracks([]tracker.Row{row("10002", 1_799_000_000_000, "b")})
	save := m.saveSetup(t, m.project(), m.regrouped(map[string]string{"s1": "done"}))
	sub := m.subscribeShown(t, m.project())
	held := m.viewOf(t, sub.ID).Groups
	path := m.setupPathOf(t, m.project())
	unsyncableDir(t, filepath.Dir(path))

	n, told := m.boardEventCount(), m.events.setups.Load()
	_, err := m.svc.SetProjectSetup(m.project(), save.Stored.Revision, m.regrouped(map[string]string{"s1": "left_out"}))
	var unsure *SetupWriteError
	if !errors.As(err, &unsure) || unsure.Path != path || !strings.Contains(err.Error(), "sync dir") {
		t.Fatalf("a save whose sync failed = %v; want a SetupWriteError for %s", err, path)
	}
	if !m.readAgainSince(n) || m.events.setups.Load() != told+1 {
		t.Fatalf("the save whose sync failed: read again %v, told %d times", m.readAgainSince(n), m.events.setups.Load()-told)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(onDisk)
	got := must[SetupView](t)(m.svc.ProjectSetup(m.project()))
	if got.Revision != hex.EncodeToString(sum[:]) || got.Revision == save.Stored.Revision {
		t.Fatalf("the setup after = %+v; want the file's, revision %x", got, sum)
	}
	if v := m.viewOf(t, sub.ID); v.Groups == held || !slices.Equal(v.LeftOut, []string{"s1"}) {
		t.Fatalf("the open board after: groups %q (held %q), left out %v", v.Groups, held, v.LeftOut)
	}

	n, told = m.boardEventCount(), m.events.setups.Load()
	if _, err := m.svc.SetProjectSetup(m.project(), got.Revision, Setup{}); !errors.As(err, &unsure) {
		t.Fatalf("a clear whose sync failed = %v; want a SetupWriteError", err)
	}
	if !m.readAgainSince(n) || m.events.setups.Load() != told+1 {
		t.Fatalf("the clear whose sync failed: read again %v, told %d times", m.readAgainSince(n), m.events.setups.Load()-told)
	}
	if got := must[SetupView](t)(m.svc.ProjectSetup(m.project())); got.Revision != "" {
		t.Fatalf("the setup after the clear = %+v; want none, as the file is gone", got)
	}
	if v := m.viewOf(t, sub.ID); v.Groups != "" {
		t.Fatalf("the open board after the clear reads with groups %q", v.Groups)
	}
}
