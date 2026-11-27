package trackerapp

import (
	"encoding/json"
	"errors"
	"maps"
	"math"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerdash"
)

// tiles is the summary row the board subscription sub's listing answers.
func (m *mirrorFixture) tiles(t *testing.T, sub string) BoardTilesView {
	t.Helper()
	_, tiles, err := m.svc.Dashboards(t.Context(), sub)
	if err != nil {
		t.Fatal(err)
	}
	return tiles
}

// pinsOf is the pins the board's read gives subscription sub.
func (m *mirrorFixture) pinsOf(t *testing.T, sub string) []string {
	t.Helper()
	return must[BoardView](t)(m.svc.SubscriptionBoardView(sub, nil)).Pins
}

// readAgains counts the events that told the board's clients to read it
// again.
func (m *mirrorFixture) readAgains() int {
	n := 0
	boards, _, _ := m.events.snapshot()
	for _, e := range boards {
		if e.Scope == m.scope() && e.ReadAgain {
			n++
		}
	}
	return n
}

// valueIn is a limit's change to v, a number counted in estimates.
func valueIn(v float64, estimates string) *LimitChange {
	return &LimitChange{Value: &v, Estimates: estimates}
}

// keptWidget keeps an agent's number widget on a new dashboard and
// answers its ID.
func (m *mirrorFixture) keptWidget(t *testing.T, sub string) string {
	t.Helper()
	id := m.propose(t, openTickets())
	must[string](t)(m.svc.KeepProposal(sub, id, KeepTarget{Title: "Mine"}))
	return id
}

// setupFile is the fixture's project's setup file, by section.
func (m *mirrorFixture) setupFile(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(m.setupPathOf(t, m.project()))
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]json.RawMessage
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	return file
}

