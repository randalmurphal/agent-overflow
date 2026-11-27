package trackerapp

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerdash"
)

// openTickets is a widget an agent writes: the board's tickets, counted.
func openTickets() KeptWidget {
	return KeptWidget{Form: trackerdash.FormNumber, Width: 3, Metric: trackerdash.Metric{
		Title: "Tickets on the board", Words: []string{"Every ticket the board holds."}, SQL: "SELECT id AS ticket FROM tickets",
		Reduce: trackerdash.ReduceCount, Unit: trackerdash.UnitTickets,
	}}
}

// dashboardFixture is a mirror fixture whose board holds three tickets,
// subscribed for its project.
func dashboardFixture(t *testing.T) (*mirrorFixture, string) {
	t.Helper()
	m := newMirrorFixture(t)
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a"), row("10002", 1_799_000_000_000, "b"), row("10003", 1_799_000_000_000, "c")})
	return m, m.subscribeShown(t, m.project()).ID
}

// A widget reads a ticket's type, priority and labels as the board's read
// gave them: the copy decodes the blob mirrorCard wrote, by its shape
// (cardShape), and a label the read could not name counts by its ID.
func TestAWidgetReadsTypesPrioritiesAndLabelsAsTheReadGaveThem(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	bug := row("10001", 1_799_000_000_000, "a")
	bug.Card.Type = tracker.IssueType{ID: "10004", Name: "Bug"}
	bug.Card.Priority = tracker.Choice{ID: "2", Name: "High"}
	bug.Card.Labels = []tracker.Choice{{ID: "frontend", Name: "frontend"}, {ID: "lbl-9"}}
	m.tracks([]tracker.Row{bug, row("10002", 1_799_000_000_000, "b")})
	m.subscribeShown(t, m.project())
	w := openTickets()
	w.Metric.SQL = `SELECT t.id AS ticket, coalesce(t.type, '-') || '|' || coalesce(t.priority, '-') || '|' ||
	  coalesce((SELECT group_concat(name, ',') FROM (SELECT name FROM labels WHERE ticket = t.id ORDER BY name)), '-') AS note FROM tickets t`
	tried, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), w, TileMarks{}, "thread-1", false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range tried.Rows {
		got[r.ID] = r.Note
	}
	if want := map[string]string{"10001": "Bug|High|frontend,lbl-9", "10002": "-|-|-"}; !maps.Equal(got, want) {
		t.Fatalf("type|priority|labels by ticket = %v; want %v", got, want)
	}
}

// A widget that reads a type, a priority or labels through a join's
// USING or NATURAL counts what its ON twin counts: tried alone, kept on
// one dashboard beside the others, and drilled, each copy filled with what
// its query reads. The board holds one Bug, High, labeled ticket of three.
func TestAJoinCountsTheSameUsingNaturalOrOn(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	bug := row("10001", 1_799_000_000_000, "a")
	bug.Card.Type = tracker.IssueType{ID: "10004", Name: "Bug"}
	bug.Card.Priority = tracker.Choice{ID: "2", Name: "High"}
	bug.Card.Labels = []tracker.Choice{{ID: "frontend", Name: "frontend"}}
	task := row("10002", 1_799_000_000_000, "b")
	task.Card.Type = tracker.IssueType{ID: "10003", Name: "Task"}
	task.Card.Priority = tracker.Choice{ID: "3", Name: "Low"}
	m.tracks([]tracker.Row{bug, task, row("10003", 1_799_000_000_000, "c")})
	sub := m.subscribeShown(t, m.project()).ID
	queries := map[string]string{
		"type USING":       `WITH kinds(type) AS (VALUES ('Bug')) SELECT t.id AS ticket FROM tickets t JOIN kinds USING(type)`,
		"type ON":          `WITH kinds(k) AS (VALUES ('Bug')) SELECT t.id AS ticket FROM tickets t JOIN kinds ON kinds.k = t.type`,
		"priority NATURAL": `WITH w(priority) AS (VALUES ('High')) SELECT id AS ticket FROM tickets NATURAL JOIN w`,
		"priority ON":      `WITH w(p) AS (VALUES ('High')) SELECT t.id AS ticket FROM tickets t JOIN w ON w.p = t.priority`,
		"labels USING":     `WITH c AS (SELECT id AS ticket FROM tickets) SELECT c.ticket FROM c JOIN labels USING(ticket)`,
		"labels ON":        `SELECT t.id AS ticket FROM tickets t JOIN labels l ON l.ticket = t.id`,
	}
	names := slices.Sorted(maps.Keys(queries))
	widgets := map[string]string{} // widget ID to its query's name
	dashboard := ""
	for _, name := range names {
		w := openTickets()
		w.Metric.Title, w.Metric.SQL = name, queries[name]
		tried, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), w, TileMarks{}, "thread-1", true)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := numText(t, tried.Value.Value); got != `{"v":1}` {
			t.Errorf("%s tried: %s; want 1", name, got)
		}
		target := KeepTarget{Dashboard: dashboard}
		if dashboard == "" {
			target = KeepTarget{Title: "Joins"}
		}
		if dashboard, err = m.svc.KeepProposal(sub, tried.View.ID, target); err != nil {
			t.Fatalf("keep %s: %v", name, err)
		}
		widgets[tried.View.ID] = name
	}
	for _, v := range m.evaluate(t, sub, dashboard).Widgets {
		if got := numText(t, v.Value); got != `{"v":1}` {
			t.Errorf("%s kept: %s; want 1", widgets[v.ID], got)
		}
		d, err := m.svc.DashboardDrill(t.Context(), sub, trackerdash.DrillRequest{Request: trackerdash.Request{Dashboard: dashboard, TimeZone: "UTC"}, Widget: v.ID})
		if err != nil {
			t.Fatalf("drill %s: %v", widgets[v.ID], err)
		}
		if got := numText(t, &d.Value); got != `{"v":1}` || len(d.Rows) != 1 || d.Rows[0].ID != "10001" {
			t.Errorf("%s drilled: %s, rows %+v; want 1, 10001", widgets[v.ID], got, d.Rows)
		}
	}
}

