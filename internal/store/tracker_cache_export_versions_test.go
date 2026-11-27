package store

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// versionsBoard is a sprint board (10, points) with sprints 40 (closed)
// and 41 (active).
func versionsBoard(t *testing.T) (*Store, int64) {
	t.Helper()
	s, _, root := newMirror(t)
	st := webStructure()
	st.Sprints = []MirrorSprint{
		{ID: "40", OriginBoardID: "10", Name: "WEB Sprint 40", State: MirrorSprintClosed, StartAt: 12_000, EndAt: 14_000, CompletedAt: 14_000},
		{ID: "41", OriginBoardID: "10", Name: "WEB Sprint 41", State: MirrorSprintActive, StartAt: 14_000, EndAt: 30_000},
	}
	mustReplaceStructure(t, s, root, st, true)
	return s, root
}

// versionCard writes issue 10001's card of update time updated, made at
// 1,000, in status with estimate (negative: none) and sprints, from a read
// after every read before it.
func versionCard(t *testing.T, s *Store, root, updated int64, status string, estimate float64, sprints ...string) {
	t.Helper()
	r := sectionRow("10001", "10001", status, estimate, sprints...)
	r.Card.CreatedAt, r.Card.UpdatedAt = 1_000, updated
	p := webPage("10", r)
	p.ReadAt = nextMirrorRead(s, root)
	mustWriteRowPage(t, s, root, p, nil)
}

// versionHistory writes h as the history read for the issue as it stands
// (dueHistories), from a read after every read before it.
func versionHistory(t *testing.T, s *Store, root int64, h MirrorHistory) {
	t.Helper()
	if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{h}), MirrorCatalog{},
		nextMirrorRead(s, root)); err != nil {
		t.Fatal(err)
	}
}

// exportIntervals exports the intervals and the tickets of board 10 from
// window, reading the changes from the file and from the copy, and
// returns each export's status and sprint intervals and each ticket's
// history_behind. Both exports must agree.
func exportIntervals(t *testing.T, s *Store, root, window int64) (status, sprint, behind []string) {
	t.Helper()
	for k, relations := range [][]string{{"status_intervals", "sprint_intervals", "tickets"}, {"status_intervals", "sprint_intervals", "tickets", "changes"}} {
		conn := exportConn(t)
		if _, ok, err := s.ExportMirrorBoard(context.Background(), conn, root, "10", MirrorRegroup{}, relations, window, MirrorLoggedDays{}, CardShapeForTest); err != nil || !ok {
			t.Fatalf("export of %v: %v, %v", relations, ok, err)
		}
		st := exportRows(t, conn, `SELECT ticket || '|' || status || '|' || from_at || '|' || coalesce(to_at, '-') || '|' || coalesce(began, '-') || '|' || open
		  FROM status_intervals ORDER BY ticket, from_at`)
		sp := exportRows(t, conn, `SELECT sprint || '|' || ticket || '|' || coalesce(CAST(points AS INTEGER), '-') || '|' || from_at || '|' || coalesce(to_at, '-')
		  FROM sprint_intervals ORDER BY sprint, ticket, from_at`)
		be := exportRows(t, conn, `SELECT id || '|' || history_behind FROM tickets ORDER BY id`)
		if k == 0 {
			status, sprint, behind = st, sp, be
			continue
		}
		if !reflect.DeepEqual(st, status) || !reflect.DeepEqual(sp, sprint) || !reflect.DeepEqual(be, behind) {
			t.Fatalf("from the copy's changes:\n%v\n%v\n%v\nfrom the file:\n%v\n%v\n%v", st, sp, be, status, sprint, behind)
		}
	}
	return status, sprint, behind
}

