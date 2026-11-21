package store

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The mirror at the size of a large workspace: the store's part of a first
// sync, each per-tick, per-open and per-page path, process start, and the
// plans, at the board sizes AO_TRACKER_SCALE names (a comma list), each as
// one board and, for the largest, as five boards sharing the root. Each
// path reports its statements and rows written (exact) and the median and
// spread of its time over scaleReps runs. Unset, the test is skipped.
//
//	AO_TRACKER_SCALE=5000,20000 make go-test GO_TEST_PKGS=./internal/store \
//	  GO_TEST_FLAGS="-count=1 -timeout 60m -v -run TestMirrorAtScale"
func TestMirrorAtScale(t *testing.T) {
	sizes := trackerScaleSizes(t)
	for i, n := range sizes {
		cases := []benchMirrorSize{{boards: 1, issues: n}}
		if i == len(sizes)-1 {
			cases = append(cases, benchMirrorSize{boards: 5, issues: n / 5})
		}
		for _, size := range cases {
			t.Run(size.String(), func(t *testing.T) { mirrorAtScale(t, size) })
		}
	}
}

// trackerScaleSizes are the board sizes AO_TRACKER_SCALE names; the test
// is skipped when it names none.
func trackerScaleSizes(t *testing.T) []int {
	t.Helper()
	env := os.Getenv("AO_TRACKER_SCALE")
	if env == "" {
		t.Skip("AO_TRACKER_SCALE names no sizes")
	}
	var sizes []int
	for field := range strings.SplitSeq(env, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || n < 100 {
			t.Fatalf("AO_TRACKER_SCALE=%q: each size is a whole number of at least 100", env)
		}
		sizes = append(sizes, n)
	}
	return sizes
}

// scaleReps is how many times each path runs; the statements and rows are
// the last run's.
const scaleReps = 5

// scaleCost is one path's measured cost.
type scaleCost struct {
	statements int
	writes     map[string]int
	times      []time.Duration
}

func (c scaleCost) String() string {
	sorted := slices.Sorted(slices.Values(c.times))
	return fmt.Sprintf("%9s (%s to %s) %5d statements, rows written %v",
		sorted[len(sorted)/2].Round(time.Microsecond), sorted[0].Round(time.Microsecond), sorted[len(sorted)-1].Round(time.Microsecond),
		c.statements, c.writes)
}

// measureScale runs op scaleReps times, rep 0 first, each after its
// untimed and uncounted prepare (if any).
func measureScale(t *testing.T, rec *mirrorWriteRecorder, prepare func(rep int), op func(rep int) error) scaleCost {
	t.Helper()
	var c scaleCost
	for rep := range scaleReps {
		if prepare != nil {
			prepare(rep)
		}
		rec.reset()
		var err error
		start := time.Now()
		c.statements = countMirrorStatements(t, func() { err = op(rep) })
		c.times = append(c.times, time.Since(start))
		if err != nil {
			t.Fatalf("rep %d: %v", rep, err)
		}
		c.writes = rec.rowWrites()
	}
	return c
}

