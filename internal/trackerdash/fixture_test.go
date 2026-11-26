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
}

func newFixture(plansSprints bool, estimates string) *fixture {
	return &fixture{plansSprints: plansSprints, estimates: estimates}
}

func (f *fixture) exec(format string, args ...any) *fixture {
	f.stmts = append(f.stmts, fmt.Sprintf(format, args...))
	return f
}

func (f *fixture) person(id, name string) *fixture {
	return f.exec(`INSERT INTO people VALUES ('%s', '%s')`, id, name)
}

func (f *fixture) status(id, name, group string) *fixture {
	return f.exec(`INSERT INTO statuses VALUES ('%s', '%s', '%s')`, id, name, group)
}

func (f *fixture) sprint(id, name, state string, start, completed int64, position int) *fixture {
	return f.exec(`INSERT INTO sprints VALUES ('%s', '%s', '%s', %d, NULL, %s, %d)`, id, name, state, start, nullable(completed), position)
}

// ticket is one ticket; fields beyond the key name columns as SQL
// literals: status, status_group, assignee, epic, created_at, updated_at,
// points, history_from, started_at, done_at, status_since, resolved_at.
// open and counts follow the group as the store's rules set them: open
// unless done, canceled or left out; counted unless left out.
type ticket struct {
	id, key, status, group, assignee, epic                   string
	created, updated, historyFrom, started, done, since, res int64
	points                                                   string
}

func (f *fixture) ticket(t ticket) *fixture {
	if t.points == "" {
		t.points = "NULL"
	}
	return f.exec(`INSERT INTO tickets(id, key, title, type_level, status, status_group, assignee, parent, epic, created_at, updated_at,
		resolved_at, status_since, points, history_from, started_at, done_at, open, counts)
		VALUES ('%s', '%s', 'Title of %s', 0, '%s', '%s', %s, NULL, %s, %s, %d, %s, %d, %s, %s, %s, %s, %d, %d)`,
		t.id, t.key, t.key, t.status, t.group, quote(t.assignee), quote(t.epic), nullable(t.created), t.updated, nullable(t.res), t.since, t.points,
		nullable(t.historyFrom), nullable(t.started), nullable(t.done),
		map[bool]int{true: 1}[t.group != "done" && t.group != "canceled" && t.group != "left_out"], map[bool]int{true: 1}[t.group != "left_out"])
}

func (f *fixture) member(sprint, ticket string, done bool) *fixture {
	f.exec(`INSERT INTO sprint_tickets VALUES ('%s', '%s')`, sprint, ticket)
	return f.exec(`INSERT INTO sprint_finished VALUES ('%s', '%s', %d)`, sprint, ticket, map[bool]int{true: 1}[done])
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
	return func(ctx context.Context, conn *sql.Conn) (store.MirrorExport, bool, error) {
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
		return store.MirrorExport{Name: "WEB board", EstimateUnit: unit, PlansSprints: f.plansSprints}, true, nil
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
	case n.V == nil && n.NC > 0:
		return fmt.Sprintf("not known, %d not counted", n.NC)
	case n.V == nil:
		return "none"
	case n.NC > 0:
		return fmt.Sprintf("%g, %d not counted", *n.V, n.NC)
	}
	return fmt.Sprintf("%g", *n.V)
}
