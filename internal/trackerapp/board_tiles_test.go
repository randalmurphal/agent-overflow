package trackerapp

import (
	"encoding/json"
	"errors"
	"maps"
	"math"
	"os"
	"slices"
	"testing"

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
	told := m.events.dashboards.Load()
	limit := 2.0
	if err := m.svc.SetWidgetLimit(t.Context(), sub, w, &limit); err != nil {
		t.Fatal(err)
	}
	if err := m.svc.SetWidgetLimit(t.Context(), sub, w, &limit); err != nil {
		t.Fatal(err)
	}
	if n, last := m.events.dashboards.Load()-told, m.events.lastDashboards.Load(); n != 1 || *last != (DashboardsChange{Project: m.project(), Board: string(testBoard)}) {
		t.Fatalf("a limit set twice was told %d times, last %+v; want once, of the board, counting nothing again", n, *last)
	}
	told = m.events.dashboards.Load()
	if err := m.svc.PinWidget(t.Context(), sub, "stuck", true); err != nil {
		t.Fatal(err)
	}
	if err := m.svc.PinWidget(t.Context(), sub, w, true); err != nil {
		t.Fatal(err)
	}
	if n := m.events.dashboards.Load() - told; n != 0 {
		t.Fatalf("two pins were told as a change of the dashboards %d times", n)
	}
	want := BoardTilesView{Limits: map[string]float64{w: 2}, Proposed: []string{}, Max: maxPins}
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
	if err := m.svc.SetWidgetLimit(t.Context(), sub, w, &limit); err != nil {
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
	limit := 3.0
	if err := m.svc.SetWidgetLimit(t.Context(), sub, "stuck_list", &limit); refusalCode(err) != "dashboard_limit_not_number" {
		t.Fatalf("a limit on a list: %v", err)
	}
	nan := math.NaN()
	if err := m.svc.SetWidgetLimit(t.Context(), sub, "stuck", &nan); refusalCode(err) != "dashboard_limit_invalid" {
		t.Fatalf("a limit that is not a number: %v", err)
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
	request := trackerdash.Request{Dashboard: m.list(t, sub).Dashboards[0].ID, Widgets: append(slices.Clone(row), "stuck_list")}
	if _, err := m.svc.EvaluateDashboard(t.Context(), sub, request); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a row request of six widgets: %v", err)
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
	limit := 1.0
	if err := m.svc.SetWidgetLimit(t.Context(), sub, w, &limit); refusalCode(err) != "dashboard_widget_gone" {
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
	limit := 8.0
	if err := m.svc.SetWidgetLimit(t.Context(), sub, "pace", &limit); err != nil {
		t.Fatal(err)
	}
	if got := m.tiles(t, sub).Limits; !maps.Equal(got, map[string]float64{"pace": 8}) {
		t.Fatalf("set in points, the limits = %v", got)
	}
	m.saveSetup(t, m.project(), Setup{Estimates: &Estimates{Count: "tickets"}})
	if got := m.tiles(t, sub).Limits; len(got) != 0 {
		t.Fatalf("counting tickets, the limits = %v; want none", got)
	}
	m.saveSetup(t, m.project(), Setup{})
	if got := m.tiles(t, sub).Limits; !maps.Equal(got, map[string]float64{"pace": 8}) {
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
	want := BoardTilesView{Limits: map[string]float64{id: 2}, Proposed: []string{id}, Max: maxPins}
	if got := m.tiles(t, sub); !tilesEqual(got, want) {
		t.Fatalf("the proposal's tiles = %+v; want %+v", got, want)
	}
	must[string](t)(m.svc.KeepProposal(sub, id, KeepTarget{Title: "Mine"}))
	want = BoardTilesView{Limits: map[string]float64{id: 2}, Proposed: []string{}, Max: maxPins}
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