// A number widget's limit and a widget's pin, a built-in's or a kept
// one's, are the setup file's board_tiles section: the limits listed with
// the dashboards, the pins in the board's read, kept over a restart and
// over a save of the setup screen. A limit's change is told as a change
// of the dashboards that counts nothing again; a pin's is not told there.
// Remove takes the widget's pin and limit with it.
func TestBoardTilesLiveInTheirOwnSection(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	if tiles := m.tiles(t, sub); tiles.Limits == nil || tiles.Proposed == nil || tiles.Max != maxPins {
		t.Fatalf("a board with no tiles lists %+v; want each empty, none absent, and the row's room", tiles)
	}
	w := m.keptWidget(t, sub)
	told := m.boardEventCount()
	limit := valueIn(2, "points")
	if err := m.svc.SetWidgetLimit(t.Context(), sub, w, limit); err != nil {
		t.Fatal(err)
	}
	if err := m.svc.SetWidgetLimit(t.Context(), sub, w, limit); err != nil {
		t.Fatal(err)
	}
	if got := m.dashboardsSince(told); !slices.Equal(got, []ProjectDashboards{{Project: m.project()}}) {
		t.Fatalf("a limit set twice was told %+v; want once, of the project, counting nothing again", got)
	}
	told = m.boardEventCount()
	if err := m.svc.PinWidget(t.Context(), sub, "stuck", true); err != nil {
		t.Fatal(err)
	}
	if err := m.svc.PinWidget(t.Context(), sub, w, true); err != nil {
		t.Fatal(err)
	}
	if got := m.dashboardsSince(told); len(got) != 0 {
		t.Fatalf("two pins were told as a change of the dashboards: %+v", got)
	}
	want := BoardTilesView{Limits: map[string]TileLimit{w: {2, "tickets"}}, Proposed: []string{}, Max: maxPins}
	if got := m.tiles(t, sub); !tilesEqual(got, want) {
		t.Fatalf("the tiles = %+v; want %+v", got, want)
	}
	pins := []string{"stuck", w}
	if got := m.pinsOf(t, sub); !slices.Equal(got, pins) {
		t.Fatalf("the board's read pins %v; want %v", got, pins)
	}

	// The section is its own: the dashboards section holds no tile, so an
	// older build, which refuses a field it does not know there, reads it.
	file := m.setupFile(t)
	var dashboards dashboardsV1
	if err := decodeStrict(file[sectionDashboards], &dashboards); err != nil {
		t.Fatalf("the dashboards section as an older build reads it: %v", err)
	}
	var tiles boardTilesV1
	if err := decodeStrict(file[sectionBoardTiles], &tiles); err != nil || len(tiles.Boards) != 1 || tiles.Version != setupSectionVersion {
		t.Fatalf("the board_tiles section = %s, %v", file[sectionBoardTiles], err)
	}
	if got := tiles.Boards[0].Limits[w]; got != (TileLimit{Value: 2, Unit: "tickets"}) {
		t.Fatalf("the limit as stored = %+v; want 2 in the tickets its number counts", got)
	}

	m.saveSetup(t, m.project(), m.regrouped(map[string]string{"s1": "done"}))
	m.restart(t)
	sub = m.subscribeShown(t, m.project()).ID
	if got := m.tiles(t, sub); !tilesEqual(got, want) {
		t.Fatalf("after a setup save and a restart the tiles = %+v; want %+v", got, want)
	}
	if got := m.pinsOf(t, sub); !slices.Equal(got, pins) {
		t.Fatalf("after a setup save and a restart the pins = %v; want %v", got, pins)
	}

	if err := m.svc.SetWidgetLimit(t.Context(), sub, w, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.svc.PinWidget(t.Context(), sub, "stuck", false); err != nil {
		t.Fatal(err)
	}
	if err := m.svc.SetWidgetLimit(t.Context(), sub, w, limit); err != nil {
		t.Fatal(err)
	}
	dashboard := m.list(t, sub).Dashboards[1].ID
	if err := m.svc.RemoveWidget(sub, dashboard, w); err != nil {
		t.Fatal(err)
	}
	if got, pins := m.tiles(t, sub), m.pinsOf(t, sub); len(pins) != 0 || len(got.Limits) != 0 {
		t.Fatalf("after Remove the tiles = %+v, the pins %v", got, pins)
	}
	if _, ok := m.setupFile(t)[sectionBoardTiles]; ok {
		t.Fatal("a board with no tiles left keeps the section")
	}
}

// A limit is a number widget's and a finite number; the row takes five,
// each once; a widget the board's dashboards do not hold takes neither.
// The request a row counts names at most five widgets.
func TestBoardTilesRefuseWhatTheRowCannotHold(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	if err := m.svc.SetWidgetLimit(t.Context(), sub, "stuck_list", valueIn(3, "points")); refusalCode(err) != "dashboard_limit_not_number" {
		t.Fatalf("a limit on a list: %v", err)
	}
	if err := m.svc.SetWidgetLimit(t.Context(), sub, "stuck", valueIn(math.NaN(), "points")); refusalCode(err) != "dashboard_limit_invalid" {
		t.Fatalf("a limit that is not a number: %v", err)
	}
	for _, c := range []LimitChange{{}, {Step: 2}, {Step: -2, Value: new(float64), Estimates: "points"}} {
		if err := m.svc.SetWidgetLimit(t.Context(), sub, "stuck", &c); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("a limit's change %+v: %v", c, err)
		}
	}
	if err := m.svc.PinWidget(t.Context(), sub, "w_0123456789abcdef", true); refusalCode(err) != "dashboard_widget_gone" {
		t.Fatalf("a pin of a widget the board does not hold: %v", err)
	}
	row := []string{"pace", "start_to_done", "stuck", "finished", "carrying"}
	for _, id := range row {
		if err := m.svc.PinWidget(t.Context(), sub, id, true); err != nil {
			t.Fatalf("pin %s: %v", id, err)
		}
	}
	if err := m.svc.PinWidget(t.Context(), sub, "stuck_list", true); refusalCode(err) != "dashboard_row_full" {
		t.Fatalf("a sixth pin: %v", err)
	}
	if err := m.svc.PinWidget(t.Context(), sub, "stuck", true); err != nil {
		t.Fatal(err)
	}
	if got := m.pinsOf(t, sub); !slices.Equal(got, row) {
		t.Fatalf("the row = %v", got)
	}
	request := trackerdash.Request{Dashboard: m.list(t, sub).Dashboards[0].ID, TimeZone: "UTC", Widgets: append(slices.Clone(row), "stuck_list")}
	if _, err := m.svc.EvaluateDashboard(t.Context(), sub, request); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a row request of six widgets: %v", err)
	}
	if _, err := m.svc.DashboardDrill(t.Context(), sub, trackerdash.DrillRequest{Request: request, Widget: "stuck", IDs: true}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a drill of a row request of six widgets: %v", err)
	}
}

