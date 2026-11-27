package trackerapp

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

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
// by editBoard. Which way a limit warns is the widget's good direction
// (overLimit); nothing is stored for it. A pin or a limit naming a widget
// the board no longer has is ignored where it is read.

// maxPins is how many widgets a board's summary row shows.
const maxPins = 5

// BoardTiles is what a project keeps for one board's summary row: the
// widgets pinned to it in its order, and each number widget's limit.
type BoardTiles struct {
	Tracker string             `json:"tracker"`
	Site    string             `json:"site"`
	Board   string             `json:"board"`
	Pins    []string           `json:"pins,omitempty"`
	Limits  map[string]float64 `json:"limits,omitempty"`
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
		for id, v := range b.Limits {
			if err := checkWidgetID(id); err != nil {
				return nil, err
			}
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, invalidSetup("%s gives widget %s a limit that is not a number", sectionBoardTiles, id)
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

// boardPins is the widgets key's project pins to board, in the row's
// order; nil for none.
func (s *Service) boardPins(key setupKey, board string) ([]string, error) {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	held, err := s.heldSetupLocked(key.slug)
	if err != nil {
		return nil, err
	}
	return slices.Clone(tilesOf(held.kept.tiles, key.site, board).Pins), nil
}

// BoardTilesView is a board's summary row as its Dashboards screen and
// its Board read it: the widgets pinned, in the row's order, and each
// number widget's limit, by widget ID; both empty, never absent, so a
// client tells a computer that keeps tiles from an older one that does
// not.
type BoardTilesView struct {
	Pins   []string           `json:"pins"`
	Limits map[string]float64 `json:"limits"`
}

// tilesView is key's board's tiles as the listing answers them, keeping
// only the widgets the board's dashboards have (listed) and a limit only
// on a number widget.
func tilesView(t BoardTiles, listed map[string]trackerdash.WidgetView) BoardTilesView {
	out := BoardTilesView{Pins: []string{}, Limits: map[string]float64{}}
	for _, id := range t.Pins {
		if _, ok := listed[id]; ok {
			out.Pins = append(out.Pins, id)
		}
	}
	for id, v := range t.Limits {
		if w, ok := listed[id]; ok && w.Form == trackerdash.FormNumber {
			out.Limits[id] = v
		}
	}
	return out
}

// listedWidgets is every widget of l's dashboards, by ID.
func listedWidgets(l trackerdash.Listing) map[string]trackerdash.WidgetView {
	out := map[string]trackerdash.WidgetView{}
	for _, d := range l.Dashboards {
		for _, w := range d.Widgets {
			out[w.ID] = w
		}
	}
	return out
}

// SetWidgetLimit sets the limit of widget, a number widget of the board
// subscription's dashboards, or clears it for nil; the board's tile and
// the widget are marked when its number passes it.
func (s *Service) SetWidgetLimit(ctx context.Context, subscription, widget string, limit *float64) error {
	if limit != nil && (math.IsNaN(*limit) || math.IsInf(*limit, 0)) {
		return dashRefusal("dashboard_limit_invalid", "a limit is a number")
	}
	return s.editTiles(ctx, subscription, widget, func(w trackerdash.WidgetView, t *BoardTiles) error {
		if w.Form != trackerdash.FormNumber {
			return dashRefusal("dashboard_limit_not_number", fmt.Sprintf("%q is not a number tile; only a number carries a limit", w.Title))
		}
		old, had := t.Limits[widget]
		switch {
		case limit == nil && !had, limit != nil && had && old == *limit:
			return errNoEdit
		case limit == nil:
			delete(t.Limits, widget)
		default:
			if t.Limits == nil {
				t.Limits = map[string]float64{}
			}
			t.Limits[widget] = *limit
		}
		return nil
	})
}

// PinWidget pins widget, one of the board subscription's dashboards', to
// the end of the board's summary row, or takes it off; a full row
// (maxPins) refuses another.
func (s *Service) PinWidget(ctx context.Context, subscription, widget string, pinned bool) error {
	return s.editTiles(ctx, subscription, widget, func(w trackerdash.WidgetView, t *BoardTiles) error {
		has := slices.Contains(t.Pins, widget)
		switch {
		case has == pinned:
			return errNoEdit
		case !pinned:
			t.Pins = slices.DeleteFunc(t.Pins, func(id string) bool { return id == widget })
		case len(t.Pins) >= maxPins:
			return rowFull()
		default:
			t.Pins = append(t.Pins, widget)
		}
		return nil
	})
}

// rowFull is a pin refused because the board's summary row is full.
func rowFull() error {
	return dashRefusal("dashboard_row_full", fmt.Sprintf("the board's summary row already shows %d tiles; take one off to pin this one", maxPins))
}

// editTiles changes the tiles of the board subscription's board with
// edit, given widget as the board's dashboards list it now, and tells the
// clients when it changed. A widget the board's dashboards do not list is
// refused.
func (s *Service) editTiles(ctx context.Context, subscription, widget string, edit func(trackerdash.WidgetView, *BoardTiles) error) error {
	scope, key, err := s.boardHandle(subscription)
	if err != nil {
		return err
	}
	list, err := s.Dashboards(ctx, subscription)
	if err != nil {
		return err
	}
	w, ok := listedWidgets(list)[widget]
	if !ok {
		return dashRefusal("dashboard_widget_gone", "that widget is no longer on this board's dashboards")
	}
	changed, err := s.editBoard(key.slug, key.site, string(scope.Board), func(b *boardKept) error {
		return edit(w, &b.tiles)
	})
	if changed {
		s.dashboardsChanged()
	}
	return err
}
