package store

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// boundLimitSQL matches a LIMIT whose value is a bound parameter: `?`,
// `?N`, or a named one.
var boundLimitSQL = regexp.MustCompile(`LIMIT\s+[?:@$]`)

// TestStoreSQLNeverBindsLimit fails on a bound LIMIT in the package's
// non-test source and lists each one at file:line. SQLite's planner reads
// a bound LIMIT when it compiles the statement, so every rebind of that
// parameter expires the statement and its next run compiles it again. A
// statement the cache (stmt_cache.go) keeps compiled therefore recompiles
// on every run unless its LIMIT is a literal in the text; OFFSET may bind.
func TestStoreSQLNeverBindsLimit(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	var found []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for _, at := range boundLimitSQL.FindAllIndex(src, -1) {
			found = append(found, fmt.Sprintf("%s:%d", name, 1+bytes.Count(src[:at[0]], []byte("\n"))))
		}
	}
	if scanned == 0 {
		t.Fatal("no package source found to scan")
	}
	if len(found) > 0 {
		t.Errorf("bound LIMIT; render the limit into the SQL text with strconv.Itoa:\n%s", strings.Join(found, "\n"))
	}
}

// statementsSince lists the statements the observed store compiled or ran
// after before was taken.
func statementsSince(counts *sqlCounts, before map[sqlUseKey]int) []string {
	seen := map[string]bool{}
	var out []string
	for key, n := range counts.snapshot() {
		if key.use == sqlStmtClose || n <= before[key] || seen[key.query] {
			continue
		}
		seen[key.query] = true
		out = append(out, key.query)
	}
	return out
}

// TestNonPositiveLimitIsRefusedBeforeSQL covers the accessors whose limit
// has no documented empty result. A non-positive limit is an error that
// names it, returned before any statement reaches SQLite, where LIMIT 0
// would read nothing and a negative LIMIT would read everything.
func TestNonPositiveLimitIsRefusedBeforeSQL(t *testing.T) {
	s, counts := openObservedStore(t)
	for _, limit := range []int{0, -1} {
		named := fmt.Sprintf("limit %d ", limit)
		for _, tc := range []struct {
			name, want string
			call       func() error
		}{
			{"RowsStoringServedSubagentKeys", named, func() error {
				_, err := s.RowsStoringServedSubagentKeys(limit)
				return err
			}},
			{"NextThreadTransferJobs", named, func() error {
				_, err := s.NextThreadTransferJobs(limit)
				return err
			}},
			// The batch reads one reference past the row budget.
			{"unsealThreadHistoryBatch", named, func() error {
				_, _, err := s.unsealThreadHistoryBatch("t", historyRepairBudget{rows: limit - 1, piece: 16, bytes: 1 << 20, time: time.Hour})
				return err
			}},
			{"RecomputeSubagentAggregates", named, func() error {
				_, err := s.RecomputeSubagentAggregates(context.Background(), "t", limit)
				return err
			}},
			// The walk reads one member past the cap.
			{"scanWorkItemTreeRuns", fmt.Sprintf("got 4 and %d", limit), func() error {
				return scanWorkItemTreeRuns(s.reader(), "root", 4, limit, func(WorkItemTreeRun) error { return nil })
			}},
			{"timelineKeyedIDSelection", named, func() error {
				_, _, err := timelineKeyedIDSelection(s.reader(), "t", "items.created_at AS created_at",
					"items.id = ?", []any{"x"}, "created_at DESC", limit)
				return err
			}},
		} {
			before := counts.snapshot()
			err := tc.call()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s(%d) = %v, want an error naming %q", tc.name, limit, err, strings.TrimSpace(tc.want))
			}
			if ran := statementsSince(counts, before); len(ran) > 0 {
				t.Errorf("%s(%d) ran SQL:\n%s", tc.name, limit, strings.Join(ran, "\n"))
			}
		}
	}
}