// A pin or a limit is checked against the board as the edit writes it: a
// widget the project's file no longer holds (removed from another window,
// whose write this service has not read yet) is refused, and no tile is
// left for it.
func TestATileWriteChecksTheBoardAsItIsWritten(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	w := m.keptWidget(t, sub)
	file := m.setupFile(t)
	delete(file, sectionDashboards)
	m.writeSetupFile(t, m.project(), string(must[[]byte](t)(json.Marshal(file))))
	if err := m.svc.PinWidget(t.Context(), sub, w, true); refusalCode(err) != "dashboard_widget_gone" {
		t.Fatalf("a pin of a widget removed beside it: %v", err)
	}
	if err := m.svc.SetWidgetLimit(t.Context(), sub, w, valueIn(1, "points")); refusalCode(err) != "dashboard_widget_gone" {
		t.Fatalf("a limit of a widget removed beside it: %v", err)
	}
	if _, ok := m.setupFile(t)[sectionBoardTiles]; ok {
		t.Fatal("a tile was written for a widget the file no longer holds")
	}
}

// A tile a file holds for a widget the board no longer has (an older
// build's, or a hand's) is not shown and holds no place: the row takes
// five live tiles, and the next write drops it.
func TestATileOfAWidgetGoneHoldsNoPlace(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
	m.writeSetupFile(t, m.project(), `{"board_tiles": {"version": 1, "boards": [{"tracker": "jira", "site": "`+m.conn.Site+`", "board": "`+string(testBoard)+`",`+
		` "pins": ["pace", "start_to_done", "w_dead", "stuck", "finished"], "limits": {"w_dead": {"value": 1, "unit": "tickets"}}}]}}`)
	sub := m.subscribeShown(t, m.project()).ID
	live := []string{"pace", "start_to_done", "stuck", "finished"}
	if got := m.pinsOf(t, sub); !slices.Equal(got, live) {
		t.Fatalf("the pins = %v; want the live ones %v", got, live)
	}
	if got := m.tiles(t, sub).Limits; len(got) != 0 {
		t.Fatalf("the limits = %v; want none", got)
	}
	if err := m.svc.PinWidget(t.Context(), sub, "carrying", true); err != nil {
		t.Fatalf("a fifth live pin beside a dead one: %v", err)
	}
	var tiles boardTilesV1
	if err := json.Unmarshal(m.setupFile(t)[sectionBoardTiles], &tiles); err != nil {
		t.Fatal(err)
	}
	if got := tiles.Boards[0]; !slices.Equal(got.Pins, append(live, "carrying")) || len(got.Limits) != 0 {
		t.Fatalf("the tiles as written = %+v", got)
	}
}

// A limit applies in the unit its number was in when it was set: a
// project that changes what it counts in shows it no more, and shows it
// again when it counts that way again.
func TestALimitAppliesInTheUnitItWasSetIn(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	if err := m.svc.SetWidgetLimit(t.Context(), sub, "pace", valueIn(8, "points")); err != nil {
		t.Fatal(err)
	}
	if got := m.tiles(t, sub).Limits; !maps.Equal(got, map[string]TileLimit{"pace": {8, "points"}}) {
		t.Fatalf("set in points, the limits = %v", got)
	}
	m.saveSetup(t, m.project(), Setup{Estimates: &Estimates{Count: "tickets"}})
	if got := m.tiles(t, sub).Limits; len(got) != 0 {
		t.Fatalf("counting tickets, the limits = %v; want none", got)
	}
	m.saveSetup(t, m.project(), Setup{})
	if got := m.tiles(t, sub).Limits; !maps.Equal(got, map[string]TileLimit{"pace": {8, "points"}}) {
		t.Fatalf("counting points again, the limits = %v", got)
	}
}

