package trackerapp

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/trackerdash"
)

// The dashboards a project keeps: the setup file's dashboards section
// (setup.go), by tracker site and board, each dashboard a title and its
// widgets, each widget drawing a metric of its own. The section is not
// part of Setup, so a replace keeps it as held; it is written only by
// editBoard, with the board's tiles (board_tiles.go), which reads the file
// again under setupMu as a replace does. Only the file's bound (setupMaxBytes) bounds how many there are.
// A widget's metric is checked when it is counted, as a built-in's is, so
// one that no longer validates shows its failure on its widget and leaves
// the rest of the setup readable.

// BoardDashboards is the dashboards a project keeps for one board of one
// tracker site, in the picker's order.
type BoardDashboards struct {
	Tracker    string          `json:"tracker"`
	Site       string          `json:"site"`
	Board      string          `json:"board"`
	Dashboards []KeptDashboard `json:"dashboards"`
}

// KeptDashboard is one dashboard a project keeps: its ID (dashboardPrefix
// and hex), its title, and its widgets in order.
type KeptDashboard struct {
	ID      string       `json:"id"`
	Title   string       `json:"title"`
	Widgets []KeptWidget `json:"widgets"`
}

// KeptWidget is one widget, kept or proposed: its ID (widgetPrefix and
// hex), which also names its metric, its form and width, and its metric.
type KeptWidget struct {
	ID     string             `json:"id"`
	Form   trackerdash.Form   `json:"form"`
	Width  int                `json:"width"`
	Metric trackerdash.Metric `json:"metric"`
}

type dashboardsV1 struct {
	Version int               `json:"version"`
	Boards  []BoardDashboards `json:"boards"`
}

const (
	// The prefixes of a kept dashboard's and a widget's IDs, which no
	// built-in dashboard or metric has.
	dashboardPrefix = "d_"
	widgetPrefix    = "w_"
	// maxDashboardTitle bounds a dashboard's title.
	maxDashboardTitle = 120
)

// newOwnID is a new dashboard or widget ID with prefix.
func newOwnID(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails; it crashes instead
	return prefix + hex.EncodeToString(b[:])
}

