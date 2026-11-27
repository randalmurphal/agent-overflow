package trackerdash

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

const day = int64(24 * time.Hour / time.Millisecond)

// testNow is a Wednesday noon in UTC.
var testNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func ms(t time.Time) int64 { return t.UnixMilli() }

// at is testNow less d days.
func ago(d float64) int64 { return ms(testNow) - int64(d*float64(day)) }

// fixture is a board's export written as SQL into the relations, the
// fill a test hands the evaluator in place of the store's.
type fixture struct {
	plansSprints bool
	estimates    string
	stmts        []string
	// groups is each status's group, which its stays' open follows.
	groups map[string]string
	// reading is a board whose first read has not finished.
	reading bool
}

func newFixture(plansSprints bool, estimates string) *fixture {
	return &fixture{plansSprints: plansSprints, estimates: estimates, groups: map[string]string{}}
}

func (f *fixture) exec(format string, args ...any) *fixture {
	f.stmts = append(f.stmts, fmt.Sprintf(format, args...))
	return f
}

func (f *fixture) person(id, name string) *fixture {
	return f.exec(`INSERT INTO people VALUES ('%s', '%s')`, id, name)
}

func (f *fixture) status(id, name, group string) *fixture {
	f.groups[id] = group
	return f.exec(`INSERT INTO statuses VALUES ('%s', '%s', '%s')`, id, name, group)
}

// openGroup is whether a status of group counts as open, as the store's
// rule sets tickets.open and status_intervals.open: not done, canceled or
// left out, an unclassified status included.
func openGroup(group string) int {
	return map[bool]int{true: 1}[group != "done" && group != "canceled" && group != "left_out"]
}

func (f *fixture) sprint(id, name, state string, start, completed int64, position int) *fixture {
	return f.exec(`INSERT INTO sprints VALUES ('%s', '%s', '%s', %d, NULL, %s, %d)`, id, name, state, start, nullable(completed), position)
}

// ticket is one ticket; fields beyond the key name columns as SQL
// literals: status, status_group, assignee, epic, created_at, updated_at,
// points, history_from, started_at, done_at, status_since, resolved_at;
// behind is history_behind.
// open and counts follow the group as the store's rules set them: open
// unless done, canceled or left out; counted unless left out. A ticket
// with a history shows its start unless startUnknown (it was in progress
// when its history begins), as started_known says.
type ticket struct {
	id, key, status, group, assignee, epic                   string
	created, updated, historyFrom, started, done, since, res int64
	points                                                   string
	startUnknown, behind                                     bool
}

func (f *fixture) ticket(t ticket) *fixture {
	if t.points == "" {
		t.points = "NULL"
	}
	// The store's status interval now: the status from status_since.
	if t.since > 0 && t.status != "" {
		f.exec(`INSERT INTO status_intervals(ticket, status, from_at, to_at, began, open) VALUES ('%s', '%s', %d, NULL, %d, %d)`,
			t.id, t.status, t.since, t.since, openGroup(t.group))
	}
	return f.exec(`INSERT INTO tickets(id, key, title, type_level, status, status_group, assignee, parent, epic, created_at, updated_at,
		resolved_at, status_since, points, history_from, history_behind, started_known, started_at, done_at, open, counts)
		VALUES ('%s', '%s', 'Title of %s', 0, '%s', '%s', %s, NULL, %s, %s, %d, %s, %d, %s, %s, %d, %d, %s, %s, %d, %d)`,
		t.id, t.key, t.key, t.status, t.group, quote(t.assignee), quote(t.epic), nullable(t.created), t.updated, nullable(t.res), t.since, t.points,
		nullable(t.historyFrom), map[bool]int{true: 1}[t.behind], map[bool]int{true: 1}[t.historyFrom != 0 && !t.startUnknown], nullable(t.started),
		nullable(t.done), openGroup(t.group), map[bool]int{true: 1}[t.group != "left_out"])
}

// dated is a sprint with its start and end dates (0 for none).
func (f *fixture) dated(id, name, state string, start, end, completed int64, position int) *fixture {
	return f.exec(`INSERT INTO sprints VALUES ('%s', '%s', '%s', %s, %s, %s, %d)`, id, name, state, nullable(start), nullable(end), nullable(completed), position)
}

// stay is a ticket's status interval from from until to (0: now), which
// began at from.
func (f *fixture) stay(ticket, status string, from, to int64) *fixture {
	return f.covered(ticket, status, from, to, from)
}

// covered is a ticket's status interval from from until to (0: now),
// which began at began (0: before from, at a time not known).
func (f *fixture) covered(ticket, status string, from, to, began int64) *fixture {
	return f.exec(`INSERT INTO status_intervals(ticket, status, from_at, to_at, began, open) VALUES ('%s', '%s', %d, %s, %s, %d)`,
		ticket, status, from, nullable(to), nullable(began), openGroup(f.groups[status]))
}

