package store

import (
	"bytes"
	"context"
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
	if err := copyTestStoreTemplate(t, path); err != nil {
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
	filledRec := recordMirrorWrites(t, filled)
	rec := filledRec
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
	support := supportAtScale(t, s, root, rec)
	for _, name := range slices.Sorted(maps.Keys(support)) {
		t.Logf("%-58s %s", name, support[name])
	}
	readyListedAgainAtScale(t, filled, filledRec)
	mirrorPlansAtScale(t, s)
}

// readyListedAgainAtScale measures, once, on the first sync's store s,
// whose copy the paths ran on, what a save that lists board 0 again waits
// for while its retired copy holds every row of the sync and the root
// stays live: ReadyTrackerBoards of that board.
func readyListedAgainAtScale(t *testing.T, s *Store, rec *mirrorWriteRecorder) {
	t.Helper()
	listed, err := s.ListProjectTrackerBoards(defaultTestProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := replaceFromStored(s, defaultTestProjectID, listed[1:]); err != nil {
		t.Fatal(err)
	}
	_, listing, err := replaceFromStored(s, defaultTestProjectID, listed)
	if err != nil || len(listing.Relisted) != 1 {
		t.Fatalf("listing board 0 again: %+v, %v", listing, err)
	}
	rec.reset()
	start := time.Now()
	statements := countMirrorStatements(t, func() { err = s.ReadyTrackerBoards(context.Background(), listing.Relisted) })
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	most, total := rec.mostDeletedInATransaction()
	t.Logf("%-34s %9s %5d statements, %d rows deleted, at most %d in a transaction", "ready a board listed again",
		took.Round(time.Microsecond), statements, total, most)
}

// benchRegroup is a project's regrouping of 30 statuses of the bench
// mirror's site: 28 its boards do not use and, when used, its in-progress
// status as done and its canceled one left out of counts, or two more it
// does not use.
func benchRegroup(t *testing.T, used bool) MirrorRegroup {
	t.Helper()
	groups := map[string]int{"unused-a": MirrorGroupDone, "unused-b": MirrorGroupLeftOut}
	if used {
		groups = map[string]int{"3": MirrorGroupDone, "5": MirrorGroupLeftOut}
	}
	for i := range 28 {
		groups[fmt.Sprint("unused-", i)] = 1 + i%MirrorGroupLeftOut
	}
	regroup, err := NewMirrorRegroup(groups)
	if err != nil {
		t.Fatal(err)
	}
	return regroup
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
		_, _, err := s.RecordMirrorSyncAttempt(root, board, MirrorSyncAttempt{Lifetime: lifetime, ReadAt: mirrorReadAt(next())})
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
				if _, _, err := s.ReplaceMirrorHistories(root, sweep, batch, MirrorCatalog{}, nextMirrorRead(s, root)); err != nil {
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
			page := MirrorRowPage{BoardID: board, Lifetime: lifetime, EstimateResetAt: reset, Rows: rows, ReadAt: mirrorReadAt(next())}
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
	// held are the tickets' cards as the mirror holds them, which an idle
	// round reads again.
	var held []MirrorCard
	heldCards := func() {
		var err error
		if held, err = s.ListMirrorCards(root, tickets); err != nil || len(held) != len(tickets) {
			t.Fatalf("%d cards of %d tickets: %v", len(held), len(tickets), err)
		}
		// A card read leaves the labels out (MirrorCard.Labels); the
		// tracker gives them to every card it writes.
		for i := range held {
			held[i].Labels = slices.Clone(benchLabels)
		}
	}
	ticketsOf := func(cards []MirrorCard, rev int) []MirrorTicket {
		out := make([]MirrorTicket, len(cards))
		for i, c := range cards {
			out[i] = MirrorTicket{Card: c, Body: doc(c.IssueID, rev)}
		}
		return out
	}
	heldCards()
	if _, err := s.StoreMirrorTickets(root, sweep, ticketsOf(held, 0), MirrorCatalog{}, mirrorReadAt(7), mirrorReadAt(7)); err != nil {
		t.Fatal(err)
	}
	cards := func(n int) []MirrorCard { return slices.Clone(held[:n]) }
	first, err := s.ListMirrorSection(root, board, MirrorRegroup{}, sections[0], MirrorRowKey{})
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
		at := next()
		got, err := s.ReconcileMirrorBoard(root, board, lifetime, listing, true, mirrorReadAt(at))
		if err == nil && len(got.Missing)+len(got.Unsettled) > 0 {
			err = fmt.Errorf("missing %d, unsettled %d", len(got.Missing), len(got.Unsettled))
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
		_, _, err = s.ReplaceMirrorHistories(root, sweep, batch, MirrorCatalog{}, mirrorReadOf(root, int64(1_000_000+rep)))
		return err
	})
	// The board's reads, with no regrouping and with a project's 30
	// regrouped statuses: none of them the board's, or two of them, one
	// left out of counts (a fifth of the board's rows).
	for _, regrouped := range []struct {
		name    string
		regroup MirrorRegroup
	}{{"", MirrorRegroup{}}, {", 30 regrouped, none used", benchRegroup(t, false)}, {", 30 regrouped, 2 used", benchRegroup(t, true)}} {
		regroup := regrouped.regroup
		out["board view, 3 sections"+regrouped.name] = measureScale(t, rec, nil, func(int) error {
			_, _, err := s.ReadMirrorBoardView(root, board, regroup, theseSections(sections))
			return err
		})
		out["section page, middle of a column"+regrouped.name] = measureScale(t, rec, func(int) {
			page, err := s.ListMirrorSection(root, board, regroup, sections[0], MirrorRowKey{})
			if err != nil {
				t.Fatal(err)
			}
			deep.Revision = page.Next.Revision
		}, func(int) error {
			_, err := s.ListMirrorSection(root, board, regroup, sections[0], deep)
			return err
		})
		out["section page, backlog"+regrouped.name] = measureScale(t, rec, nil, func(int) error {
			_, err := s.ListMirrorSection(root, board, regroup, MirrorSection{Kind: MirrorSectionBacklog}, MirrorRowKey{})
			return err
		})
		out["pace, last 5 sprints"+regrouped.name] = measureScale(t, rec, nil, func(int) error {
			pace, err := mirrorReadSnapshot(s, "pace", func(tx sqlQueryer) ([]MirrorSprintFinished, error) {
				shown, _, err := readMirrorShownBoardTx(tx, root, board)
				if err != nil {
					return nil, err
				}
				return readMirrorPaceTx(tx, root, board, shown, regroup)
			})
			if err == nil && len(pace) != MirrorPaceSprints {
				err = fmt.Errorf("pace of %d sprints, want %d", len(pace), MirrorPaceSprints)
			}
			return err
		})
		out["section counts"+regrouped.name] = measureScale(t, rec, nil, func(int) error {
			_, err := s.CountMirrorSections(root, board, regroup)
			return err
		})
		counts, err := s.CountMirrorSections(root, board, regroup)
		if err != nil {
			t.Fatal(err)
		}
		// A change that moved a total, and one that did not: the caller holds
		// the totals the patch would read.
		for _, held := range []MirrorTotalsAt{{}, {Lifetime: lifetime, Revision: counts.TotalsRevision}} {
			kind := map[bool]string{true: "totals moved", false: "totals held"}[held == MirrorTotalsAt{}]
			for _, k := range []int{1, 100} {
				out[fmt.Sprintf("patch read, %d changed, %s%s", k, kind, regrouped.name)] = measureScale(t, rec, nil, func(rep int) error {
					patch, _, err := s.ReadMirrorBoardPatch(root, board, regroup, sections, listing[:k], held)
					if err == nil && (patch.Counts == nil) != (held != MirrorTotalsAt{}) {
						err = fmt.Errorf("held %+v, read totals %v", held, patch.Counts != nil)
					}
					return err
				})
			}
		}
	}
	// The rows a view build of MirrorMaxBodies open tickets reads, spread
	// over the board.
	ticketIDs := make([]string, MirrorMaxBodies)
	for i := range ticketIDs {
		ticketIDs[i] = benchIssueID(0, i*(size.issues/MirrorMaxBodies))
	}
	out["ticket rows, 16 tickets"] = measureScale(t, rec, nil, func(int) error {
		rows, err := s.ListMirrorTicketRows(root, ticketIDs)
		if err == nil && len(rows) != len(ticketIDs) {
			err = fmt.Errorf("%d rows of %d tickets", len(rows), len(ticketIDs))
		}
		return err
	})
	// The deltas moved some of the tickets' cards.
	heldCards()
	out["ticket open, stored and verified"] = measureScale(t, rec, nil, func(int) error {
		if err := syncState(""); err != nil {
			return err
		}
		if bodies, err := s.ListMirrorBodies(root, tickets[:1]); err != nil || len(bodies) != 1 {
			return fmt.Errorf("bodies held %d: %v", len(bodies), err)
		}
		if _, err := s.GetMirrorCatalog(root, []string{"acc-user1"}, []string{"1"}); err != nil {
			return err
		}
		if _, err := s.ListMirrorCards(root, tickets[:4]); err != nil {
			return err
		}
		_, err := s.StoreMirrorTickets(root, sweep, []MirrorTicket{{Card: cards(1)[0]}}, MirrorCatalog{}, mirrorReadAt(next()), mirrorReadAt(next()))
		return err
	})
	// A round's inputs are built before its measured section: the tickets
	// the tracker answered, changedTickets of them (every tenth) at a new
	// update time with a new body: since, plus the rep, after every update
	// an earlier path stored.
	var roundTickets, roundChanged []MirrorTicket
	roundInputs := func(changedTickets int, since int64) func(rep int) {
		return func(rep int) {
			heldCards()
			roundTickets, roundChanged = ticketsOf(cards(len(tickets)), 0), nil
			for i := range changedTickets {
				at := i * 10
				roundTickets[at].Card.UpdatedAt = since + int64(rep)
				roundTickets[at].Body = doc(tickets[at], 1+rep)
				roundChanged = append(roundChanged, roundTickets[at])
			}
		}
	}
	// A Jira round checks every ticket with one cards read, then stores
	// the tickets of those that changed, read in full.
	jira := func(rep int) error {
		if err := syncState(""); err != nil {
			return err
		}
		checks := make([]MirrorTicket, len(roundTickets))
		for i, rt := range roundTickets {
			checks[i] = MirrorTicket{Card: rt.Card}
		}
		if _, err := s.StoreMirrorTickets(root, sweep, checks, MirrorCatalog{}, MirrorRead{}, mirrorReadAt(next())); err != nil || len(roundChanged) == 0 {
			return err
		}
		_, err := s.StoreMirrorTickets(root, sweep, roundChanged, MirrorCatalog{}, MirrorRead{}, mirrorReadAt(next()))
		return err
	}
	// A Linear round reads every ticket in full and stores them in one
	// write.
	linear := func(rep int) error {
		if err := syncState(""); err != nil {
			return err
		}
		_, err := s.StoreMirrorTickets(root, sweep, roundTickets, MirrorCatalog{}, MirrorRead{}, mirrorReadAt(next()))
		return err
	}
	out["Jira round of 100, idle"] = measureScale(t, rec, roundInputs(0, 0), jira)
	out["Jira round of 100, 10 changed"] = measureScale(t, rec, roundInputs(10, 1_000_000), jira)
	out["Linear round of 100, idle"] = measureScale(t, rec, roundInputs(0, 0), linear)
	out["Linear round of 100, 10 changed"] = measureScale(t, rec, roundInputs(10, 2_000_000), linear)
	// Credit's listing of the acts due on every board of the root: with
	// the board's own groups, under which no sprint member is done, and
	// with the status every member holds at its completion regrouped as
	// done, so each is a candidate; with a support rule naming every
	// board, walked over every row (a window from 0), first while every
	// row is estimated, so none is support, then with no row estimated,
	// so every row is; with no receipt stored (first use) and with a
	// receipt of each source for every issue (the steady state).
	everyDone, err := NewMirrorRegroup(map[string]int{"5": MirrorGroupDone})
	if err != nil {
		t.Fatal(err)
	}
	var everyBoard []string
	for b := range size.boards {
		everyBoard = append(everyBoard, benchBoardID(b))
	}
	supportAll, err := MirrorRegroup{}.WithSupport(MirrorSupportRule{Boards: everyBoard})
	if err != nil {
		t.Fatal(err)
	}
	for _, credit := range []struct {
		name    string
		regroup MirrorRegroup
		reset   bool
	}{{"", MirrorRegroup{}, false}, {", every member done", everyDone, false}, {", support by board, every row estimated", supportAll, false},
		{", support by board, no row estimated", supportAll, true}} {
		if credit.reset {
			mustExec(t, s.db, `UPDATE tracker_cache_boards SET estimate_reset_at = estimate_reset_at + 1000000000000 WHERE root_id = ?`, root)
		}
		boards := make([]MirrorActsBoard, size.boards)
		for b := range boards {
			boards[b] = MirrorActsBoard{BoardID: benchBoardID(b), Regroup: credit.regroup}
		}
		listed := 0
		list := func(int) error {
			due, _, err := s.ListMirrorActsDue(root, boards, 0, testNow)
			listed = len(due)
			return err
		}
		out["acts due, none read"+credit.name] = measureScale(t, rec, func(int) {
			mustExec(t, s.db, `DELETE FROM tracker_cache_issue_act_reads`)
		}, list)
		t.Logf("acts due, none read%s: %d listed", credit.name, listed)
		out["acts due, every one read"+credit.name] = measureScale(t, rec, func(int) {
			mustExec(t, s.db, `INSERT OR REPLACE INTO tracker_cache_issue_act_reads(root_id, issue_id, source, read_for, read_at, read_wall)
				SELECT root_id, issue_id, src.value, updated_at, 1, ? FROM tracker_cache_issues, json_each('[1,2]') src WHERE root_id = ?`, testNow, root)
		}, list)
		t.Logf("acts due, every one read%s: %d listed", credit.name, listed)
		if credit.reset {
			mustExec(t, s.db, `UPDATE tracker_cache_boards SET estimate_reset_at = estimate_reset_at - 1000000000000 WHERE root_id = ?`, root)
		}
	}
	out["cleanup pass, nothing pending"] = measureScale(t, rec, nil, func(int) error { return s.FinishMirrorCleanup(context.Background(), nil) })
	// A complete listing that leaves out a quarter of the board, whose rows
	// a delta page reads back before each run (untimed), so a listing has
	// named them: last, as its issues stay unkept.
	quarter := size.issues / 4
	unlisted := make([]MirrorRow, quarter)
	for i := range unlisted {
		unlisted[i] = benchRow(0, size.issues-quarter+i, 1000)
	}
	// The reconcile leaves the unlisted rows to the read by ID, which
	// removes them: both are measured.
	out["reconcile, a quarter unlisted, read by ID"] = measureScale(t, rec, func(int) {
		for page := range slices.Chunk(unlisted, mirrorMaxPageRows) {
			if _, err := s.WriteMirrorRowPage(root, MirrorRowPage{BoardID: board, Lifetime: lifetime, Rows: page, ReadAt: mirrorReadAt(next())}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}, func(int) error {
		at := next()
		got, err := s.ReconcileMirrorBoard(root, board, lifetime, listing[:size.issues-quarter], true, mirrorReadAt(at))
		if err == nil && len(got.Unsettled) != quarter {
			err = fmt.Errorf("unsettled %d of %d unlisted", len(got.Unsettled), quarter)
		}
		removed := 0
		for ids := range slices.Chunk(got.Unsettled, mirrorMaxPageRows) {
			if err != nil {
				break
			}
			var ch MirrorChanges
			ch, err = s.WriteMirrorRows(root, MirrorRowPage{BoardID: board, Lifetime: lifetime, Missing: ids, ReadAt: mirrorReadAt(next())})
			removed += len(ch.Removed)
		}
		if err == nil && removed != quarter {
			err = fmt.Errorf("the read by ID removed %d of %d unlisted", removed, quarter)
		}
		return err
	})
	// A process's first read of the root, which seeds the root's clock
	// from the read times it stores: the rows it walks, its VM steps and
	// its plan.
	var seedSteps int
	out["first read of the root (seed)"] = measureScale(t, rec, func(int) { s.forgetMirrorReads() }, func(int) error {
		var err error
		seedSteps = countMirrorVMSteps(t, func() { _, err = s.BeginMirrorRead(root, testNow) })
		return err
	})
	walked := map[string]int{}
	for _, tc := range mirrorReadTimeColumns {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM `+tc.table+` WHERE root_id = ?`, root).Scan(&n); err != nil {
			t.Fatal(err)
		}
		walked[tc.table] = n
	}
	t.Logf("first read of the root (seed): %d VM steps, rows of the root walked %v, plan:\n%s", seedSteps, walked, explainMirror(t, s, mirrorReadSeedSQL))
	return out
}

// supportAtScale measures board 0's totals, as a board view and a patch
// read them, under support rules (mirrorSupportSQL), with every row
// estimated, as the bench board is, and then with none (its estimate
// reset moved past every row's read, so every row reads as unknown): a
// rule that lists a type the board's cards do not have, labels that none
// of their cards carries or one that every card carries, or the board. It
// runs last: it leaves the board's rows unknown.
func supportAtScale(t *testing.T, s *Store, root int64, rec *mirrorWriteRecorder) map[string]scaleCost {
	t.Helper()
	board := benchBoardID(0)
	rule := func(r MirrorSupportRule) MirrorRegroup {
		regroup, err := MirrorRegroup{}.WithSupport(r)
		if err != nil {
			t.Fatal(err)
		}
		return regroup
	}
	rules := []struct {
		name    string
		regroup MirrorRegroup
		support bool
	}{
		{"no rule", MirrorRegroup{}, false},
		{"a type no card has", rule(MirrorSupportRule{Types: []string{"10004"}}), false},
		{"2 labels no card has", rule(MirrorSupportRule{Labels: []string{"support", "customer"}}), false},
		{"a label every card has", rule(MirrorSupportRule{Labels: []string{"auth"}}), true},
		{"the board", rule(MirrorSupportRule{Boards: []string{board}}), true},
	}
	sections := []MirrorSection{{Kind: MirrorSectionColumn}, {Kind: MirrorSectionBacklog}, {Kind: MirrorSectionSprint, SprintID: strconv.Itoa(benchBoardSprints - 1)}}
	out := map[string]scaleCost{}
	for _, rows := range []string{"every row estimated", "no row estimated"} {
		if rows == "no row estimated" {
			mustExec(t, s.db, `UPDATE tracker_cache_boards SET estimate_reset_at = (SELECT max(read_at) FROM tracker_cache_board_issues WHERE root_id = ?1 AND board_id = ?2)
				WHERE root_id = ?1 AND board_id = ?2`, root, board)
		}
		for _, r := range rules {
			regroup := r.regroup
			counts, err := s.CountMirrorSections(root, board, regroup)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if rows == "no row estimated" && !r.support {
				want = counts.All.Rows
			}
			if counts.All.Unestimated != want {
				t.Fatalf("%s, %s: %d without an estimate, want %d", rows, r.name, counts.All.Unestimated, want)
			}
			out[fmt.Sprintf("section counts, %s, %s", rows, r.name)] = measureScale(t, rec, nil, func(int) error {
				_, err := s.CountMirrorSections(root, board, regroup)
				return err
			})
			out[fmt.Sprintf("board view, 3 sections, %s, %s", rows, r.name)] = measureScale(t, rec, nil, func(int) error {
				_, _, err := s.ReadMirrorBoardView(root, board, regroup, theseSections(sections))
				return err
			})
		}
	}
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