// TestNonPositiveLimitReadsNothing covers the accessors documented to
// return nothing for a non-positive limit: they return before any
// statement reaches SQLite.
func TestNonPositiveLimitReadsNothing(t *testing.T) {
	s, counts := openObservedStore(t)
	for _, limit := range []int{0, -1} {
		for _, tc := range []struct {
			name string
			call func() (int, error)
		}{
			{"ListPairingLinksForUser", func() (int, error) {
				got, err := s.ListPairingLinksForUser("u", limit)
				return len(got), err
			}},
			{"ListRecentTurns", func() (int, error) {
				got, err := s.ListRecentTurns("t", limit)
				return len(got), err
			}},
			{"ListRecentAuthAudit", func() (int, error) {
				got, err := s.ListRecentAuthAudit(limit)
				return len(got), err
			}},
			{"ListAuthAuditForDevice", func() (int, error) {
				got, err := s.ListAuthAuditForDevice("d", limit)
				return len(got), err
			}},
			{"ResolveThreadPrefix", func() (int, error) {
				got, err := s.ResolveThreadPrefix("abc", limit)
				return len(got), err
			}},
			{"ListThreadUserMessageHistory", func() (int, error) {
				got, err := s.ListThreadUserMessageHistory("t", limit)
				return len(got), err
			}},
			{"ListItemsInRange", func() (int, error) {
				got, err := s.ListItemsInRange("t", TimelineCursor{}, TimelineCursor{TurnIndex: 1 << 30}, limit, true)
				return len(got), err
			}},
			{"ThreadTitleContextItems", func() (int, error) {
				got, _, err := s.ThreadTitleContextItems("t", limit)
				return len(got), err
			}},
		} {
			before := counts.snapshot()
			if n, err := tc.call(); n != 0 || err != nil {
				t.Errorf("%s(%d) = %d rows, %v; want none and no error", tc.name, limit, n, err)
			}
			if ran := statementsSince(counts, before); len(ran) > 0 {
				t.Errorf("%s(%d) ran SQL:\n%s", tc.name, limit, strings.Join(ran, "\n"))
			}
		}
	}
}

// TestNonPositiveLimitReadsTheDefaultPage covers the accessors with a
// default page size: a non-positive limit reads that page, so the SQL
// carries the default and never LIMIT 0 or a negative LIMIT.
func TestNonPositiveLimitReadsTheDefaultPage(t *testing.T) {
	s, counts := openObservedStore(t)
	now := time.Now().UnixMilli()
	nonPositive := regexp.MustCompile(`LIMIT\s+(0\b|-)`)
	for _, limit := range []int{0, -1} {
		for _, tc := range []struct {
			name, want string
			call       func() error
		}{
			{"SearchThreads", "LIMIT 20 OFFSET ?", func() error {
				_, err := s.SearchThreads("hello", ThreadSearchFilter{Limit: limit})
				return err
			}},
			{"ListThreadsByActivity", "LIMIT 20 OFFSET ?", func() error {
				_, err := s.ListThreadsByActivity(ThreadSearchFilter{Limit: limit})
				return err
			}},
			{"ListRemoteWatches of a thread", "LIMIT 100", func() error {
				_, err := s.ListRemoteWatches("t", 0, limit)
				return err
			}},
			{"ListRemoteWatches due", "LIMIT 100", func() error {
				_, err := s.ListRemoteWatches("", now, limit)
				return err
			}},
			{"ListThreadRequestsByCaller", "LIMIT 20 OFFSET ?", func() error {
				_, err := s.ListThreadRequestsByCaller("t", limit, 0)
				return err
			}},
			{"ListOpenThreadRequestsForComputer", "LIMIT 50", func() error {
				_, err := s.ListOpenThreadRequestsForComputer("c", limit)
				return err
			}},
			{"DueThreadReminders", "LIMIT 32", func() error {
				_, err := s.DueThreadReminders(now, limit)
				return err
			}},
			{"DueThreadRequestPolls", "LIMIT 32", func() error {
				_, err := s.DueThreadRequestPolls(now, limit)
				return err
			}},
			{"UndeliveredThreadRequestWakes", "LIMIT 32", func() error {
				_, err := s.UndeliveredThreadRequestWakes(now, limit)
				return err
			}},
		} {
			before := counts.snapshot()
			if err := tc.call(); err != nil {
				t.Errorf("%s(%d): %v", tc.name, limit, err)
				continue
			}
			ran := statementsSince(counts, before)
			found := false
			for _, query := range ran {
				found = found || strings.Contains(query, tc.want)
				if bad := nonPositive.FindString(query); bad != "" {
					t.Errorf("%s(%d) ran %q:\n%s", tc.name, limit, bad, query)
				}
			}
			if !found {
				t.Errorf("%s(%d) ran no statement with %q:\n%s", tc.name, limit, tc.want, strings.Join(ran, "\n"))
			}
		}
	}
}