// A history read for the card's version that holds a change made after
// it (the tracker moved on between the card's read and the history's)
// leaves the status the card shows open from its status time; the later
// change is of a version the card has not reached and makes no stretch.
func TestAHistoryAheadOfItsCardEndsNoStretchPastTheCard(t *testing.T) {
	t.Parallel()
	s, root := versionsBoard(t)
	versionCard(t, s, root, 20_000, "10001", 1)
	versionHistory(t, s, root, MirrorHistory{IssueID: "10001", Changes: []MirrorChange{
		{Kind: MirrorChangeStatus, At: 15_000, From: "1", To: "10001"}, {Kind: MirrorChangeStatus, At: 21_000, From: "10001", To: "3"}}})
	status, _, _ := exportIntervals(t, s, root, 500)
	if want := []string{"10001|1|1000|15000|1000|1", "10001|10001|15000|-|15000|0"}; !reflect.DeepEqual(status, want) {
		t.Fatalf("status intervals = %v; want %v", status, want)
	}
}

// A card read after its history (it joined a sprint, or its estimate
// changed, since the history was read) has no sprint stretch, and its
// ticket says its history is behind, until a history read for its version
// covers it; a history read that is not final covers nothing.
func TestACardAheadOfItsHistoryHasNoSprintStretch(t *testing.T) {
	t.Parallel()
	five := 5.0
	s, root := versionsBoard(t)
	versionCard(t, s, root, 2_000, "1", 5)
	versionHistory(t, s, root, MirrorHistory{IssueID: "10001"})
	versionCard(t, s, root, 21_000, "1", 5, "41")
	if _, sprint, behind := exportIntervals(t, s, root, 500); len(sprint) > 0 || !reflect.DeepEqual(behind, []string{"10001|1"}) {
		t.Fatalf("joined 41 after the history: sprint intervals %v, behind %v; want none, 1", sprint, behind)
	}
	versionHistory(t, s, root, MirrorHistory{IssueID: "10001", Wait: 60_000, Changes: []MirrorChange{
		{Kind: MirrorChangeSprint, At: 21_000, FromSprints: []string{}, ToSprints: []string{"41"}}}})
	if _, sprint, behind := exportIntervals(t, s, root, 500); len(sprint) > 0 || !reflect.DeepEqual(behind, []string{"10001|1"}) {
		t.Fatalf("a history not final: sprint intervals %v, behind %v; want none, 1", sprint, behind)
	}
	history := MirrorHistory{IssueID: "10001", Changes: []MirrorChange{
		{Kind: MirrorChangeSprint, At: 21_000, FromSprints: []string{}, ToSprints: []string{"41"}},
		{Kind: MirrorChangeEstimate, At: 1_500, BoardID: "10", ToNumber: &five}}}
	versionHistory(t, s, root, history)
	if _, sprint, behind := exportIntervals(t, s, root, 500); !reflect.DeepEqual(sprint, []string{"41|10001|5|21000|-"}) || !reflect.DeepEqual(behind, []string{"10001|0"}) {
		t.Fatalf("covered: sprint intervals %v, behind %v", sprint, behind)
	}
	// Re-estimated to 8 after that history: behind again.
	versionCard(t, s, root, 22_000, "1", 8, "41")
	if _, sprint, behind := exportIntervals(t, s, root, 500); len(sprint) > 0 || !reflect.DeepEqual(behind, []string{"10001|1"}) {
		t.Fatalf("re-estimated after the history: sprint intervals %v, behind %v; want none, 1", sprint, behind)
	}
}