// An agent's widget carries the limit and the pin it asked for: listed on
// its proposal, and given to its tile by Keep, the pin only while the row
// has room. A limit on a widget that is not a number is refused.
func TestKeepGivesAProposalTheTileItsAgentAskedFor(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	limit := 2.0
	list := openTickets()
	list.Form = trackerdash.FormList
	if _, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), list, TileMarks{Limit: &limit}, "thread-1", true); refusalCode(err) != "dashboard_limit_not_number" {
		t.Fatalf("a list proposed with a limit: %v", err)
	}
	// A sum of points: Keep stores its limit in points, the unit its
	// number is in, so the tile shows it.
	sum := openTickets()
	sum.Metric.Title, sum.Metric.SQL = "Estimate on the board", `SELECT id AS ticket, points AS value FROM tickets`
	sum.Metric.Reduce, sum.Metric.Unit = trackerdash.ReduceSum, trackerdash.UnitEstimate
	tried, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), sum, TileMarks{Limit: &limit, Pin: true}, "thread-1", true)
	if err != nil {
		t.Fatal(err)
	}
	id := tried.View.ID
	want := BoardTilesView{Limits: map[string]TileLimit{id: {2, "points"}}, Proposed: []string{id}, Max: maxPins}
	if got := m.tiles(t, sub); !tilesEqual(got, want) {
		t.Fatalf("the proposal's tiles = %+v; want %+v", got, want)
	}
	must[string](t)(m.svc.KeepProposal(sub, id, KeepTarget{Title: "Mine"}))
	want = BoardTilesView{Limits: map[string]TileLimit{id: {2, "points"}}, Proposed: []string{}, Max: maxPins}
	if got, pins := m.tiles(t, sub), m.pinsOf(t, sub); !tilesEqual(got, want) || !slices.Equal(pins, []string{id}) {
		t.Fatalf("kept, the tiles = %+v and the pins %v; want %+v and %v", got, pins, want, id)
	}

	for _, b := range []string{"pace", "start_to_done", "stuck", "finished"} {
		if err := m.svc.PinWidget(t.Context(), sub, b, true); err != nil {
			t.Fatal(err)
		}
	}
	more, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), openTickets(), TileMarks{Pin: true}, "thread-1", true)
	if err != nil {
		t.Fatal(err)
	}
	must[string](t)(m.svc.KeepProposal(sub, more.View.ID, KeepTarget{Title: "More"}))
	if got := m.pinsOf(t, sub); len(got) != maxPins || slices.Contains(got, more.View.ID) {
		t.Fatalf("a Keep onto a full row pinned: %v", got)
	}
	if !slices.ContainsFunc(m.list(t, sub).Dashboards, func(d trackerdash.DashboardView) bool { return d.Title == "More" }) {
		t.Fatal("a Keep onto a full row did not keep the widget")
	}
}

// A pin is told on the board's event as the pinning project's pins, which
// that project's subscribers hold with no read; nothing reads the board
// again, and another project listing the board keeps its own pins.
func TestAPinIsToldOnTheBoardEventOfItsProject(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	m.listIn(t, "other")
	theirs := m.subscribeShown(t, "other").ID
	before := m.readAgains()
	if err := m.svc.PinWidget(t.Context(), sub, "stuck", true); err != nil {
		t.Fatal(err)
	}
	var told []ProjectPins
	m.events.waitFor(t, "the pins on the board's event", func(boards []BoardEvent, _ []IssueEvent, _ []error) bool {
		told = nil
		for _, e := range boards {
			if e.Scope == m.scope() && e.Pins != nil {
				told = append(told, *e.Pins)
			}
		}
		return len(told) > 0
	})
	if len(told) != 1 || told[0].Project != m.project() || !slices.Equal(told[0].Pins, []string{"stuck"}) {
		t.Fatalf("the pins told = %+v; want the project's [stuck], once", told)
	}
	if n := m.readAgains() - before; n != 0 {
		t.Fatalf("a pin read the board again %d times", n)
	}
	if got := m.pinsOf(t, theirs); len(got) != 0 {
		t.Fatalf("the other project's pins = %v; want none", got)
	}
}

func tilesEqual(a, b BoardTilesView) bool {
	return a.Max == b.Max && slices.Equal(a.Proposed, b.Proposed) && maps.Equal(a.Limits, b.Limits)
}

// timeDashboardFixture is dashboardFixture of a board whose estimates are
// time.
func timeDashboardFixture(t *testing.T) (*mirrorFixture, string) {
	t.Helper()
	m := newMirrorFixture(t)
	m.p.set(func(p *scripted) {
		p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
			st := webStructure(b)
			st.Estimate = tracker.Estimate{Unit: tracker.UnitTime}
			return st, nil
		}
	})
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
	return m, m.subscribeShown(t, m.project()).ID
}