func mirrorAtScale(t *testing.T, size benchMirrorSize) {
	issues := size.boards * size.issues
	path := filepath.Join(t.TempDir(), "store.sqlite")
	if err := copyTestStoreTemplate(path); err != nil {
		t.Fatal(err)
	}
	base, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The store's part of a first sync: each board's structure, row pages
	// and histories, through the writers a sync calls.
	filled, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := filled.Close(); err != nil {
			t.Error(err)
		}
	})
	rec := recordMirrorWrites(t, filled)
	start := time.Now()
	statements := countMirrorStatements(t, func() { err = fillBenchMirror(filled, size) })
	if err != nil {
		t.Fatal(err)
	}
	fill := time.Since(start)
	t.Logf("first sync's store work: %s, %d statements, rows written %v", fill.Round(time.Millisecond), statements, rec.rowWrites())
	if cp, err := filled.TruncateCheckpoint(); err != nil || cp.Busy {
		t.Fatalf("checkpoint: %+v, %v", cp, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("main file after a checkpoint: %d bytes, %d bytes an issue over the empty store's %d", info.Size(),
		(info.Size()-base.Size())/int64(issues), base.Size())
	// The paths run on a copy, opened as a process start opens it.
	path = filepath.Join(t.TempDir(), "copy.sqlite")
	if err := copyStoreFile(filled.path, path); err != nil {
		t.Fatal(err)
	}
	var s *Store
	var opens []time.Duration
	for range scaleReps {
		start := time.Now()
		if s, err = New(path); err != nil {
			t.Fatal(err)
		}
		opens = append(opens, time.Since(start))
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%-34s %s", "store.New", scaleCost{times: opens, writes: map[string]int{}})
	if s, err = New(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	root, ok, err := s.GetMirrorRoot(benchConnectionID(t, s))
	if err != nil || !ok {
		t.Fatalf("root: %v, %v", ok, err)
	}
	rec = recordMirrorWrites(t, s)
	paths := scalePaths(t, s, root, size, rec)
	for _, name := range slices.Sorted(maps.Keys(paths)) {
		t.Logf("%-34s %s", name, paths[name])
	}
	mirrorPlansAtScale(t, s)
}

// scalePaths measures each path on board 0 of a mirror of size.
func scalePaths(t *testing.T, s *Store, root int64, size benchMirrorSize, rec *mirrorWriteRecorder) map[string]scaleCost {
	t.Helper()
	board := benchBoardID(0)
	lifetime, reset, sweep := currentLifetime(s, root, board), currentEstimateReset(s, root, board), currentSweep(s, root)
	at := int64(10)
	next := func() int64 { at++; return at }
	syncState := func(board string) error { _, _, err := s.GetMirrorSync(root, board, testNow); return err }
	record := func() error {
		_, err := s.RecordMirrorSyncAttempt(root, board, MirrorSyncAttempt{Lifetime: lifetime, ReadAt: next()})
		return err
	}
	// The rows of a delta that changed k issues spread over the board, at
	// an update time of k's and rep's.
	changed := func(k, rep int) []MirrorRow {
		rows := make([]MirrorRow, k)
		for r := range rows {
			rows[r] = benchRow(0, r*(size.issues/k), int64(10_000*k+rep))
		}
		return rows
	}
	// settle reads the history of every issue due one, so a path finds
	// only the issues it makes due.
	settle := func() {
		for {
			listed, _, err := s.ListMirrorHistoryDue(root, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if len(listed) == 0 {
				return
			}
			for part := range slices.Chunk(listed, mirrorHistoryBatch) {
				batch := make([]MirrorHistory, len(part))
				for i, d := range part {
					batch[i] = benchHistory(d.IssueID, 100, benchChanges)
					batch[i].For, batch[i].BoardsRev, batch[i].Receipt = d.UpdatedAt, d.BoardsRev, d.Receipt
				}
				if _, _, err := s.ReplaceMirrorHistories(root, sweep, batch, MirrorCatalog{}, 0); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	delta := func(k int) func(rep int) error {
		return func(rep int) error {
			if err := syncState(board); err != nil {
				return err
			}
			rows := changed(k, rep)
			page := MirrorRowPage{BoardID: board, Lifetime: lifetime, EstimateResetAt: reset, Rows: rows, ReadAt: next()}
			got, err := s.WriteMirrorRowPage(root, page, fmt.Appendf(nil, "cursor-%d", rep))
			if err := benchApplied(got, err, k, 0); err != nil {
				return err
			}
			if err := record(); err != nil {
				return err
			}
			// The change funnel's lookup of the boards of the changed cards
			// and of their children.
			ids := got.Cards
			_, err = s.ListMirrorIssueBoards(root, ids, ids)
			return err
		}
	}
	sections := []MirrorSection{{Kind: MirrorSectionColumn}, {Kind: MirrorSectionBacklog}, {Kind: MirrorSectionSprint, SprintID: strconv.Itoa(benchBoardSprints - 1)}}
	listing := make([]string, size.issues)
	for i := range listing {
		listing[i] = benchIssueID(0, i)
	}
	tickets := listing[:100]
	doc := func(id string, rev int) []byte {
		return append(bytes.Repeat([]byte("d"), 16_000), fmt.Sprintf("%s-%d", id, rev)...)
	}
	docs := func(ids []string, rev int) []MirrorDetailDoc {
		out := make([]MirrorDetailDoc, len(ids))
		for i, id := range ids {
			out[i] = MirrorDetailDoc{IssueID: id, Data: doc(id, rev)}
		}
		return out
	}
	if _, err := s.StoreMirrorDetails(root, sweep, docs(tickets, 0), 7, 7); err != nil {
		t.Fatal(err)
	}
	// held are the tickets' cards as the mirror holds them once the deltas
	// ran, which an idle round reads again.
	var held []MirrorCard
	cards := func(n int) []MirrorCard { return slices.Clone(held[:n]) }
	first, err := s.ListMirrorSection(root, board, sections[0], MirrorRowKey{})
	if err != nil {
		t.Fatal(err)
	}
	middle := (size.issues / 2) - (size.issues/2)%5
	deep := MirrorRowKey{OrderKey: fmt.Sprintf("0|i%05d:", middle), IssueID: benchIssueID(0, middle), Lifetime: first.Next.Lifetime}
	out := map[string]scaleCost{}
	out["idle tick"] = measureScale(t, rec, nil, func(int) error {
		if err := syncState(board); err != nil {
			return err
		}
		return record()
	})
	out["delta tick, 1 changed"] = measureScale(t, rec, nil, delta(1))
	out["delta tick, 100 changed"] = measureScale(t, rec, nil, delta(100))
	out["reconcile, complete listing"] = measureScale(t, rec, nil, func(int) error {
		// The settled bound is the service's: a minute before the read.
		at := next()
		got, err := s.ReconcileMirrorBoard(root, board, lifetime, listing, true, at, at-60_000)
		if err == nil && len(got.Missing)+len(got.Removed)+len(got.Unsettled) > 0 {
			err = fmt.Errorf("missing %d, removed %d, unsettled %d", len(got.Missing), len(got.Removed), len(got.Unsettled))
		}
		return err
	})
	due := listing[size.issues/2 : size.issues/2+mirrorHistoryBatch]
	out["history, 50 due"] = measureScale(t, rec, func(int) {
		settle()
		list, err := encodeMirrorIDs(due)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, s.db, `UPDATE tracker_cache_issues SET history_for = 0 WHERE root_id = ? AND issue_id IN (SELECT value FROM json_each(?))`, root, list)
	}, func(rep int) error {
		listed, _, err := s.ListMirrorHistoryDue(root, testNow)
		if err != nil {
			return err
		}
		batch := make([]MirrorHistory, len(listed))
		for i, d := range listed {
			batch[i] = benchHistory(d.IssueID, int64(3000+rep), benchChanges)
			batch[i].For, batch[i].BoardsRev, batch[i].Receipt = d.UpdatedAt, d.BoardsRev, d.Receipt
		}
		_, _, err = s.ReplaceMirrorHistories(root, sweep, batch, MirrorCatalog{}, 0)
		return err
	})
	out["board view, 3 sections"] = measureScale(t, rec, nil, func(int) error {
		_, _, err := s.ReadMirrorBoardView(root, board, sections)
		return err
	})
	out["section page, middle of a column"] = measureScale(t, rec, func(int) {
		page, err := s.ListMirrorSection(root, board, sections[0], MirrorRowKey{})
		if err != nil {
			t.Fatal(err)
		}
		deep.Revision = page.Next.Revision
	}, func(int) error {
		_, err := s.ListMirrorSection(root, board, sections[0], deep)
		return err
	})
	out["section counts"] = measureScale(t, rec, nil, func(int) error { _, err := s.CountMirrorSections(root, board); return err })
	counts, err := s.CountMirrorSections(root, board)
	if err != nil {
		t.Fatal(err)
	}
	// A change that moved a total, and one that did not: the caller holds
	// the totals the patch would read.
	for _, held := range []MirrorTotalsAt{{}, {Lifetime: lifetime, Revision: counts.TotalsRevision}} {
		kind := map[bool]string{true: "totals moved", false: "totals held"}[held == MirrorTotalsAt{}]
		for _, k := range []int{1, 100} {
			out[fmt.Sprintf("patch read, %d changed, %s", k, kind)] = measureScale(t, rec, nil, func(rep int) error {
				patch, _, err := s.ReadMirrorBoardPatch(root, board, sections, listing[:k], held)
				if err == nil && (patch.Counts == nil) != (held != MirrorTotalsAt{}) {
					err = fmt.Errorf("held %+v, read totals %v", held, patch.Counts != nil)
				}
				return err
			})
		}
	}
	if held, err = s.ListMirrorCards(root, tickets); err != nil || len(held) != len(tickets) {
		t.Fatalf("%d cards of %d tickets: %v", len(held), len(tickets), err)
	}
	out["ticket open, stored and verified"] = measureScale(t, rec, nil, func(int) error {
		if err := syncState(""); err != nil {
			return err
		}
		d, ok, err := s.GetMirrorDetail(root, tickets[0])
		if err != nil || !ok {
			return fmt.Errorf("detail held %v: %v", ok, err)
		}
		if _, err := s.GetMirrorCatalog(root, []string{"acc-user1"}, []string{"1"}); err != nil {
			return err
		}
		if _, err := s.ListMirrorCards(root, tickets[:4]); err != nil {
			return err
		}
		if _, err := s.UpsertMirrorCards(root, sweep, cards(1), MirrorCatalog{}, next()); err != nil {
			return err
		}
		_, err = s.ConfirmMirrorDetail(root, d, next())
		return err
	})
	jira := func(changedTickets int) func(rep int) error {
		return func(rep int) error {
			if err := syncState(""); err != nil {
				return err
			}
			if _, err := s.ListMirrorDetailStates(root, tickets); err != nil {
				return err
			}
			read := cards(len(tickets))
			for i := range changedTickets {
				read[i*10].UpdatedAt = int64(1_000_000 + rep)
			}
			if _, err := s.UpsertMirrorCards(root, sweep, read, MirrorCatalog{}, next()); err != nil || changedTickets == 0 {
				return err
			}
			var ids []string
			for i := range changedTickets {
				ids = append(ids, read[i*10].IssueID)
			}
			_, err := s.StoreMirrorDetails(root, sweep, docs(ids, 1+rep), 0, next())
			return err
		}
	}
	linear := func(changedTickets int) func(rep int) error {
		return func(rep int) error {
			if err := syncState(""); err != nil {
				return err
			}
			read := docs(tickets, 0)
			for i := range changedTickets {
				read[i*10].Data = doc(tickets[i*10], 100+rep)
			}
			_, err := s.StoreMirrorDetails(root, sweep, read, 0, next())
			return err
		}
	}
	out["Jira round of 100, idle"] = measureScale(t, rec, nil, jira(0))
	out["Jira round of 100, 10 changed"] = measureScale(t, rec, nil, jira(10))
	out["Linear round of 100, idle"] = measureScale(t, rec, nil, linear(0))
	out["Linear round of 100, 10 changed"] = measureScale(t, rec, nil, linear(10))
	out["cleanup pass, nothing pending"] = measureScale(t, rec, nil, func(int) error { return s.FinishMirrorCleanup(nil) })
	return out
}

// mirrorPlansAtScale checks that every mirror statement this process ran,
// with each mirror trigger's, plans on s as it plans on a small mirror,
// and that the plan reads by key: no store database has sqlite_stat1, so
// a plan depends on the schema and not on how many rows a table holds.
func mirrorPlansAtScale(t *testing.T, s *Store) {
	t.Helper()
	small, jira, root := newMirror(t)
	runEveryMirrorAccessor(t, small, jira, root)
	var queries []string
	mirrorStatements.Range(func(query, _ any) bool {
		queries = append(queries, query.(string))
		return true
	})
	slices.Sort(queries)
	differ := map[string]bool{}
	for _, query := range queries {
		if plan := explainMirror(t, s, query); plan != explainMirror(t, small, query) {
			differ[strings.TrimSpace(query)] = true
			t.Errorf("%s\nplans otherwise at scale:\n%s", strings.TrimSpace(query), plan)
		}
		violations := slices.DeleteFunc(mirrorPlanViolations(t, s, query), func(line string) bool { return mirrorPlanLineAllowed(query, line) })
		if len(violations) > 0 {
			t.Errorf("%s\nplan lines that scan or sort at scale:\n%s", strings.TrimSpace(query), strings.Join(violations, "\n"))
		}
	}
	t.Logf("plans: %d statements, %d plan otherwise than on a small mirror %v", len(queries), len(differ), slices.Sorted(maps.Keys(differ)))
}