// A board whose estimate field changed reads the estimate history of the
// new field: until a row read after the change and a history read after
// that, the ticket is behind and has no sprint stretch.
func TestAnEstimateChangeOfTheBoardWaitsForItsHistory(t *testing.T) {
	t.Parallel()
	five, hour := 5.0, 3600.0
	s, root := versionsBoard(t)
	versionCard(t, s, root, 2_000, "1", 5, "41")
	versionHistory(t, s, root, MirrorHistory{IssueID: "10001", Changes: []MirrorChange{{Kind: MirrorChangeEstimate, At: 1_500, BoardID: "10", ToNumber: &five}}})
	if _, sprint, _ := exportIntervals(t, s, root, 500); !reflect.DeepEqual(sprint, []string{"41|10001|-|1000|1500", "41|10001|5|1500|-"}) {
		t.Fatalf("in points: %v", sprint)
	}
	st := webStructure()
	st.Sprints = []MirrorSprint{
		{ID: "40", OriginBoardID: "10", Name: "WEB Sprint 40", State: MirrorSprintClosed, StartAt: 12_000, EndAt: 14_000, CompletedAt: 14_000},
		{ID: "41", OriginBoardID: "10", Name: "WEB Sprint 41", State: MirrorSprintActive, StartAt: 14_000, EndAt: 30_000},
	}
	st.Board.EstimateUnit, st.Board.EstimateField = MirrorEstimateTime, "timeoriginalestimate"
	st.ReadAt = nextMirrorRead(s, root)
	mustReplaceStructure(t, s, root, st, true)
	if _, sprint, behind := exportIntervals(t, s, root, 500); len(sprint) > 0 || !reflect.DeepEqual(behind, []string{"10001|1"}) {
		t.Fatalf("after the board's estimate changed: %v, behind %v; want none, 1", sprint, behind)
	}
	versionCard(t, s, root, 2_000, "1", 3600, "41")
	if _, sprint, behind := exportIntervals(t, s, root, 500); len(sprint) > 0 || !reflect.DeepEqual(behind, []string{"10001|1"}) {
		t.Fatalf("its row read again, its history not: %v, behind %v; want none, 1", sprint, behind)
	}
	versionHistory(t, s, root, MirrorHistory{IssueID: "10001", Changes: []MirrorChange{{Kind: MirrorChangeEstimate, At: 1_500, BoardID: "10", ToNumber: &hour}}})
	if _, sprint, behind := exportIntervals(t, s, root, 500); !reflect.DeepEqual(sprint, []string{"41|10001|-|1000|1500", "41|10001|3600|1500|-"}) || !reflect.DeepEqual(behind, []string{"10001|0"}) {
		t.Fatalf("its history read again: %v, behind %v", sprint, behind)
	}
}

// A Linear history, complete from minutes after creation, with no status
// change, shows the status over the stretch it covers when it is final
// for the card: an interval from where it begins, whose start is not when
// the status began. One that is not final shows nothing.
func TestAStatusKnownOverAStretchHasItsInterval(t *testing.T) {
	t.Parallel()
	s, root := versionsBoard(t)
	versionCard(t, s, root, 5_000, "1", 1, "41")
	versionHistory(t, s, root, MirrorHistory{IssueID: "10001", CompleteFrom: 1_180, Wait: 60_000})
	if status, _, _ := exportIntervals(t, s, root, 500); len(status) > 0 {
		t.Fatalf("a history not final: %v; want none", status)
	}
	versionHistory(t, s, root, MirrorHistory{IssueID: "10001", CompleteFrom: 1_180})
	status, sprint, _ := exportIntervals(t, s, root, 500)
	if want := []string{"10001|1|1180|-|-|1"}; !reflect.DeepEqual(status, want) || !reflect.DeepEqual(sprint, []string{"41|10001|1|1180|-"}) {
		t.Fatalf("status intervals = %v, sprint %v; want %v", status, sprint, want)
	}
	// A move out of it later: the stay before it began before the history,
	// at a time not known; the one after began at the move.
	versionCard(t, s, root, 6_000, "3", 1, "41")
	versionHistory(t, s, root, MirrorHistory{IssueID: "10001", CompleteFrom: 1_180,
		Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: 3_000, From: "1", To: "3"}}})
	if status, _, _ = exportIntervals(t, s, root, 500); !reflect.DeepEqual(status, []string{"10001|1|1180|3000|-|1", "10001|3|3000|-|3000|1"}) {
		t.Fatalf("after a move, status intervals = %v", status)
	}
}

// truthEvent is one change of the issue in versionTruth.
type truthEvent struct {
	at       int64
	kind     string
	status   string
	sprints  []string
	estimate float64
}