// A limit is stored in the unit the number it was set from was counted
// in: a value counted in other estimates than the board's now (a count
// that answered before the project changed what it counts in) is
// refused, and nothing is stored.
func TestALimitIsSetOnlyFromACountInTheBoardsUnit(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	if err := m.svc.SetWidgetLimit(t.Context(), sub, "pace", valueIn(3600, "time")); refusalCode(err) != "dashboard_limit_unit" {
		t.Fatalf("a value counted in time on a board of points: %v", err)
	}
	if err := m.svc.SetWidgetLimit(t.Context(), sub, "pace", &LimitChange{Value: new(4.0), Estimates: "none", Step: 1}); refusalCode(err) != "dashboard_limit_unit" {
		t.Fatalf("a step from a value counted in tickets on a board of points: %v", err)
	}
	if _, err := os.Stat(m.setupPathOf(t, m.project())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused limit was written: %v", err)
	}
	m.saveSetup(t, m.project(), Setup{Estimates: &Estimates{Count: "tickets"}})
	if err := m.svc.SetWidgetLimit(t.Context(), sub, "pace", valueIn(8, "points")); refusalCode(err) != "dashboard_limit_unit" {
		t.Fatalf("a value counted in points once the project counts tickets: %v", err)
	}
	if err := m.svc.SetWidgetLimit(t.Context(), sub, "pace", valueIn(8, "none")); err != nil {
		t.Fatal(err)
	}
	if got := m.tiles(t, sub).Limits; !maps.Equal(got, map[string]TileLimit{"pace": {8, "tickets"}}) {
		t.Fatalf("counting tickets, the limits = %v", got)
	}
}

// A Raise or a Lower moves the limit stored when it is written by one
// step of its unit: an hour of seconds, half a day, 1 otherwise; so each
// accepted press moves it a step from whatever another window set, never
// below 0. With no limit stored in the widget's unit now, a step starts
// from the number the count answered, and without one it is refused.
func TestALimitStepsFromTheLimitStored(t *testing.T) {
	t.Parallel()
	m, sub := timeDashboardFixture(t)
	step := func(widget string, by int, from *LimitChange) {
		t.Helper()
		c := LimitChange{Step: by}
		if from != nil {
			c.Value, c.Estimates = from.Value, from.Estimates
		}
		if err := m.svc.SetWidgetLimit(t.Context(), sub, widget, &c); err != nil {
			t.Fatalf("a step of %s by %d from %+v: %v", widget, by, from, err)
		}
	}
	limits := func() map[string]TileLimit { return m.tiles(t, sub).Limits }
	if err := m.svc.SetWidgetLimit(t.Context(), sub, "pace", &LimitChange{Step: 1}); refusalCode(err) != "dashboard_limit_start" {
		t.Fatalf("a step with no limit and no number: %v", err)
	}
	// One second short of an hour, as a count may answer: a step adds an
	// hour to it.
	step("pace", 1, valueIn(3599, "time"))
	if got := limits()["pace"]; got != (TileLimit{7199, "seconds"}) {
		t.Fatalf("a Raise from the number counted = %+v; want an hour more, in seconds", got)
	}
	if err := m.svc.SetWidgetLimit(t.Context(), sub, "pace", valueIn(3600, "time")); err != nil {
		t.Fatal(err)
	}
	step("pace", 1, nil)
	// Another window that still shows no limit steps from what is stored,
	// not from the number it sends.
	step("pace", 1, valueIn(5, "time"))
	if got := limits()["pace"]; got != (TileLimit{3 * 3600, "seconds"}) {
		t.Fatalf("two Raises of an hour = %+v; want three hours", got)
	}
	step("pace", -1, nil)
	if got := limits()["pace"]; got != (TileLimit{2 * 3600, "seconds"}) {
		t.Fatalf("a Lower = %+v; want two hours", got)
	}
	if err := m.svc.SetWidgetLimit(t.Context(), sub, "pace", valueIn(1800, "time")); err != nil {
		t.Fatal(err)
	}
	step("pace", -1, nil)
	if got := limits()["pace"]; got != (TileLimit{0, "seconds"}) {
		t.Fatalf("a Lower past 0 = %+v; want 0", got)
	}
	step("start_to_done", 1, valueIn(2, "time"))
	step("start_to_done", 1, nil)
	if got := limits()["start_to_done"]; got != (TileLimit{3, "days"}) {
		t.Fatalf("two Raises of days = %+v; want half a day each", got)
	}
	step("stuck", 1, valueIn(4, "time"))
	if got := limits()["stuck"]; got != (TileLimit{5, "tickets"}) {
		t.Fatalf("a Raise of tickets = %+v; want 1", got)
	}
	// A limit stored in another unit is not stepped: the step starts from
	// the number now, in the unit now.
	m.saveSetup(t, m.project(), Setup{Estimates: &Estimates{Count: "tickets"}})
	step("pace", 1, valueIn(4, "none"))
	if got := limits()["pace"]; got != (TileLimit{5, "tickets"}) {
		t.Fatalf("a Raise over a limit in seconds, counting tickets = %+v; want the count's 4 and 1", got)
	}
}