// TestChannelMessageLimit pins that ListChannelMessages reads every
// message, with no LIMIT clause, for a non-positive limit, and renders a
// positive limit into the text.
func TestChannelMessageLimit(t *testing.T) {
	s, counts := openObservedStore(t)
	for _, tc := range []struct {
		limit int
		want  string
	}{{0, ""}, {-1, ""}, {5, "LIMIT 5"}} {
		before := counts.snapshot()
		if _, err := s.ListChannelMessages("ch", -1, tc.limit); err != nil {
			t.Fatal(err)
		}
		ran := statementsSince(counts, before)
		if len(ran) != 1 {
			t.Fatalf("limit %d ran %d statements, want 1:\n%s", tc.limit, len(ran), strings.Join(ran, "\n"))
		}
		query := ran[0]
		if tc.want == "" && strings.Contains(query, "LIMIT") {
			t.Errorf("limit %d rendered a LIMIT clause:\n%s", tc.limit, query)
		}
		if tc.want != "" && !strings.HasSuffix(query, tc.want) {
			t.Errorf("limit %d does not end in %q:\n%s", tc.limit, tc.want, query)
		}
	}
}

// TestTimelineSelectionLimit pins how timelineArms renders Limit: a
// negative one is an error before the lineage read, zero renders no LIMIT
// clause, and a positive one is a literal that binds nothing.
func TestTimelineSelectionLimit(t *testing.T) {
	s, counts := openObservedStore(t)
	sel := timelineSelection{
		Columns:   timelineIDColumns,
		Where:     "items.kind = ?",
		WhereArgs: []any{"user_text"},
		OrderBy:   "turn_index DESC, item_index DESC",
	}

	sel.Limit = -1
	before := counts.snapshot()
	if _, _, err := timelineArms(s.reader(), "t", sel); err == nil || !strings.Contains(err.Error(), "limit -1 ") {
		t.Errorf("Limit -1: %v, want an error naming the limit", err)
	}
	if ran := statementsSince(counts, before); len(ran) > 0 {
		t.Errorf("Limit -1 ran SQL:\n%s", strings.Join(ran, "\n"))
	}

	sel.Limit = 0
	unlimited, unlimitedArgs, err := timelineArms(s.reader(), "t", sel)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(unlimited, "LIMIT") {
		t.Errorf("Limit 0 rendered a LIMIT clause:\n%s", unlimited)
	}

	sel.Limit = 7
	limited, limitedArgs, err := timelineArms(s.reader(), "t", sel)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(limited, "LIMIT 7") {
		t.Errorf("Limit 7 does not end in a literal LIMIT 7:\n%s", limited)
	}
	if !slices.Equal(limitedArgs, unlimitedArgs) || strings.Count(limited, "?") != len(limitedArgs) {
		t.Errorf("Limit 7 binds %v for %d placeholders; want the unlimited selection's %v",
			limitedArgs, strings.Count(limited, "?"), unlimitedArgs)
	}
	rows, err := s.reader().Query(limited, limitedArgs...)
	if err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
}