// versionTruth is what issue 10001 holds over time: made at 1,000 in To
// Do, in no sprint, at 1 point, then its events, one per instant.
type versionTruth struct {
	events []truthEvent
}

type truthState struct {
	status   string
	sprints  []string
	estimate float64
}

func (v versionTruth) at(t int64) truthState {
	s := truthState{status: "1", sprints: []string{}, estimate: 1}
	for _, e := range v.events {
		if e.at > t {
			break
		}
		switch e.kind {
		case MirrorChangeStatus:
			s.status = e.status
		case MirrorChangeSprint:
			s.sprints = e.sprints
		case MirrorChangeEstimate:
			s.estimate = e.estimate
		}
	}
	return s
}

// changes is the history a read at read holds: every change at or before
// it, with its from-values.
func (v versionTruth) changes(read int64) []MirrorChange {
	var out []MirrorChange
	for _, e := range v.events {
		if e.at > read {
			break
		}
		before := v.at(e.at - 1)
		switch e.kind {
		case MirrorChangeStatus:
			out = append(out, MirrorChange{Kind: e.kind, At: e.at, From: before.status, To: e.status})
		case MirrorChangeSprint:
			out = append(out, MirrorChange{Kind: e.kind, At: e.at, FromSprints: before.sprints, ToSprints: e.sprints})
		case MirrorChangeEstimate:
			from, to := before.estimate, e.estimate
			out = append(out, MirrorChange{Kind: e.kind, At: e.at, BoardID: "10", FromNumber: &from, ToNumber: &to})
		}
	}
	return out
}

func newVersionTruth(rng *rand.Rand) versionTruth {
	var v versionTruth
	at := int64(2_000)
	statuses := []string{"1", "3", "10001"}
	sets := [][]string{{}, {"40"}, {"41"}, {"40", "41"}}
	for range 4 + rng.Intn(6) {
		at += int64(500 + rng.Intn(3_000))
		e := truthEvent{at: at, kind: []string{MirrorChangeStatus, MirrorChangeSprint, MirrorChangeEstimate}[rng.Intn(3)]}
		now := v.at(at)
		switch e.kind {
		case MirrorChangeStatus:
			for e.status = now.status; e.status == now.status; {
				e.status = statuses[rng.Intn(len(statuses))]
			}
		case MirrorChangeSprint:
			for e.sprints = now.sprints; slices.Equal(e.sprints, now.sprints); {
				e.sprints = sets[rng.Intn(len(sets))]
			}
		case MirrorChangeEstimate:
			for e.estimate = now.estimate; e.estimate == now.estimate; {
				e.estimate = float64(1 + rng.Intn(8))
			}
		}
		v.events = append(v.events, e)
	}
	return v
}