// An agent's limit is kept in the unit its widget's number was in when it
// was proposed: a project that changes what it counts in before the Keep
// keeps the limit in that unit, not applied, until it counts that way
// again.
func TestKeepStoresAProposalsLimitInTheUnitItWasProposedIn(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	sum := openTickets()
	sum.Metric.Title, sum.Metric.SQL = "Estimate on the board", `SELECT id AS ticket, points AS value FROM tickets`
	sum.Metric.Reduce, sum.Metric.Unit = trackerdash.ReduceSum, trackerdash.UnitEstimate
	limit := 8.0
	tried, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), sum, TileMarks{Limit: &limit}, "thread-1", true)
	if err != nil {
		t.Fatal(err)
	}
	id := tried.View.ID
	m.saveSetup(t, m.project(), Setup{Estimates: &Estimates{Count: "tickets"}})
	if got := m.tiles(t, sub).Limits; len(got) != 0 {
		t.Fatalf("counting tickets, the proposal's limits = %v; want none", got)
	}
	must[string](t)(m.svc.KeepProposal(sub, id, KeepTarget{Title: "Mine"}))
	var tiles boardTilesV1
	if err := json.Unmarshal(m.setupFile(t)[sectionBoardTiles], &tiles); err != nil {
		t.Fatal(err)
	}
	if got := tiles.Boards[0].Limits[id]; got != (TileLimit{8, "points"}) {
		t.Fatalf("the kept limit = %+v; want 8 points, as proposed", got)
	}
	if got := m.tiles(t, sub).Limits; len(got) != 0 {
		t.Fatalf("counting tickets, the kept limits = %v; want none", got)
	}
	m.saveSetup(t, m.project(), Setup{})
	if got := m.tiles(t, sub).Limits; !maps.Equal(got, map[string]TileLimit{id: {8, "points"}}) {
		t.Fatalf("counting points again, the limits = %v", got)
	}
}

// Of two pins written at once, the pins event numbered last carries both,
// whatever order the writes are told in: a publication sends the pins as
// stored when it is numbered. The first write is held after it commits
// until the second has committed and been told.
func TestTheLastPinsEventCarriesThePinsStored(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	other := m.subscribeShown(t, m.project()).ID
	committed, release := make(chan struct{}), make(chan struct{})
	var writes atomic.Int32
	m.svc.afterSetupWrite = func() {
		if writes.Add(1) == 1 {
			close(committed)
			<-release
		}
	}
	first := make(chan error, 1)
	go func() { first <- m.svc.PinWidget(t.Context(), sub, "stuck", true) }()
	<-committed
	if err := m.svc.PinWidget(t.Context(), other, "finished", true); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	boards, _, _ := m.events.snapshot()
	var last *BoardEvent
	for i := range boards {
		if e := &boards[i]; e.Scope == m.scope() && e.Pins != nil && (last == nil || e.Seq > last.Seq) {
			last = e
		}
	}
	want := []string{"stuck", "finished"}
	if last == nil || !slices.Equal(last.Pins.Pins, want) || !slices.Equal(m.pinsOf(t, sub), want) {
		t.Fatalf("the last pins event = %+v, the board's read %v; want %v", last, m.pinsOf(t, sub), want)
	}
}

// A board that pins nothing reads its pins with no work: no widget index
// is built under the setup lock on its reads. Not parallel, as
// AllocsPerRun counts the whole process's allocations.
func TestABoardWithNoPinsReadsThemWithNoWork(t *testing.T) {
	m, sub := dashboardFixture(t)
	scope, key, err := m.svc.boardHandle(sub)
	if err != nil {
		t.Fatal(err)
	}
	kind := trackerdash.Kind{PlansSprints: true, Epics: true}
	if pins, err := m.svc.boardPins(key, string(scope.Board), kind); err != nil || pins != nil {
		t.Fatalf("the pins = %v, %v", pins, err)
	}
	if n := testing.AllocsPerRun(10, func() { _, _ = m.svc.boardPins(key, string(scope.Board), kind) }); n != 0 {
		t.Fatalf("a read of no pins allocates %v times; want none", n)
	}
}
