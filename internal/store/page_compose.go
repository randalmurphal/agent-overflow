package store

import (
	"strings"

	"agent-overflow/internal/settings"
)

// Page assembly (docs/architecture/timeline-window-pages.md §2.1 steps
// 4-5): the units a pager walked become the rows it ships, the stubs that
// account for everything it did not, and the range those units span.

// clampActivityRunWindowRows bounds how many members of one run a page
// ships. The bounds are the settings bounds because the number IS the
// per-client `activityRunWindowRows` setting riding the request; clamping
// again here means a caller that forgot cannot make the store read an
// unbounded window per run.
func clampActivityRunWindowRows(rows int) int {
	if rows == 0 {
		return settings.DefaultActivityRunWindowRows
	}
	if rows < settings.MinActivityRunWindowRows {
		return settings.MinActivityRunWindowRows
	}
	if rows > settings.MaxActivityRunWindowRows {
		return settings.MaxActivityRunWindowRows
	}
	return rows
}

// composePagedUnits hydrates a page's shipped rows, builds a stub for
// every run in it, and bounds the whole thing by the units' physical
// edges.
//
// The range is the UNITS' span, not the shipped rows': a page whose
// newest unit is a run ends at that run's last member even when the
// shipped span stops earlier. That is what makes "every physical row in
// the range is shipped or counted by exactly one stub" true, and what the
// has-more probes are asked about.
func (s *Store) composePagedUnits(q sqlQueryer, threadID string, units []pageUnit, scope timelineScope) (PagedItems, error) {
	if len(units) == 0 {
		return emptyPagedItems(), nil
	}
	ids := make([]string, 0, len(units))
	runs := []ActivityRunStub{}
	for _, unit := range units {
		for i := unit.shippedFrom; i < unit.shippedTo; i++ {
			ids = append(ids, unit.rows[i].ID)
		}
		if unit.run {
			runs = append(runs, buildActivityRunStub(unit.rows, unit.shippedFrom, unit.shippedTo))
		}
	}
	items, err := s.hydratePageItems(q, threadID, ids)
	if err != nil {
		return PagedItems{}, err
	}

	oldest := units[0].oldest().cursor()
	newest := units[len(units)-1].newest().cursor()
	hasMoreOlder, err := hasOlderItems(q, threadID, oldest, scope)
	if err != nil {
		return PagedItems{}, err
	}
	hasMoreNewer, err := hasNewerItems(q, threadID, newest, scope)
	if err != nil {
		return PagedItems{}, err
	}
	return PagedItems{
		Scope:           scope.context,
		Items:           items,
		Runs:            runs,
		OldestCursor:    oldest,
		NewestCursor:    newest,
		OldestTurnIndex: oldest.TurnIndex,
		NewestTurnIndex: newest.TurnIndex,
		HasMore:         hasMoreOlder,
		HasMoreOlder:    hasMoreOlder,
		HasMoreNewer:    hasMoreNewer,
	}, nil
}

// hydratePageItems resolves the page's shipped ids to rendered rows:
// the full Item projection plus the read-time decorations (proposed-plan
// state, subagent anchor aggregates) every frontend-bound window carries.
func (s *Store) hydratePageItems(q sqlQueryer, threadID string, ids []string) ([]Item, error) {
	if len(ids) == 0 {
		return []Item{}, nil
	}
	selectedSQL, selectedArgs := idListSelection(ids)
	return s.querySelectedPagedItems(q, threadID, selectedSQL, selectedArgs...)
}

// idListSelection renders an explicit id list as the single-column
// selection the hydrators consume. `VALUES` rather than a `UNION ALL`
// chain: SQLite's compound-select limit does not apply to it, and a page
// can name a couple of thousand rows.
func idListSelection(ids []string) (string, []any) {
	var sql strings.Builder
	sql.Grow(6 * len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			sql.WriteString(", ")
		}
		sql.WriteString("(?)")
		args = append(args, id)
	}
	return "VALUES " + sql.String(), args
}