// numText is n as the wire writes it, "null" for none.
func numText(t *testing.T, n *trackerdash.Num) string {
	t.Helper()
	data, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// propose shows w on the fixture's board for thread-1 and answers its
// proposal's ID.
func (m *mirrorFixture) propose(t *testing.T, w KeptWidget) string {
	t.Helper()
	tried, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), w, TileMarks{}, "thread-1", true)
	if err != nil {
		t.Fatal(err)
	}
	return tried.View.ID
}

// evaluate counts dashboard of subscription sub, the built-in when "".
func (m *mirrorFixture) evaluate(t *testing.T, sub, dashboard string) trackerdash.Answer {
	t.Helper()
	if dashboard == "" {
		dashboard = m.list(t, sub).Dashboards[0].ID
	}
	return must[trackerdash.Answer](t)(m.svc.EvaluateDashboard(t.Context(), sub, trackerdash.Request{Dashboard: dashboard, TimeZone: "UTC"}))
}

// list is the dashboards and proposals subscription sub shows.
func (m *mirrorFixture) list(t *testing.T, sub string) trackerdash.Listing {
	t.Helper()
	l, _, err := m.svc.Dashboards(t.Context(), sub)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// proposed is whether listing l holds proposal id.
func proposed(l trackerdash.Listing, id string) bool {
	return slices.ContainsFunc(l.Proposals, func(v trackerdash.ProposalView) bool { return v.View.ID == id })
}

func refusalCode(err error) string {
	if r, ok := errors.AsType[*trackerdash.Refusal](err); ok {
		return r.Code
	}
	return ""
}

// A widget tried without show is counted and joins nothing; shown, it is
// a proposal on every dashboard of the board, with the thread that
// proposed it, and the clients are told. Nothing reaches the setup file
// until a Keep.
func TestATriedWidgetIsCountedAndShownOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	tried, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), openTickets(), TileMarks{}, "thread-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if tried.Count != 3 || tried.Value.Value == nil || *tried.Value.Value.V != 3 || len(tried.Rows) != 3 || tried.Rows[0].Key == "" {
		t.Fatalf("tried = %+v", tried)
	}
	if l := m.list(t, sub); len(l.Proposals) != 0 || m.events.dashboards.Load() != 0 {
		t.Fatalf("a try without show proposed: %+v, told %d", l.Proposals, m.events.dashboards.Load())
	}

	id := m.propose(t, openTickets())
	if !isOwnID(id, widgetPrefix) || m.events.dashboards.Load() != 1 {
		t.Fatalf("proposal %q, told %d", id, m.events.dashboards.Load())
	}
	if ps := m.list(t, sub).Proposals; len(ps) != 1 || ps[0].View.ID != id || ps[0].Thread != "thread-1" || ps[0].View.How.SQL == "" {
		t.Fatalf("proposals = %+v", ps)
	}
	builtin := m.evaluate(t, sub, "")
	i := slices.IndexFunc(builtin.Widgets, func(v trackerdash.WidgetValue) bool { return v.ID == id })
	if i < 0 || *builtin.Widgets[i].Value.V != 3 {
		t.Fatalf("the proposal's value among %+v", builtin.Widgets)
	}
	if _, err := os.Stat(m.setupPathOf(t, m.project())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a proposal wrote the setup file: %v", err)
	}
}

