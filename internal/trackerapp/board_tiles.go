package trackerapp

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerdash"
)

// The board tiles a project keeps: the setup file's board_tiles section
// (setup.go), by tracker site and board, the widgets pinned to the board's
// summary row, at most maxPins in the row's order, and the limit a number
// widget carries. A widget is named by its ID on the board's dashboards: a
// built-in widget's or a kept one's, so a built-in tile carries a limit
// and a pin as a kept one does. The section is a section of its own, not
// fields of the dashboards section, so an older build, which refuses a
// field it does not know, keeps it as it keeps any section it does not
// read. Like the dashboards it is not part of Setup and is written only
// by editBoard, which checks every edit against the widgets the board has
// as it writes (tileWidgets) and drops a pin or a limit naming one it no
// longer has; a reader drops them too (livePins, shownLimits), for a file
// an older build or a hand left so. Which way a limit warns is the
// widget's good direction (overLimit); nothing is stored for it.

// maxPins is how many widgets a board's summary row shows.
const maxPins = 5

// BoardTiles is what a project keeps for one board's summary row: the
// widgets pinned to it in its order, and each number widget's limit.
type BoardTiles struct {
	Tracker string               `json:"tracker"`
	Site    string               `json:"site"`
	Board   string               `json:"board"`
	Pins    []string             `json:"pins,omitempty"`
	Limits  map[string]TileLimit `json:"limits,omitempty"`
}

// TileLimit is a number widget's limit and the unit its number was in
// when the limit was set (trackerdash.ValueUnit): tickets, days, points or
// seconds. A limit applies only while the number is in that unit, so a
// project that changes what it counts in keeps the limit, not applied,
// until it counts that way again.
type TileLimit struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

// limitUnits are the units a limit is set in.
var limitUnits = []string{"tickets", "days", "points", "seconds"}

// limitStep is how far a Raise or a Lower moves a limit set in unit: an
// hour of seconds, half a day, else 1.
func limitStep(unit string) float64 {
	switch unit {
	case "seconds":
		return 3600
	case "days":
		return 0.5
	}
	return 1
}

// stepped is a limit of from in unit moved by step, a Raise (1) or a Lower
// (-1), by its unit's step. A Lower stops at 0, so it never lowers a limit
// at or below 0, which only a signed count's number sets; a Raise always
// moves it. A limit set as a value is kept as it is set.
func stepped(from float64, step int, unit string) float64 {
	next := from + float64(step)*limitStep(unit)
	if step < 0 {
		next = max(next, min(from, 0))
	}
	return next
}

type boardTilesV1 struct {
	Version int          `json:"version"`
	Boards  []BoardTiles `json:"boards"`
}