// isOwnID is whether id is prefix and 1 to 62 lowercase hex digits, as
// newOwnID makes it: a metric's name too (trackerdash.Metric.Validate).
func isOwnID(id, prefix string) bool {
	rest, ok := strings.CutPrefix(id, prefix)
	if !ok || rest == "" || len(id) > 64 {
		return false
	}
	for _, c := range rest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validateDashboards checks the dashboards section and returns it
// normalized: each site as its connection names it (checkSite), one
// entry per site and board, in (tracker, site, board) order, without one
// that keeps no dashboard; every ID well formed and used once.
func validateDashboards(boards []BoardDashboards) ([]BoardDashboards, error) {
	type boardKey struct {
		site  siteKey
		board string
	}
	seen := map[boardKey]bool{}
	ids := map[string]bool{}
	once := func(id string) error {
		if ids[id] {
			return invalidSetup("%s names %s twice", sectionDashboards, id)
		}
		ids[id] = true
		return nil
	}
	var out []BoardDashboards
	for _, b := range boards {
		key, err := checkSite(b.Tracker, b.Site, sectionDashboards, map[siteKey]bool{})
		if err != nil {
			return nil, err
		}
		b.Site = key.site
		if err := checkSetupID("board", b.Board); err != nil {
			return nil, err
		}
		if k := (boardKey{key, b.Board}); seen[k] {
			return nil, invalidSetup("%s names board %s of %s %s twice", sectionDashboards, truncateID(b.Board), key.tracker, key.site)
		} else {
			seen[k] = true
		}
		for _, d := range b.Dashboards {
			if !isOwnID(d.ID, dashboardPrefix) {
				return nil, invalidSetup("%s names dashboard %q, which is not a dashboard's ID", sectionDashboards, truncateID(d.ID))
			}
			if err := once(d.ID); err != nil {
				return nil, err
			}
			if err := checkDashboardTitle(d.Title); err != nil {
				return nil, err
			}
			for _, w := range d.Widgets {
				if !isOwnID(w.ID, widgetPrefix) {
					return nil, invalidSetup("dashboard %s names widget %q, which is not a widget's ID", d.ID, truncateID(w.ID))
				}
				if err := once(w.ID); err != nil {
					return nil, err
				}
			}
		}
		if len(b.Dashboards) > 0 {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, func(a, b BoardDashboards) int {
		if c := compareSites(a.Tracker, a.Site, b.Tracker, b.Site); c != 0 {
			return c
		}
		return strings.Compare(a.Board, b.Board)
	})
	return out, nil
}

// checkDashboardTitle checks a dashboard's title.
func checkDashboardTitle(title string) error {
	if strings.TrimSpace(title) == "" || len(title) > maxDashboardTitle {
		return invalidSetup("a dashboard's title is empty or over %d bytes", maxDashboardTitle)
	}
	return nil
}

// dashboardsOf is the dashboards stored keeps for board of site, nil for
// none.
func dashboardsOf(stored []BoardDashboards, site siteKey, board string) []KeptDashboard {
	for _, b := range stored {
		if b.Tracker == site.tracker && b.Site == site.site && b.Board == board {
			return b.Dashboards
		}
	}
	return nil
}

// errNoEdit is an edit that changes nothing: what it removes is gone.
var errNoEdit = errors.New("trackerapp: the dashboards hold nothing to change")

// boardKept is what a project keeps for one board in the kept sections:
// its dashboards and its summary row's tiles.
type boardKept struct {
	dashboards []KeptDashboard
	tiles      BoardTiles
}

// editBoard replaces what slug's project keeps for board of site, a board
// of kind k, with what edit makes of it, reading the file again and
// writing it under setupMu as a replace does (rewriteSetup). edit gets a
// copy it may change and the widgets the board has as the file is read
// (tileWidgets); the tiles it is given, and those it leaves, hold only
// those widgets (pruneTiles), so a widget removed from another window
// leaves no tile. errNoEdit writes nothing and is no failure, and answers
// changed false. Whoever holds the setup is told it moved (setupMoved),
// so a board whose pins changed is told them; the caller tells the
// dashboards' clients, once its whole change is made.
func (s *Service) editBoard(slug string, site siteKey, board string, k trackerdash.Kind, edit func(*boardKept, map[string]tileWidget) error) (changed bool, err error) {
	var editErr error
	var before, after *heldSetup
	before, after, err = s.rewriteSetup(slug, func(stored *heldSetup) (Setup, keptSections, bool) {
		b := boardKept{
			dashboards: cloneDashboards(dashboardsOf(stored.kept.dashboards, site, board)),
			tiles:      cloneTiles(tilesOf(stored.kept.tiles, site, board)),
		}
		ws := tileWidgets(k, b.dashboards)
		pruneTiles(&b.tiles, ws)
		if editErr = edit(&b, ws); editErr != nil {
			return Setup{}, keptSections{}, false
		}
		pruneTiles(&b.tiles, tileWidgets(k, b.dashboards))
		next := keptSections{
			dashboards: slices.DeleteFunc(slices.Clone(stored.kept.dashboards), func(d BoardDashboards) bool {
				return d.Tracker == site.tracker && d.Site == site.site && d.Board == board
			}),
			tiles: slices.DeleteFunc(slices.Clone(stored.kept.tiles), func(t BoardTiles) bool {
				return t.Tracker == site.tracker && t.Site == site.site && t.Board == board
			}),
		}
		next.dashboards = append(next.dashboards, BoardDashboards{Tracker: site.tracker, Site: site.site, Board: board, Dashboards: b.dashboards})
		b.tiles.Tracker, b.tiles.Site, b.tiles.Board = site.tracker, site.site, board
		next.tiles = append(next.tiles, b.tiles)
		if next.dashboards, editErr = validateDashboards(next.dashboards); editErr != nil {
			return Setup{}, keptSections{}, false
		}
		if next.tiles, editErr = validateBoardTiles(next.tiles); editErr != nil {
			return Setup{}, keptSections{}, false
		}
		return stored.view.Setup, next, true
	})
	s.setupMoved(slug, before, after)
	switch {
	case errors.Is(editErr, errNoEdit):
		return false, nil
	case editErr != nil:
		return false, editErr
	case err != nil:
		if full, ok := errors.AsType[*SetupError](err); ok && full.Full {
			return false, dashRefusal("dashboard_full", fmt.Sprintf("this project's dashboards fill its setup file (%d KiB); remove a widget to keep this one", setupMaxBytes>>10))
		}
		return false, err
	}
	return true, nil
}

// cloneDashboards is a copy of ds that an edit may change without
// changing ds.
func cloneDashboards(ds []KeptDashboard) []KeptDashboard {
	out := slices.Clone(ds)
	for i := range out {
		out[i].Widgets = slices.Clone(out[i].Widgets)
	}
	return out
}

// dashRefusal is a dashboard edit refused with code and the sentence the
// person reads.
func dashRefusal(code, sentence string) error {
	return &trackerdash.Refusal{Code: code, Sentence: sentence}
}