// stretch is a ticket's sprint interval at points (empty: none) from from
// until to (0: now).
func (f *fixture) stretch(sprint, ticket, points string, from, to int64) *fixture {
	if points == "" {
		points = "NULL"
	}
	return f.exec(`INSERT INTO sprint_intervals VALUES ('%s', '%s', %s, %d, %s)`, sprint, ticket, points, from, nullable(to))
}

func (f *fixture) member(sprint, ticket string, done bool) *fixture {
	f.exec(`INSERT INTO sprint_tickets VALUES ('%s', '%s')`, sprint, ticket)
	return f.exec(`INSERT INTO sprint_finished VALUES ('%s', '%s', %d)`, sprint, ticket, map[bool]int{true: 1}[done])
}

// unsettled is a member of sprint whose status at its completion is not
// known.
func (f *fixture) unsettled(sprint, ticket string) *fixture {
	f.exec(`INSERT INTO sprint_tickets VALUES ('%s', '%s')`, sprint, ticket)
	return f.exec(`INSERT INTO sprint_finished VALUES ('%s', '%s', NULL)`, sprint, ticket)
}

func nullable(v int64) string {
	if v == 0 {
		return "NULL"
	}
	return fmt.Sprint(v)
}

func quote(s string) string {
	if s == "" {
		return "NULL"
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// fill is the fixture as a Fill.
func (f *fixture) fill() Fill {
	return func(ctx context.Context, conn *sql.Conn, _ []string) (store.MirrorExport, bool, error) {
		if _, err := conn.ExecContext(ctx, store.MirrorExportSchema); err != nil {
			return store.MirrorExport{}, false, err
		}
		unit := map[string]int{"points": store.MirrorEstimatePoints, "time": store.MirrorEstimateTime, "none": store.MirrorEstimateNone}[f.estimates]
		stmts := append([]string{fmt.Sprintf(`INSERT INTO board VALUES ('WEB board', %d, '%s')`, map[bool]int{true: 1}[f.plansSprints], f.estimates)}, f.stmts...)
		for _, s := range stmts {
			if _, err := conn.ExecContext(ctx, s); err != nil {
				return store.MirrorExport{}, false, fmt.Errorf("%s: %w", s, err)
			}
		}
		return store.MirrorExport{Name: "WEB board", EstimateUnit: unit, PlansSprints: f.plansSprints, FirstReadDone: !f.reading,
			Earliest: ms(testNow) - 180*day}, true, nil
	}
}

func (f *fixture) board() Board { return Board{Fill: f.fill(), Epics: true, Viewer: "u1"} }

func evaluator() *Evaluator { return NewEvaluator(func() time.Time { return testNow }) }

// sprintBoard is a points board with six closed sprints a week apart (s1
// oldest, completed 36 days ago, s6 newest, 1 day ago) and an active one,
// two people, and tickets finished across them.
func sprintBoard() *fixture {
	f := newFixture(true, "points").
		person("u1", "User 1").person("u2", "User 2").
		status("1", "To Do", "todo").status("3", "In Progress", "in_progress").status("5", "Done", "done").
		exec(`INSERT INTO epics VALUES ('e1', 'WEB-100', 'Checkout')`)
	for i := 1; i <= 6; i++ {
		completed := ago(float64(36 - 7*(i-1)))
		f.sprint(fmt.Sprintf("s%d", i), fmt.Sprintf("WEB Sprint %d", i), "closed", completed-14*day, completed, i)
	}
	f.sprint("s7", "WEB Sprint 7", "active", ago(1), 0, 7)
	return f
}

func mustEvaluate(t *testing.T, e *Evaluator, b Board, r Request) Answer {
	t.Helper()
	a, err := e.Evaluate(context.Background(), b, r)
	if err != nil {
		t.Fatalf("evaluate %+v: %v", r, err)
	}
	return a
}

func widgetOf(t *testing.T, a Answer, id string) WidgetValue {
	t.Helper()
	for _, w := range a.Widgets {
		if w.ID == id {
			return w
		}
	}
	t.Fatalf("no widget %s in %+v", id, a.Widgets)
	return WidgetValue{}
}

func num(n *Num) string {
	switch {
	case n == nil:
		return "absent"
	case n.Gap:
		return "gap"
	case n.UR > 0:
		return fmt.Sprintf("not known, %d days not read", n.UR)
	case n.V == nil && n.NC > 0:
		return fmt.Sprintf("not known, %d not counted", n.NC)
	case n.V == nil:
		return "none"
	case n.NC > 0:
		return fmt.Sprintf("%g, %d not counted", *n.V, n.NC)
	}
	return fmt.Sprintf("%g", *n.V)
}