// validateBoardTiles checks the board_tiles section and returns it
// normalized as validateDashboards does: one entry per site and board, in
// (tracker, site, board) order, without one that keeps nothing.
func validateBoardTiles(boards []BoardTiles) ([]BoardTiles, error) {
	type boardKey struct {
		site  siteKey
		board string
	}
	seen := map[boardKey]bool{}
	var out []BoardTiles
	for _, b := range boards {
		key, err := checkSite(b.Tracker, b.Site, sectionBoardTiles, map[siteKey]bool{})
		if err != nil {
			return nil, err
		}
		b.Site = key.site
		if err := checkSetupID("board", b.Board); err != nil {
			return nil, err
		}
		if k := (boardKey{key, b.Board}); seen[k] {
			return nil, invalidSetup("%s names board %s of %s %s twice", sectionBoardTiles, truncateID(b.Board), key.tracker, key.site)
		} else {
			seen[k] = true
		}
		if len(b.Pins) > maxPins {
			return nil, invalidSetup("%s pins %d widgets to board %s, over %d", sectionBoardTiles, len(b.Pins), truncateID(b.Board), maxPins)
		}
		for i, id := range b.Pins {
			if err := checkWidgetID(id); err != nil {
				return nil, err
			}
			if slices.Contains(b.Pins[:i], id) {
				return nil, invalidSetup("%s pins widget %s to board %s twice", sectionBoardTiles, id, truncateID(b.Board))
			}
		}
		for id, l := range b.Limits {
			if err := checkWidgetID(id); err != nil {
				return nil, err
			}
			if math.IsNaN(l.Value) || math.IsInf(l.Value, 0) {
				return nil, invalidSetup("%s gives widget %s a limit that is not a number", sectionBoardTiles, id)
			}
			if !slices.Contains(limitUnits, l.Unit) {
				return nil, invalidSetup("%s gives widget %s a limit in %q, which is not one of %s", sectionBoardTiles, id, truncateID(l.Unit), strings.Join(limitUnits, ", "))
			}
		}
		if len(b.Limits) == 0 {
			b.Limits = nil
		}
		if len(b.Pins) == 0 {
			b.Pins = nil
		}
		if b.Pins != nil || b.Limits != nil {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, func(a, b BoardTiles) int {
		if c := compareSites(a.Tracker, a.Site, b.Tracker, b.Site); c != 0 {
			return c
		}
		return strings.Compare(a.Board, b.Board)
	})
	return out, nil
}

// checkWidgetID checks a widget's ID as a tile names it: 1 to 64
// lowercase letters, digits and underscores, as a built-in's ID and a
// kept one's (widgetPrefix and hex) both are.
func checkWidgetID(id string) error {
	ok := id != "" && len(id) <= 64
	for _, c := range id {
		ok = ok && (c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_')
	}
	if !ok {
		return invalidSetup("%s names widget %q, which is not a widget's ID", sectionBoardTiles, truncateID(id))
	}
	return nil
}

// tilesOf is what stored keeps for board of site's summary row; the
// zero value for nothing.
func tilesOf(stored []BoardTiles, site siteKey, board string) BoardTiles {
	for _, b := range stored {
		if b.Tracker == site.tracker && b.Site == site.site && b.Board == board {
			return b
		}
	}
	return BoardTiles{}
}

// cloneTiles is a copy of t that an edit may change without changing t.
func cloneTiles(t BoardTiles) BoardTiles {
	t.Pins = slices.Clone(t.Pins)
	t.Limits = maps.Clone(t.Limits)
	return t
}

// tileWidget is a widget a board's tile may name: its form and metric.
type tileWidget struct {
	form   trackerdash.Form
	metric trackerdash.Metric
}

// tileWidgets is every widget a tile of a board of kind k may name, by
// ID: its built-in dashboard's and those of kept, its project's
// dashboards for it. The one rule for which widgets a board has, which
// every write and read of its tiles applies.
func tileWidgets(k trackerdash.Kind, kept []KeptDashboard) map[string]tileWidget {
	d, metrics := trackerdash.Builtin(k)
	out := make(map[string]tileWidget, len(d.Widgets))
	for _, w := range d.Widgets {
		out[w.ID] = tileWidget{form: w.Form, metric: metrics[w.Metric]}
	}
	for _, kd := range kept {
		for _, w := range kd.Widgets {
			out[w.ID] = tileWidget{form: w.Form, metric: w.Metric}
		}
	}
	return out
}

// pruneTiles drops from t a pin of a widget ws does not hold, and a limit
// of one it does not hold as a number. A limit set in another unit stays.
func pruneTiles(t *BoardTiles, ws map[string]tileWidget) {
	t.Pins = slices.DeleteFunc(t.Pins, func(id string) bool {
		_, ok := ws[id]
		return !ok
	})
	maps.DeleteFunc(t.Limits, func(id string, _ TileLimit) bool {
		return ws[id].form != trackerdash.FormNumber
	})
}

// livePins is t's pins of widgets ws holds, in the row's order; nil for
// none.
func livePins(t BoardTiles, ws map[string]tileWidget) []string {
	var out []string
	for _, id := range t.Pins {
		if _, ok := ws[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// shownLimits is t's limits that apply on a board whose estimates are
// estimateUnit: of a number widget ws holds, set in the unit its number
// is in now.
func shownLimits(t BoardTiles, ws map[string]tileWidget, estimateUnit int) map[string]TileLimit {
	out := map[string]TileLimit{}
	for id, l := range t.Limits {
		if w, ok := ws[id]; ok && w.form == trackerdash.FormNumber && trackerdash.ValueUnit(w.metric, estimateUnit) == l.Unit {
			out[id] = l
		}
	}
	return out
}

// admitPin pins id to the end of t's row when the row has room: the one
// rule for a row's room, which a Pin and a Keep apply.
func admitPin(t *BoardTiles, id string) error {
	switch {
	case slices.Contains(t.Pins, id):
		return errNoEdit
	case len(t.Pins) >= maxPins:
		return rowFull()
	}
	t.Pins = append(t.Pins, id)
	return nil
}

// tileBoard is what a board's tiles read of the board as the mirror holds
// it: its kind, which picks its built-in dashboard, and the unit its
// project counts it in (store.MirrorEstimate*).
type tileBoard struct {
	kind         trackerdash.Kind
	estimateUnit int
}

// tileBoardOf reads board scope of key's project as its tiles read it.
func (s *Service) tileBoardOf(scope Scope, key setupKey) (tileBoard, error) {
	groups, err := s.boardStatusGroups(key)
	if err != nil {
		return tileBoard{}, err
	}
	root, err := s.readRoot(scope.Connection)
	if err != nil {
		return tileBoard{}, err
	}
	defer s.leave()
	st, ok, err := s.store.GetMirrorBoard(root, string(scope.Board))
	switch {
	case err != nil:
		return tileBoard{}, fmt.Errorf("trackerapp: read board %s from the mirror: %w", scope.Board, err)
	case !ok:
		return tileBoard{}, fmt.Errorf("%w: board %s", ErrNotMirrored, scope.Board)
	}
	return tileBoard{
		kind:         trackerdash.Kind{PlansSprints: st.Board.PlansSprints, Epics: hasEpics(key.site.tracker)},
		estimateUnit: int(groups.unit(tracker.EstimateUnit(st.Board.EstimateUnit))),
	}, nil
}

// boardPins is the widgets key's project pins to board, a board of kind
// k, in the row's order; nil for none.
func (s *Service) boardPins(key setupKey, board string, k trackerdash.Kind) ([]string, error) {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	held, err := s.heldSetupLocked(key.slug)
	if err != nil {
		return nil, err
	}
	t := tilesOf(held.kept.tiles, key.site, board)
	if len(t.Pins) == 0 {
		return nil, nil
	}
	return livePins(t, tileWidgets(k, dashboardsOf(held.kept.dashboards, key.site, board))), nil
}

// BoardTilesView is a board's summary row as its Dashboards screen reads
// it, beside the pins its board subscription carries (BoardView.Pins):
// each number widget's limit that applies now, with its unit, by widget
// ID, a proposed one's included, the proposals Keep pins (Proposed), and
// how many tiles the row shows (Max); the maps and lists not nil.
type BoardTilesView struct {
	Limits   map[string]TileLimit `json:"limits"`
	Proposed []string             `json:"proposed"`
	Max      int                  `json:"max"`
}

// tilesView is t and the marks of proposed as a listing answers them: the
// limits that apply on a board whose estimates are estimateUnit among the
// widgets ws, and those proposed that l lists, whose limit applies while
// the unit it was proposed in holds, as a kept one's does.
func tilesView(t BoardTiles, ws map[string]tileWidget, estimateUnit int, proposed []heldProposal, l trackerdash.Listing) BoardTilesView {
	out := BoardTilesView{Limits: shownLimits(t, ws, estimateUnit), Proposed: []string{}, Max: maxPins}
	for _, p := range proposed {
		if !slices.ContainsFunc(l.Proposals, func(v trackerdash.ProposalView) bool { return v.View.ID == p.widget.ID }) {
			continue
		}
		if p.limit != nil && p.limit.Unit == trackerdash.ValueUnit(p.widget.Metric, estimateUnit) {
			out.Limits[p.widget.ID] = *p.limit
		}
		if p.pin {
			out.Proposed = append(out.Proposed, p.widget.ID)
		}
	}
	return out
}

// checkMarks refuses marks w cannot carry: a limit on a widget that is not
// a number, or one that is not a finite number.
func checkMarks(w KeptWidget, marks TileMarks) error {
	if marks.Limit == nil {
		return nil
	}
	if w.Form != trackerdash.FormNumber {
		return dashRefusal("dashboard_limit_not_number", fmt.Sprintf("a limit is a number widget's; this one is a %s", w.Form))
	}
	return checkLimit(*marks.Limit)
}

// checkLimit refuses a limit that is not a finite number.
func checkLimit(v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return dashRefusal("dashboard_limit_invalid", "a limit is a finite number")
	}
	return nil
}

// estimatesWords is how a refusal names what a board's estimates are
// counted in (trackerdash.EstimatesOf).
func estimatesWords(estimates string) string {
	switch estimates {
	case "points":
		return "story points"
	case "time":
		return "estimated time"
	}
	return "tickets"
}

// LimitChange is a change of a number widget's limit: Value, the number
// a count answered in Estimates (trackerdash.Answer.Estimates), and Step,
// a Raise (1) or a Lower (-1) of the stored limit by its unit's step
// (limitStep). A step of a widget with no limit stored in the unit its
// number is in now starts from Value, which it then names.
type LimitChange struct {
	Value     *float64 `json:"value,omitempty"`
	Estimates string   `json:"estimates,omitempty"`
	Step      int      `json:"step,omitempty"`
}

// SetWidgetLimit changes the limit of widget, a number widget of the board
// subscription's dashboards, by change, or clears it for nil; the board's
// tile and the widget are marked when its number passes it. The limit is
// in the unit its number is in now: a Value counted in other estimates
// than the board's now is refused, and a step moves the stored limit
// from the value it holds when the write is made (stepped). The
// clients are told the board's dashboards changed, and that nothing they
// count did.
func (s *Service) SetWidgetLimit(ctx context.Context, subscription, widget string, change *LimitChange) error {
	if change != nil {
		if change.Step < -1 || change.Step > 1 || change.Step == 0 && change.Value == nil {
			return fmt.Errorf("%w: a limit's change is a value, a step of 1 or -1, or both", ErrInvalidRequest)
		}
		if change.Value != nil {
			if err := checkLimit(*change.Value); err != nil {
				return err
			}
		}
	}
	key, scope, changed, err := s.editTiles(subscription, widget, func(w tileWidget, tb tileBoard, t *BoardTiles) error {
		if w.form != trackerdash.FormNumber {
			return dashRefusal("dashboard_limit_not_number", "only a number tile carries a limit")
		}
		old, had := t.Limits[widget]
		if change == nil {
			if !had {
				return errNoEdit
			}
			delete(t.Limits, widget)
			return nil
		}
		unit := trackerdash.ValueUnit(w.metric, tb.estimateUnit)
		if change.Value != nil {
			if now := trackerdash.EstimatesOf(tb.estimateUnit); change.Estimates != now {
				return dashRefusal("dashboard_limit_unit", fmt.Sprintf("the board's numbers are counted in %s now; set the limit again from its new count", estimatesWords(now)))
			}
		}
		var value float64
		switch {
		case change.Step != 0 && had && old.Unit == unit:
			value = stepped(old.Value, change.Step, unit)
		case change.Value == nil:
			return dashRefusal("dashboard_limit_start", "this widget has no limit to step from; count it and set one")
		default:
			value = stepped(*change.Value, change.Step, unit)
		}
		next := TileLimit{Value: value, Unit: unit}
		if had && old == next {
			return errNoEdit
		}
		if t.Limits == nil {
			t.Limits = map[string]TileLimit{}
		}
		t.Limits[widget] = next
		return nil
	})
	if changed {
		s.tellDashboards(key.slug, false, boardIs(key.site, string(scope.Board)))
	}
	return err
}

// PinWidget pins widget, one of the board subscription's dashboards', to
// the end of the board's summary row, or takes it off; a full row
// (maxPins) refuses another. The pins reach the clients on the board's
// subscriptions (setupMoved), so nothing else is told.
func (s *Service) PinWidget(ctx context.Context, subscription, widget string, pinned bool) error {
	_, _, _, err := s.editTiles(subscription, widget, func(_ tileWidget, _ tileBoard, t *BoardTiles) error {
		if pinned {
			return admitPin(t, widget)
		}
		if !slices.Contains(t.Pins, widget) {
			return errNoEdit
		}
		t.Pins = slices.DeleteFunc(t.Pins, func(id string) bool { return id == widget })
		return nil
	})
	return err
}

// rowFull is a pin refused because the board's summary row is full.
func rowFull() error {
	return dashRefusal("dashboard_row_full", fmt.Sprintf("the board's summary row already shows %d tiles; take one off to pin this one", maxPins))
}

// editTiles changes the tiles of the board subscription's board with
// edit, given widget as the board has it when the edit is written
// (editBoard), and answers the board's project and scope and whether the
// tiles changed. A widget the board does not have then is refused.
func (s *Service) editTiles(subscription, widget string, edit func(tileWidget, tileBoard, *BoardTiles) error) (setupKey, Scope, bool, error) {
	scope, key, err := s.boardHandle(subscription)
	if err != nil {
		return setupKey{}, Scope{}, false, err
	}
	tb, err := s.tileBoardOf(scope, key)
	if err != nil {
		return setupKey{}, Scope{}, false, err
	}
	changed, err := s.editBoard(key.slug, key.site, string(scope.Board), tb.kind, func(b *boardKept, ws map[string]tileWidget) error {
		w, ok := ws[widget]
		if !ok {
			return dashRefusal("dashboard_widget_gone", "that widget is no longer on this board's dashboards")
		}
		return edit(w, tb, &b.tiles)
	})
	return key, scope, changed, err
}
