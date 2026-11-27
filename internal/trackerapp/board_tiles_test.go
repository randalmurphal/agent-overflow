package trackerapp

import (
	"encoding/json"
	"maps"
	"math"
	"os"
	"slices"
	"testing"

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

// A number widget's limit and a widget's pin, a built-in's or a kept
// one's, are the setup file's board_tiles section: listed with the
// dashboards, in the board's read (pins), kept over a restart and over a
// save of the setup screen, and each change told once. Remove takes the
// widget's pin and limit with it.
func TestBoardTilesLiveInTheirOwnSection(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	if tiles := m.tiles(t, sub); tiles.Pins == nil || len(tiles.Pins) != 0 || tiles.Limits == nil || tiles.Proposed == nil {
		t.Fatalf("a board with no tiles lists %+v; want each empty, none absent", tiles)
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
	if err := m.svc.PinWidget(t.Context(), sub, "stuck", true); err != nil {
		t.Fatal(err)
	}
	if err := m.svc.PinWidget(t.Context(), sub, w, true); err != nil {
		t.Fatal(err)
	}
	if n := m.events.dashboards.Load() - told; n != 3 {
		t.Fatalf("a limit set twice and two pins were told %d times; want 3", n)
	}
	want := BoardTilesView{Pins: []string{"stuck", w}, Limits: map[string]float64{w: 2}, Proposed: []string{}}
	if got := m.tiles(t, sub); !tilesEqual(got, want) {
		t.Fatalf("the tiles = %+v; want %+v", got, want)
	}
	if v := must[BoardView](t)(m.svc.SubscriptionBoardView(sub, nil)); !slices.Equal(v.Pins, want.Pins) {
		t.Fatalf("the board's read pins %v; want %v", v.Pins, want.Pins)
	}

	// The section is its own: the dashboards section holds no tile, so an
	// older build, which refuses a field it does not know there, reads it.
	data, err := os.ReadFile(m.setupPathOf(t, m.project()))
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]json.RawMessage
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	var dashboards dashboardsV1
	if err := decodeStrict(file[sectionDashboards], &dashboards); err != nil {
		t.Fatalf("the dashboards section as an older build reads it: %v", err)
	}
	var tiles boardTilesV1
	if err := decodeStrict(file[sectionBoardTiles], &tiles); err != nil || len(tiles.Boards) != 1 || tiles.Version != setupSectionVersion {
		t.Fatalf("the board_tiles section = %s, %v", file[sectionBoardTiles], err)
	}

	m.saveSetup(t, m.project(), m.regrouped(map[string]string{"s1": "done"}))
	m.restart(t)
	sub = m.subscribeShown(t, m.project()).ID
	if got := m.tiles(t, sub); !tilesEqual(got, want) {
		t.Fatalf("after a setup save and a restart the tiles = %+v; want %+v", got, want)
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
	if got := m.tiles(t, sub); len(got.Pins) != 0 || len(got.Limits) != 0 {
		t.Fatalf("after Remove the tiles = %+v", got)
	}
	data, err = os.ReadFile(m.setupPathOf(t, m.project()))
	if err != nil {
		t.Fatal(err)
	}
	file = nil
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if _, ok := file[sectionBoardTiles]; ok {
		t.Fatalf("a board with no tiles left keeps the section: %s", data)
	}
}

// A limit is a number widget's and a finite number; the row takes five,
// each once; a widget the board's dashboards do not list takes neither.
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
		t.Fatalf("a pin of a widget the board does not list: %v", err)
	}
	for _, id := range []string{"pace", "start_to_done", "stuck", "finished", "carrying"} {
		if err := m.svc.PinWidget(t.Context(), sub, id, true); err != nil {
			t.Fatalf("pin %s: %v", id, err)
		}
	}
	if err := m.svc.PinWidget(t.Context(), sub, "stuck_list", true); refusalCode(err) != "dashboard_row_full" {
		t.Fatalf("a sixth pin: %v", err)
	}
	told := m.events.dashboards.Load()
	if err := m.svc.PinWidget(t.Context(), sub, "stuck", true); err != nil {
		t.Fatal(err)
	}
	if m.events.dashboards.Load() != told {
		t.Fatal("a pin of a pinned widget was told")
	}
	if got := m.tiles(t, sub).Pins; !slices.Equal(got, []string{"pace", "start_to_done", "stuck", "finished", "carrying"}) {
		t.Fatalf("the row = %v", got)
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
	tried, err := m.svc.TryWidget(t.Context(), m.project(), m.scope(), openTickets(), TileMarks{Limit: &limit, Pin: true}, "thread-1", true)
	if err != nil {
		t.Fatal(err)
	}
	id := tried.View.ID
	want := BoardTilesView{Pins: []string{}, Limits: map[string]float64{id: 2}, Proposed: []string{id}}
	if got := m.tiles(t, sub); !tilesEqual(got, want) {
		t.Fatalf("the proposal's tiles = %+v; want %+v", got, want)
	}
	must[string](t)(m.svc.KeepProposal(sub, id, KeepTarget{Title: "Mine"}))
	want = BoardTilesView{Pins: []string{id}, Limits: map[string]float64{id: 2}, Proposed: []string{}}
	if got := m.tiles(t, sub); !tilesEqual(got, want) {
		t.Fatalf("kept, the tiles = %+v; want %+v", got, want)
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
	got := m.tiles(t, sub)
	if len(got.Pins) != maxPins || slices.Contains(got.Pins, more.View.ID) {
		t.Fatalf("a Keep onto a full row pinned: %v", got.Pins)
	}
	if !slices.ContainsFunc(m.list(t, sub).Dashboards, func(d trackerdash.DashboardView) bool { return d.Title == "More" }) {
		t.Fatal("a Keep onto a full row did not keep the widget")
	}
}

// A board whose pins changed is read again by its clients, which read
// its pins with it; a limit, which the board's read does not carry, reads
// nothing again.
func TestABoardReadsAgainWhenItsPinsChange(t *testing.T) {
	t.Parallel()
	m, sub := dashboardFixture(t)
	w := m.keptWidget(t, sub)
	before := m.readAgains()
	limit := 1.0
	if err := m.svc.SetWidgetLimit(t.Context(), sub, w, &limit); err != nil {
		t.Fatal(err)
	}
	if err := m.svc.PinWidget(t.Context(), sub, "stuck", true); err != nil {
		t.Fatal(err)
	}
	m.events.waitFor(t, "the board read again for its pins", func(boards []BoardEvent, _ []IssueEvent, _ []error) bool {
		n := 0
		for _, e := range boards {
			if e.Scope == m.scope() && e.ReadAgain {
				n++
			}
		}
		return n > before
	})
	if n := m.readAgains() - before; n != 1 {
		t.Fatalf("a limit and a pin read the board again %d times; want once, for the pin", n)
	}
}

func tilesEqual(a, b BoardTilesView) bool {
	return slices.Equal(a.Pins, b.Pins) && slices.Equal(a.Proposed, b.Proposed) && maps.Equal(a.Limits, b.Limits)
}
