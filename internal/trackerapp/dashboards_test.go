package trackerapp

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

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

// propose shows w on the fixture's board for thread-1 and answers its
// proposal's ID.
func (m *mirrorFixture) propose(t *testing.T, w KeptWidget) string {
	t.Helper()
	tried, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), w, "thread-1", true)
	if err != nil {
		t.Fatal(err)
	}
	return tried.View.ID
}

// evaluate counts dashboard of subscription sub, the built-in when "".
func (m *mirrorFixture) evaluate(t *testing.T, sub, dashboard string) trackerdash.Answer {
	t.Helper()
	if dashboard == "" {
		dashboard = must[[]trackerdash.DashboardView](t)(m.svc.Dashboards(t.Context(), sub))[0].ID
	}
	return must[trackerdash.Answer](t)(m.svc.EvaluateDashboard(t.Context(), sub, trackerdash.Request{Dashboard: dashboard, TimeZone: "UTC"}))
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
	tried, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), openTickets(), "thread-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if tried.Count != 3 || tried.Value.Value == nil || *tried.Value.Value.V != 3 || len(tried.Rows) != 3 || tried.Rows[0].Key == "" {
		t.Fatalf("tried = %+v", tried)
	}
	builtin := m.evaluate(t, sub, "")
	if len(builtin.Proposals) != 0 || m.events.dashboards.Load() != 0 {
		t.Fatalf("a try without show proposed: %+v, told %d", builtin.Proposals, m.events.dashboards.Load())
	}

	id := m.propose(t, openTickets())
	if !isOwnID(id, widgetPrefix) || m.events.dashboards.Load() != 1 {
		t.Fatalf("proposal %q, told %d", id, m.events.dashboards.Load())
	}
	builtin = m.evaluate(t, sub, builtin.Dashboard.ID)
	if len(builtin.Proposals) != 1 || builtin.Proposals[0].View.ID != id || builtin.Proposals[0].Thread != "thread-1" || builtin.Proposals[0].View.How.SQL == "" {
		t.Fatalf("proposals = %+v", builtin.Proposals)
	}
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
// another before it is counted.
func TestATriedWidgetIsRefusedWithItsReason(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	bad := openTickets()
	bad.Metric.SQL = "SELECT id AS ticket FROM tickets WHERE labels = 'x'"
	_, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), bad, "thread-1", true)
	if r, ok := errors.AsType[*trackerdash.Refusal](err); !ok || r.Code != "dashboard_widget_refused" || !strings.Contains(r.Sentence, "labels") {
		t.Fatalf("a query on a column the copy lacks: %v", err)
	}
	if a := m.evaluate(t, sub, ""); len(a.Proposals) != 0 {
		t.Fatalf("a refused widget was proposed: %+v", a.Proposals)
	}
	if _, err := m.svc.TryWidget(t.Context(), m.project(), Scope{Connection: m.conn.ID, Board: "5"}, openTickets(), "thread-1", true); !errors.Is(err, ErrNotListed) {
		t.Fatalf("a board the project does not list: %v", err)
	}
	for range maxProposals {
		m.propose(t, openTickets())
	}
	if _, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), openTickets(), "thread-1", true); refusalCode(err) != "dashboard_proposals_full" {
		t.Fatalf("a proposal past the cap: %v", err)
	}
	if _, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), openTickets(), "thread-1", false); err != nil {
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
	if n := m.events.dashboards.Load() - told; n != 2 {
		t.Fatalf("a Keep was told %d times; want the dashboards and the proposal's drop", n)
	}
	list := must[[]trackerdash.DashboardView](t)(m.svc.Dashboards(t.Context(), sub))
	if len(list) != 2 || list[1].ID != id || list[1].Title != "Bugs" || len(list[1].Widgets) != 1 || list[1].Widgets[0].ID != first {
		t.Fatalf("dashboards = %+v", list)
	}
	kept := m.evaluate(t, sub, id)
	if len(kept.Proposals) != 0 || len(kept.Widgets) != 1 || *kept.Widgets[0].Value.V != 3 {
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
		t.Fatalf("a Keep onto a deleted dashboard: %v", err)
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
	if a := m.evaluate(t, sub, id); len(a.Proposals) != 0 || len(a.Dashboard.Widgets) != 2 {
		t.Fatalf("after the second Keep and the Dismiss: %+v", a)
	}
}

// The kept dashboards are the setup file's: a restart keeps them and
// drops what was proposed; a save of the setup screen keeps them, an
// empty one included; Remove and Delete change them, and again change
// nothing and tell nothing.
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
	a := m.evaluate(t, sub, id)
	if len(a.Proposals) != 0 || len(a.Dashboard.Widgets) != 1 || a.Dashboard.Widgets[0].ID != w {
		t.Fatalf("after a restart: %+v", a)
	}

	stored := must[SetupView](t)(m.svc.ProjectSetup(m.project()))
	m.saveSetup(t, m.project(), m.regrouped(map[string]string{"s1": "done"}))
	saved := must[SetupView](t)(m.svc.ProjectSetup(m.project()))
	if _, err := m.svc.SetProjectSetup(m.project(), saved.Revision, Setup{}); err != nil {
		t.Fatal(err)
	}
	if list := must[[]trackerdash.DashboardView](t)(m.svc.Dashboards(t.Context(), sub)); len(list) != 2 || list[1].ID != id {
		t.Fatalf("after setup saves over %s: %+v", stored.Revision, list)
	}

	told := m.events.dashboards.Load()
	if err := m.svc.RemoveWidget(sub, id, w); err != nil {
		t.Fatal(err)
	}
	if err := m.svc.RemoveWidget(sub, id, w); err != nil {
		t.Fatal(err)
	}
	if list := must[[]trackerdash.DashboardView](t)(m.svc.Dashboards(t.Context(), sub)); len(list) != 2 || len(list[1].Widgets) != 0 {
		t.Fatalf("after Remove: %+v", list)
	}
	if err := m.svc.DeleteDashboard(sub, id); err != nil {
		t.Fatal(err)
	}
	if err := m.svc.DeleteDashboard(sub, id); err != nil {
		t.Fatal(err)
	}
	if n := m.events.dashboards.Load() - told; n != 2 {
		t.Fatalf("a Remove and a Delete, each twice, were told %d times; want 2", n)
	}
	if list := must[[]trackerdash.DashboardView](t)(m.svc.Dashboards(t.Context(), sub)); len(list) != 1 {
		t.Fatalf("after Delete: %+v", list)
	}
	if _, err := os.Stat(m.setupPathOf(t, m.project())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the last dashboard's delete of an empty setup left its file: %v", err)
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
			if a := m.evaluate(t, sub, ""); !slices.ContainsFunc(a.Proposals, func(v trackerdash.ProposalView) bool { return v.View.ID == p }) {
				t.Fatalf("a refused Keep dropped its proposal: %+v", a.Proposals)
			}
			break
		}
		id = got
	}
	if info, err := os.Stat(m.setupPathOf(t, m.project())); err != nil || info.Size() > setupMaxBytes {
		t.Fatalf("the full file: %v, %v", info, err)
	}
}