// After any order of card writes and history writes, the history read for
// an earlier or the card's version, final or not, holding changes made
// after the card's read or not, the export succeeds from the file and from
// the copy's changes, and every interval is what the issue held: no
// stretch of a status or a sprint overlaps another, no closed status
// stretch reaches past the open one, each closed status stretch and each
// sprint stretch up to the card's version holds what the issue held, the
// open status stretch holds the card's status, and a ticket with a sprint
// stretch is not behind.
func TestTheIntervalsHoldWhatTheIssueHeldInAnyOrderOfReads(t *testing.T) {
	t.Parallel()
	// seen counts the exports that held each case, so the sequences are
	// known to reach them: a sprint stretch, a closed status stretch, an
	// open status stretch, a ticket behind its card, and a history holding a
	// change after its card.
	var seen struct {
		sync.Mutex
		sprint, closed, open, behind, ahead int
	}
	t.Run("seeds", func(t *testing.T) {
		for i := range 80 {
			seed := int64(9_100 + i)
			t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
				t.Parallel()
				rng := rand.New(rand.NewSource(seed))
				truth := newVersionTruth(rng)
				s, root := versionsBoard(t)
				var log []string
				fail := func(format string, args ...any) {
					t.Helper()
					t.Fatalf("seed %d; truth %+v; steps:\n%s\n%s", seed, truth.events, strings.Join(log, "\n"), fmt.Sprintf(format, args...))
				}
				card := int64(1_000)
				writeCard := func(u int64) {
					c := truth.at(u)
					versionCard(t, s, root, u, c.status, c.estimate, c.sprints...)
					card = u
					log = append(log, fmt.Sprintf("card at %d: %+v", u, c))
				}
				writeCard(1_000 + int64(rng.Intn(1_000)))
				var listed []MirrorHistory
				for range 8 + rng.Intn(6) {
					switch rng.Intn(3) {
					case 0:
						if u := card + int64(rng.Intn(4_000)); u > card {
							writeCard(u)
						}
					case 1:
						listed = append(listed, dueHistories(s, root, []MirrorHistory{{IssueID: "10001"}})...)
						log = append(log, fmt.Sprintf("listed for %d", listed[len(listed)-1].For))
					default:
						var h MirrorHistory
						if len(listed) > 0 && rng.Intn(2) == 0 {
							h, listed = listed[0], listed[1:]
						} else {
							h = dueHistories(s, root, []MirrorHistory{{IssueID: "10001"}})[0]
						}
						read := h.For + int64(rng.Intn(6_000))
						h.Changes = truth.changes(read)
						if len(h.Changes) > 0 && h.Changes[len(h.Changes)-1].At > card {
							seen.Lock()
							seen.ahead++
							seen.Unlock()
						}
						if rng.Intn(4) == 0 {
							h.Wait = 60_000
						}
						res, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), []MirrorHistory{h}, MirrorCatalog{}, nextMirrorRead(s, root))
						if err != nil {
							fail("history for %d read at %d: %v", h.For, read, err)
						}
						log = append(log, fmt.Sprintf("history for %d read at %d, wait %d: %+v", h.For, read, h.Wait, res[0]))
					}
					status, sprint, behind := exportIntervals(t, s, root, 500)
					checkVersionIntervals(fail, truth, card, status, sprint, behind)
					seen.Lock()
					seen.sprint += min(1, len(sprint))
					for _, r := range status {
						if strings.Contains(r, "|-|") {
							seen.open++
						} else {
							seen.closed++
						}
					}
					if slices.Contains(behind, "10001|1") {
						seen.behind++
					}
					seen.Unlock()
				}
			})
		}
	})
	if seen.sprint == 0 || seen.closed == 0 || seen.open == 0 || seen.behind == 0 || seen.ahead == 0 {
		t.Fatalf("the sequences reached sprint stretches %d, closed %d and open %d status stretches, a ticket behind %d and a history ahead %d times; want each",
			seen.sprint, seen.closed, seen.open, seen.behind, seen.ahead)
	}
	t.Logf("the sequences reached sprint stretches %d, closed %d and open %d status stretches, a ticket behind %d and a history ahead %d times",
		seen.sprint, seen.closed, seen.open, seen.behind, seen.ahead)
}