// A widget whose query does not run is refused with its widget's
// sentence and proposes nothing; a board the project does not list is
// refused before anything is read; a board holding maxProposals refuses
// another show, and still counts a try.
func TestATriedWidgetIsRefusedWithItsReason(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	bad := openTickets()
	bad.Metric.SQL = "SELECT id AS ticket FROM tickets WHERE labels = 'x'"
	_, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), bad, TileMarks{}, "thread-1", true)
	if r, ok := errors.AsType[*trackerdash.Refusal](err); !ok || r.Code != "dashboard_widget_refused" || !strings.Contains(r.Sentence, "labels") {
		t.Fatalf("a query on a column the copy lacks: %v", err)
	}
	if l := m.list(t, sub); len(l.Proposals) != 0 {
		t.Fatalf("a refused widget was proposed: %+v", l.Proposals)
	}
	if _, err := m.svc.TryWidget(t.Context(), m.project(), Scope{Connection: m.conn.ID, Board: "5"}, openTickets(), TileMarks{}, "thread-1", true); !errors.Is(err, ErrNotListed) {
		t.Fatalf("a board the project does not list: %v", err)
	}
	for range maxProposals {
		m.propose(t, openTickets())
	}
	if _, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), openTickets(), TileMarks{}, "thread-1", true); refusalCode(err) != "dashboard_proposals_full" {
		t.Fatalf("a proposal past the cap: %v", err)
	}
	if _, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), openTickets(), TileMarks{}, "thread-1", false); err != nil {
		t.Fatalf("a try without show past the cap: %v", err)
	}
}