// checkVersionIntervals checks one export against truth for a card of
// update time card.
func checkVersionIntervals(fail func(string, ...any), truth versionTruth, card int64, status, sprint, behind []string) {
	type stretch struct {
		key, value string
		from, to   int64
		open       bool
	}
	parse := func(row string, value func([]string) string, key func([]string) string, fromAt int) stretch {
		f := strings.Split(row, "|")
		s := stretch{key: key(f), value: value(f)}
		s.from, _ = strconv.ParseInt(f[fromAt], 10, 64)
		if f[fromAt+1] == "-" {
			s.open = true
		} else {
			s.to, _ = strconv.ParseInt(f[fromAt+1], 10, 64)
		}
		return s
	}
	check := func(what string, rows []string, value, key func([]string) string, fromAt int, held func(int64) string, openHeld func(int64) string) {
		var prev *stretch
		for _, row := range rows {
			s := parse(row, value, key, fromAt)
			if s.from > card {
				fail("%s %v starts after the card's version %d", what, row, card)
			}
			if prev != nil && prev.key == s.key && (prev.open || prev.to > s.from) {
				fail("%s %v overlaps the one before it in %v", what, row, rows)
			}
			end := card
			if !s.open {
				end = s.to - 1
				if s.to > card {
					fail("%s %v ends after the card's version %d", what, row, card)
				}
			}
			for _, at := range []int64{s.from, end, (s.from + end) / 2} {
				if at < s.from || at > end {
					continue
				}
				want := held(at)
				if s.open && openHeld != nil {
					want = openHeld(at)
				}
				if got := s.value + "@" + s.key; got != want+"@"+s.key {
					fail("%s %v holds %q at %d; the issue held %q", what, row, s.value, at, want)
				}
			}
			prev = &s
		}
	}
	statusOf := func(f []string) string { return f[1] }
	check("status interval", status, statusOf, func([]string) string { return "" }, 2,
		func(at int64) string { return truth.at(at).status }, func(int64) string { return truth.at(card).status })
	sprintValue := func(f []string) string { return f[2] }
	sprintKey := func(f []string) string { return f[0] }
	for _, sp := range []string{"40", "41"} {
		var rows []string
		for _, r := range sprint {
			if strings.HasPrefix(r, sp+"|") {
				rows = append(rows, r)
			}
		}
		check("sprint "+sp+" interval", rows, sprintValue, sprintKey, 3, func(at int64) string {
			h := truth.at(at)
			if !slices.Contains(h.sprints, sp) {
				return "not in " + sp
			}
			return strconv.Itoa(int(h.estimate))
		}, nil)
	}
	// A ticket with a sprint interval has its history current: some
	// interval holds each instant up to the card's version, the issue's
	// changes and the version itself, at which it was in the sprint.
	if len(sprint) > 0 {
		instants := []int64{card}
		for _, e := range truth.events {
			if e.at <= card {
				instants = append(instants, e.at)
			}
		}
		for _, sp := range []string{"40", "41"} {
			for _, at := range instants {
				if !slices.Contains(truth.at(at).sprints, sp) {
					continue
				}
				if !slices.ContainsFunc(sprint, func(r string) bool {
					s := parse(r, sprintValue, sprintKey, 3)
					return strings.HasPrefix(r, sp+"|") && s.from <= at && (s.open || at < s.to)
				}) {
					fail("the issue was in sprint %s at %d and no interval of %v holds it", sp, at, sprint)
				}
			}
		}
	}
	if len(sprint) > 0 && !reflect.DeepEqual(behind, []string{"10001|0"}) {
		fail("sprint intervals %v of a ticket behind its card (%v)", sprint, behind)
	}
}

// A member of a closed sprint carried on to a later one whose history is
// not read has a status at the closed sprint's completion that is not
// known (the pace's rule): its done is NULL, not 0; once its history is
// read, it is known.
func TestAClosedSprintsMemberWhoseStatusThenIsNotKnownIsNotDoneOrNot(t *testing.T) {
	t.Parallel()
	s, root := versionsBoard(t)
	versionCard(t, s, root, 20_000, "10001", 1, "40", "41")
	finished := func() []string {
		conn := exportConn(t)
		if _, ok, err := s.ExportMirrorBoard(context.Background(), conn, root, "10", MirrorRegroup{}, []string{"sprint_finished"}, 500, MirrorLoggedDays{}, CardShapeForTest); err != nil || !ok {
			t.Fatalf("export: %v, %v", ok, err)
		}
		return exportRows(t, conn, `SELECT sprint || '|' || ticket || '|' || coalesce(done, '-') FROM sprint_finished ORDER BY sprint`)
	}
	if got, want := finished(), []string{"40|10001|-", "41|10001|1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("history not read: %v; want %v", got, want)
	}
	versionHistory(t, s, root, MirrorHistory{IssueID: "10001", Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: 15_000, From: "1", To: "10001"}}})
	if got, want := finished(), []string{"40|10001|0", "41|10001|1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("history read: %v; want %v", got, want)
	}
}