// Keep puts a proposal on a new dashboard, which the board lists after
// the built-in and counts with its widget, and the proposal goes; a
// second Keep of it is refused. Keep onto that dashboard appends, Dismiss
// drops, and each change is told once.
func TestKeepMovesAProposalOntoAKeptDashboard(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	first := m.propose(t, openTickets())
	if _, err := m.svc.KeepProposal(sub, first, KeepTarget{Title: "  "}); refusalCode(err) != "dashboard_title_invalid" {
		t.Fatalf("a Keep onto an unnamed dashboard: %v", err)
	}
	if _, err := m.svc.KeepProposal(sub, first, KeepTarget{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a Keep onto no dashboard: %v", err)
	}
	told := m.events.dashboards.Load()
	id, err := m.svc.KeepProposal(sub, first, KeepTarget{Title: " Bugs "})
	if err != nil {
		t.Fatal(err)
	}
	if n := m.events.dashboards.Load() - told; n != 1 {
		t.Fatalf("a Keep was told %d times; want 1", n)
	}
	list := m.list(t, sub)
	if ds := list.Dashboards; len(ds) != 2 || ds[1].ID != id || ds[1].Title != "Bugs" || len(ds[1].Widgets) != 1 || ds[1].Widgets[0].ID != first || len(list.Proposals) != 0 {
		t.Fatalf("dashboards = %+v", list)
	}
	kept := m.evaluate(t, sub, id)
	if len(kept.Widgets) != 1 || *kept.Widgets[0].Value.V != 3 {
		t.Fatalf("the kept dashboard = %+v", kept)
	}
	if _, err := m.svc.KeepProposal(sub, first, KeepTarget{Dashboard: id}); refusalCode(err) != "dashboard_proposal_gone" {
		t.Fatalf("a second Keep: %v", err)
	}

	second := m.propose(t, openTickets())
	if got, err := m.svc.KeepProposal(sub, second, KeepTarget{Dashboard: id}); err != nil || got != id {
		t.Fatalf("a Keep onto the dashboard: %q, %v", got, err)
	}
	third := m.propose(t, openTickets())
	if _, err := m.svc.KeepProposal(sub, third, KeepTarget{Dashboard: "d_0123"}); refusalCode(err) != "dashboard_gone" {
		t.Fatalf("a Keep onto a dashboard that is gone: %v", err)
	}
	told = m.events.dashboards.Load()
	if err := m.svc.DismissProposal(sub, third); err != nil {
		t.Fatal(err)
	}
	if err := m.svc.DismissProposal(sub, third); err != nil {
		t.Fatal(err)
	}
	if n := m.events.dashboards.Load() - told; n != 1 {
		t.Fatalf("two Dismisses were told %d times, want once", n)
	}
	if l := m.list(t, sub); len(l.Proposals) != 0 || len(l.Dashboards[1].Widgets) != 2 {
		t.Fatalf("after the second Keep and the Dismiss: %+v", l)
	}
}

// The kept dashboards are the setup file's: a restart keeps them and
// drops what was proposed; a save of the setup screen keeps them, an
// empty one included; Remove of a dashboard's last widget removes the
// dashboard, and again changes nothing and tells nothing.
func TestKeptDashboardsLiveInTheSetupFile(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	w := m.propose(t, openTickets())
	id := must[string](t)(m.svc.KeepProposal(sub, w, KeepTarget{Title: "Bugs"}))
	m.propose(t, openTickets())
	data, err := os.ReadFile(m.setupPathOf(t, m.project()))
	if err != nil || !strings.Contains(string(data), `"dashboards"`) || !strings.Contains(string(data), "SELECT id AS ticket FROM tickets") {
		t.Fatalf("the setup file: %s, %v", data, err)
	}

	m.restart(t)
	sub = m.subscribeShown(t, m.project()).ID
	if l := m.list(t, sub); len(l.Proposals) != 0 || len(l.Dashboards) != 2 || len(l.Dashboards[1].Widgets) != 1 || l.Dashboards[1].Widgets[0].ID != w {
		t.Fatalf("after a restart: %+v", l)
	}

	stored := must[SetupView](t)(m.svc.ProjectSetup(m.project()))
	m.saveSetup(t, m.project(), m.regrouped(map[string]string{"s1": "done"}))
	saved := must[SetupView](t)(m.svc.ProjectSetup(m.project()))
	if _, err := m.svc.SetProjectSetup(m.project(), saved.Revision, Setup{}); err != nil {
		t.Fatal(err)
	}
	if list := m.list(t, sub).Dashboards; len(list) != 2 || list[1].ID != id {
		t.Fatalf("after setup saves over %s: %+v", stored.Revision, list)
	}

	told := m.events.dashboards.Load()
	if err := m.svc.RemoveWidget(sub, id, w); err != nil {
		t.Fatal(err)
	}
	if err := m.svc.RemoveWidget(sub, id, w); err != nil {
		t.Fatal(err)
	}
	if n := m.events.dashboards.Load() - told; n != 1 {
		t.Fatalf("a Remove, twice, was told %d times; want 1", n)
	}
	if list := m.list(t, sub).Dashboards; len(list) != 1 {
		t.Fatalf("the dashboard outlived its last widget: %+v", list)
	}
	if _, err := os.Stat(m.setupPathOf(t, m.project())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the last dashboard's removal from an empty setup left its file: %v", err)
	}
}

// A dashboards section a newer version saved is kept and refuses a Keep
// with the setup's own refusal; a Keep that would take the file past its
// bound is refused saying what to remove, and the proposal stays.
func TestKeepRefusesANewerSectionAndAFullFile(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	newer := `{"dashboards": {"version": 2, "boards": "kept"}}`
	m.writeSetupFile(t, m.project(), newer)
	w := m.propose(t, openTickets())
	if _, err := m.svc.KeepProposal(sub, w, KeepTarget{Title: "Bugs"}); !errors.Is(err, ErrSetupNewer) {
		t.Fatalf("a Keep over a newer section: %v", err)
	}
	if data, _ := os.ReadFile(m.setupPathOf(t, m.project())); string(data) != newer {
		t.Fatalf("the newer section was rewritten: %s", data)
	}

	if err := os.Remove(m.setupPathOf(t, m.project())); err != nil {
		t.Fatal(err)
	}
	big := openTickets()
	big.Metric.SQL = "SELECT id AS ticket FROM tickets WHERE title <> '" + strings.Repeat("x", 15_000) + "'"
	var id string
	for {
		p := m.propose(t, big)
		target := KeepTarget{Dashboard: id}
		if id == "" {
			target = KeepTarget{Title: "Big"}
		}
		got, err := m.svc.KeepProposal(sub, p, target)
		if err != nil {
			if refusalCode(err) != "dashboard_full" {
				t.Fatalf("a Keep past the bound: %v", err)
			}
			if l := m.list(t, sub); !proposed(l, p) {
				t.Fatalf("a refused Keep dropped its proposal: %+v", l.Proposals)
			}
			break
		}
		id = got
	}
	if info, err := os.Stat(m.setupPathOf(t, m.project())); err != nil || info.Size() > setupMaxBytes {
		t.Fatalf("the full file: %v, %v", info, err)
	}
}

// A Keep is one change the clients are told of once: whoever reads the
// board when told finds the widget kept and no longer proposed, and a
// Dismiss of it then finds nothing to drop.
func TestAKeepIsToldOnceAndReadsSettled(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	p := m.propose(t, openTickets())
	type seen struct{ proposed, kept bool }
	var reads []seen
	hook := func() {
		l := m.list(t, sub)
		kept := len(l.Dashboards) == 2 && slices.ContainsFunc(l.Dashboards[1].Widgets, func(w trackerdash.WidgetView) bool { return w.ID == p })
		reads = append(reads, seen{proposed(l, p), kept})
	}
	m.events.onDashboards.Store(&hook)
	if _, err := m.svc.KeepProposal(sub, p, KeepTarget{Title: "Bugs"}); err != nil {
		t.Fatal(err)
	}
	m.events.onDashboards.Store(nil)
	if len(reads) != 1 || reads[0] != (seen{proposed: false, kept: true}) {
		t.Fatalf("what a reader found at each nudge of one Keep: %+v; want one, kept and not proposed", reads)
	}
}

// A count that overlaps a Keep counts the widget once, proposed or kept,
// never in neither place nor both: it reads the kept dashboards and the
// proposals under the proposals' lock, the order a Keep holds them in. The test holds
// that lock, waits for the count to block on it, and does a Keep's
// transition by hand.
func TestACountOverlappingAKeepFindsTheWidgetOnce(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	id := must[string](t)(m.svc.KeepProposal(sub, m.propose(t, openTickets()), KeepTarget{Title: "Bugs"}))
	q := m.propose(t, openTickets())
	scope, key, err := m.svc.boardHandle(sub)
	if err != nil {
		t.Fatal(err)
	}
	m.svc.propMu.Lock()
	unlock := sync.OnceFunc(m.svc.propMu.Unlock)
	defer unlock()
	type outcome struct {
		answer trackerdash.Answer
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		a, err := m.svc.EvaluateDashboard(t.Context(), sub, trackerdash.Request{Dashboard: id, TimeZone: "UTC"})
		done <- outcome{a, err}
	}()
	waitBlockedIn(t, "trackerapp.(*Service).ownOf", done)
	err = m.svc.keepLocked(proposalKey{key.slug, key.site, scope.Board}, key, string(scope.Board), q, KeepTarget{Dashboard: id}, id)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	c := <-done
	if c.err != nil {
		t.Fatal(c.err)
	}
	a := c.answer
	var counted int
	for _, w := range a.Widgets {
		if w.ID == q {
			counted++
		}
	}
	if counted != 1 {
		t.Fatalf("the count counted the widget %d times; want once", counted)
	}
}

// waitBlockedIn waits until a goroutine waits on a mutex inside fn, and
// fails if the one that should has already ended.
func waitBlockedIn[T any](t *testing.T, fn string, ended chan T) {
	t.Helper()
	buf := make([]byte, 1<<20)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		select {
		case v := <-ended:
			t.Fatalf("it ended before blocking in %s: %+v", fn, v)
		default:
		}
		for _, g := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
			if strings.Contains(g, "sync.(*Mutex).Lock") && strings.Contains(g, fn) {
				return
			}
		}
	}
	t.Fatalf("no goroutine blocked on a mutex in %s", fn)
}
