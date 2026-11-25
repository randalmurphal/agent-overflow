package trackerapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"maps"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// The client's patch rule, written once here as the reference the
// frontend ports. A client holds some sections of a board: each the rows
// of the pages it loaded and the cursor of the next page (none when the
// section is loaded whole), and the board's totals. For a BoardEvent's
// IDs and the BoardPatch read for them:
//
//  1. remove each ID from every section held;
//  2. insert each row the patch returns for a section at its (order,
//     issue) place, when that place is not after the section's cursor, or
//     the section is loaded whole;
//  3. take the patch's totals when it has them, absent sprints and columns
//     as empty; a patch read for the totals held has none;
//  4. carry the patch's lifetime and revision into every cursor.

// heldSection is a section as a client holds it.
type heldSection struct {
	section Section
	rows    []RowView
	next    *SectionCursor
}

// heldBoard is what a client holds of a board: totals is the totals
// revision its totals are at.
type heldBoard struct {
	sections                   []heldSection
	all, backlog               SectionTotal
	sprints                    map[string]SectionTotal
	columns                    map[int]SectionTotal
	lifetime, revision, totals int64
}

// heldFromView is what a client holds after reading v, the view of
// sections.
func heldFromView(v BoardView, sections []Section) heldBoard {
	h := heldBoard{all: v.All, backlog: v.Backlog, sprints: map[string]SectionTotal{}, columns: map[int]SectionTotal{},
		lifetime: v.Lifetime, revision: v.Revision, totals: v.TotalsRevision}
	for _, sp := range v.Sprints {
		if sp.Total.Rows > 0 {
			h.sprints[sp.ID] = sp.Total
		}
	}
	for i, col := range v.Columns {
		if col.Total.Rows > 0 {
			h.columns[i] = col.Total
		}
	}
	for i, page := range v.Sections {
		h.sections = append(h.sections, heldSection{section: sections[i], rows: slices.Clone(page.Rows), next: page.Next})
	}
	return h
}

// held names the totals h holds, for a patch read.
func (h heldBoard) held() TotalsAt { return TotalsAt{Lifetime: h.lifetime, Revision: h.totals} }

func (h heldBoard) sectionList() []Section {
	out := make([]Section, len(h.sections))
	for i, s := range h.sections {
		out[i] = s.section
	}
	return out
}

// rowAfter reports whether r's place is after cursor c.
func rowAfter(r RowView, c *SectionCursor) bool {
	if r.Order != c.Order {
		return r.Order > c.Order
	}
	return r.Card.ID > c.Issue
}

// rowCompare orders rows by (order, issue).
func rowCompare(a, b RowView) int {
	if c := strings.Compare(a.Order, b.Order); c != 0 {
		return c
	}
	return strings.Compare(a.Card.ID, b.Card.ID)
}

// applyPatch is the client's rule: h after the event naming ids and its
// patch p. It leaves h as it was.
func applyPatch(h heldBoard, ids []tracker.IssueID, p BoardPatch) heldBoard {
	out := heldBoard{all: h.all, backlog: h.backlog, sprints: h.sprints, columns: h.columns, totals: h.totals,
		lifetime: p.Lifetime, revision: p.Revision}
	if t := p.Totals; t != nil {
		out.all, out.backlog, out.totals = t.All, t.Backlog, t.Revision
		out.sprints, out.columns = map[string]SectionTotal{}, map[int]SectionTotal{}
		for _, s := range t.Sprints {
			out.sprints[s.Sprint] = s.Total
		}
		for _, c := range t.Columns {
			out.columns[c.Column] = c.Total
		}
	}
	for i, sec := range h.sections {
		rows := slices.DeleteFunc(slices.Clone(sec.rows), func(r RowView) bool { return slices.Contains(ids, tracker.IssueID(r.Card.ID)) })
		for _, r := range p.Sections[i].Rows {
			if sec.next != nil && rowAfter(r, sec.next) {
				continue
			}
			at, _ := slices.BinarySearchFunc(rows, r, rowCompare)
			rows = slices.Insert(rows, at, r)
		}
		var next *SectionCursor
		if sec.next != nil {
			c := *sec.next
			c.Lifetime, c.Revision = p.Lifetime, p.Revision
			next = &c
		}
		out.sections = append(out.sections, heldSection{section: sec.section, rows: rows, next: next})
	}
	return out
}

func (h heldBoard) sameRows(o heldBoard) bool { return h.describe() == o.describe() }

// describe is h as text, for comparing and reporting.
func (h heldBoard) describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "all %v backlog %v sprints %v columns %v\n", h.all, h.backlog, h.sprints, h.columns)
	for _, s := range h.sections {
		raw, _ := json.Marshal(s.rows)
		fmt.Fprintf(&b, "%+v loaded whole %v: %s\n", s.section, s.next == nil, raw)
	}
	return b.String()
}

// pageOn reads every section of h to its end from its cursor.
func pageOn(t *testing.T, s *Service, scope Scope, h heldBoard) heldBoard {
	t.Helper()
	for i := range h.sections {
		sec := &h.sections[i]
		for sec.next != nil {
			page, err := s.SectionRows(scope, StatusGroups{}, sec.section, sec.next)
			if err != nil {
				t.Fatalf("page on %+v from %+v: %v", sec.section, sec.next, err)
			}
			sec.rows = append(sec.rows, page.Rows...)
			sec.next = page.Next
		}
	}
	return h
}

// prefixOf is full (every section loaded whole) cut to what h loaded.
func prefixOf(full, h heldBoard) heldBoard {
	out := full
	out.sections = slices.Clone(full.sections)
	for i, sec := range h.sections {
		rows := full.sections[i].rows
		if sec.next != nil {
			rows = slices.DeleteFunc(slices.Clone(rows), func(r RowView) bool { return rowAfter(r, sec.next) })
		}
		out.sections[i] = heldSection{section: sec.section, rows: rows, next: sec.next}
	}
	return out
}

// modelTracker writes a board's rows into the mirror as reads through the
// board would, and reports what each write changed as the funnel names it.
type modelTracker struct {
	t        *testing.T
	m        *mirrorFixture
	root     int64
	lifetime int64
	readAt   int64
	rows     map[tracker.IssueID]tracker.Row
	next     int
	rng      *rand.Rand
}

func (mt *modelTracker) newRow() tracker.Row {
	mt.next++
	r := tracker.Row{Card: card(fmt.Sprint(40000+mt.next), 1_799_000_000_000), Order: tracker.OrderKey(mt.key())}
	mt.place(&r)
	return r
}

func (mt *modelTracker) key() string { return fmt.Sprintf("k%06d", mt.rng.IntN(1_000_000)) }

// place gives r a random status, sprint and estimate.
func (mt *modelTracker) place(r *tracker.Row) {
	r.Card.Status = tracker.StatusID([]string{"s1", "s1", "s2", "s3"}[mt.rng.IntN(4)])
	r.Card.Sprints = [][]tracker.SprintID{nil, nil, {"1"}, {"2"}}[mt.rng.IntN(4)]
	mt.estimate(r)
}

// estimate gives r a random estimate or none.
func (mt *modelTracker) estimate(r *tracker.Row) {
	r.Estimate = nil
	if n := mt.rng.IntN(5); n > 0 {
		v := float64(n)
		r.Estimate = &v
	}
}

// write stores rows and removes missing in one read, and returns the IDs
// a board event names for it.
func (mt *modelTracker) write(rows []tracker.Row, missing []tracker.IssueID) []tracker.IssueID {
	mt.t.Helper()
	mt.readAt++
	for i := range rows {
		rows[i].Card.Updated = time.UnixMilli(mt.readAt)
		mt.rows[rows[i].Card.ID] = rows[i]
	}
	for _, id := range missing {
		delete(mt.rows, id)
	}
	page := must[store.MirrorRowPage](mt.t)(mirrorRows(testBoard, rows, missing, webStructure(testBoard).Catalog, beginReadAt(mt.t, mt.m.st, mt.root, mt.readAt)))
	page.Lifetime = mt.lifetime
	ch := changesOf(must[store.MirrorChanges](mt.t)(mt.m.st.WriteMirrorRows(mt.root, page)))
	return unite(unite(slices.Clone(ch.Rows), ch.Cards), ch.Removed)
}

// any is a random row the tracker holds.
func (mt *modelTracker) any() tracker.Row {
	keys := make([]tracker.IssueID, 0, len(mt.rows))
	for id := range mt.rows {
		keys = append(keys, id)
	}
	slices.Sort(keys)
	return mt.rows[keys[mt.rng.IntN(len(keys))]]
}

// change makes one random change, near h's loaded edges for some.
func (mt *modelTracker) change(h heldBoard) []tracker.IssueID {
	switch kind := mt.rng.IntN(8); {
	case kind == 7:
		r := mt.any()
		mt.estimate(&r)
		return mt.write([]tracker.Row{r}, nil)
	case kind == 0 || len(mt.rows) < 50:
		return mt.write([]tracker.Row{mt.newRow()}, nil)
	case kind == 1:
		return mt.write(nil, []tracker.IssueID{mt.any().Card.ID})
	case kind == 2:
		r := mt.any()
		r.Order = tracker.OrderKey(mt.key())
		return mt.write([]tracker.Row{r}, nil)
	case kind == 3:
		// Across a loaded edge: just before or just after a cursor.
		var edges []*SectionCursor
		for _, s := range h.sections {
			if s.next != nil {
				edges = append(edges, s.next)
			}
		}
		if len(edges) == 0 {
			return mt.change(h)
		}
		edge := edges[mt.rng.IntN(len(edges))]
		r := mt.any()
		if mt.rng.IntN(2) == 0 {
			r.Order = tracker.OrderKey(edge.Order + "~")
		} else {
			r.Order = tracker.OrderKey(edge.Order[:len(edge.Order)-1])
		}
		return mt.write([]tracker.Row{r}, nil)
	case kind == 4:
		r := mt.any()
		mt.place(&r)
		return mt.write([]tracker.Row{r}, nil)
	case kind == 5:
		r := mt.any()
		r.Card.Title += "!"
		return mt.write([]tracker.Row{r}, nil)
	default:
		return mt.write([]tracker.Row{mt.newRow(), mt.newRow()}, []tracker.IssueID{mt.any().Card.ID})
	}
}

// A client applying patches holds what one reading the board view again
// holds, on randomized changes: rows entering, leaving, moving within,
// across the loaded edge and between sections, estimates changing,
// card-only changes, and two changes landing before the first patch is
// read; and its cursors page on to the same rows. A patch for a change
// that moved no total reads none.
func TestPatchingMatchesReadingAgain(t *testing.T) {
	t.Parallel()
	for seed := range uint64(3) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			runPatchModel(t, seed, 60, nil)
		})
	}
}

// updatePatchCases, set in the environment, rewrites
// testdata/board_patch_cases.json from the model: make go-test passes the
// environment through, where a flag before its package list would not
// reach the test binary.
const updatePatchCases = "AO_UPDATE_PATCH_CASES"

// patchCasesPath is the patch rule's shared cases: a seeded run of the
// model, which the frontend's port of applyPatch replays
// (frontend/src/lib/stores/trackerBoardModel.test.ts).
const patchCasesPath = "testdata/board_patch_cases.json"

// patchCases is a recorded run: the sections held, the first view, and
// each step with the held board after it, as heldCanon's FNV-1a digest;
// Final is the last held board whole, for reading a mismatch.
type patchCases struct {
	Comment  string      `json:"comment"`
	Sections []Section   `json:"sections"`
	View     BoardView   `json:"view"`
	Steps    []patchStep `json:"steps"`
	Final    string      `json:"final"`
}

// patchStep is an event's IDs with the patch read for them, or a further
// page of the held section at index Page.
type patchStep struct {
	IDs    []tracker.IssueID `json:"ids,omitempty"`
	Patch  *BoardPatch       `json:"patch,omitempty"`
	Page   *int              `json:"page,omitempty"`
	Rows   *SectionPage      `json:"rows,omitempty"`
	Expect string            `json:"expect"`
}

// The recorded run matches the model as it runs now, so the frontend
// replays the rule the store and applyPatch keep. AO_UPDATE_PATCH_CASES=1
// rewrites it.
func TestPatchCasesMatchTheModel(t *testing.T) {
	t.Parallel()
	rec := &patchCases{Comment: "Generated by TestPatchCasesMatchTheModel (internal/trackerapp/patch_model_test.go) with AO_UPDATE_PATCH_CASES=1; replayed by frontend/src/lib/stores/trackerBoardModel.test.ts."}
	runPatchModel(t, 3, 24, rec)
	got, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv(updatePatchCases) != "" {
		if err := os.WriteFile(patchCasesPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(patchCasesPath)
	if err != nil {
		t.Fatalf("read the recorded patch cases (rewrite them with AO_UPDATE_PATCH_CASES=1): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s no longer matches the model; rewrite it with AO_UPDATE_PATCH_CASES=1 make go-test GO_TEST_PKGS=./internal/trackerapp GO_TEST_FLAGS='-run TestPatchCasesMatchTheModel', and check the frontend's replay", patchCasesPath)
	}
}

// heldCanon is h as the text both this test and the frontend's replay
// digest: the totals with a row, then each section's header and rows.
func heldCanon(h heldBoard) string {
	var b strings.Builder
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	tot := func(t SectionTotal) string { return fmt.Sprintf("%d/%s", t.Rows, num(t.Estimate)) }
	fmt.Fprintf(&b, "totals all %s backlog %s sprints", tot(h.all), tot(h.backlog))
	for _, id := range slices.Sorted(maps.Keys(h.sprints)) {
		if t := h.sprints[id]; t.Rows > 0 {
			fmt.Fprintf(&b, " %s=%s", id, tot(t))
		}
	}
	b.WriteString(" columns")
	for _, i := range slices.Sorted(maps.Keys(h.columns)) {
		if t := h.columns[i]; t.Rows > 0 {
			fmt.Fprintf(&b, " %d=%s", i, tot(t))
		}
	}
	b.WriteString("\n")
	for _, s := range h.sections {
		fmt.Fprintf(&b, "%s %s %d ", s.section.Kind, s.section.Sprint, s.section.Column)
		if s.next == nil {
			b.WriteString("whole\n")
		} else {
			fmt.Fprintf(&b, "next %s %s %d %d\n", s.next.Order, s.next.Issue, s.next.Lifetime, s.next.Revision)
		}
		for _, r := range s.rows {
			est := "-"
			if r.Estimate != nil {
				est = num(*r.Estimate)
			}
			fmt.Fprintf(&b, "%s %s %s %s\n", r.Card.ID, r.Order, est, r.Card.Title)
		}
	}
	return b.String()
}

// digest is the 32-bit FNV-1a of s, in hex.
func digest(s string) string {
	h := fnv.New32a()
	h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

// runPatchModel runs rounds of random changes on a board of 270 rows,
// patching a client and checking it against reading again each round;
// rec, when set, records the run.
func runPatchModel(t *testing.T, seed uint64, rounds int, rec *patchCases) {
	m := newMirrorFixture(t)
	must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	mt := &modelTracker{t: t, m: m, root: m.root(), lifetime: m.sync(testBoard).Lifetime, readAt: 1_799_000_000_000,
		rows: map[tracker.IssueID]tracker.Row{}, rng: rand.New(rand.NewPCG(seed, 7))}
	for range 3 {
		var page []tracker.Row
		for range 90 {
			page = append(page, mt.newRow())
		}
		mt.write(page, nil)
	}
	sections := []Section{{Kind: SectionSprint, Sprint: "1"}, {Kind: SectionSprint, Sprint: "2"}, {Kind: SectionBacklog},
		{Kind: SectionColumn}, {Kind: SectionColumn, Column: 1}, {Kind: SectionColumn, Column: 2}}
	scope := m.scope()
	read := func() heldBoard {
		return heldFromView(must[BoardView](t)(m.svc.Board(scope, StatusGroups{}, sections)), sections)
	}
	first := must[BoardView](t)(m.svc.Board(scope, StatusGroups{}, sections))
	client := heldFromView(first, sections)
	if rec != nil {
		rec.Sections, rec.View = sections, first
	}
	// How many patches carried totals, and how many found the
	// client's current.
	withTotals, without := 0, 0
	for round := range rounds {
		var events [][]tracker.IssueID
		for range 1 + mt.rng.IntN(2) {
			events = append(events, mt.change(client))
		}
		for _, ids := range events {
			if len(ids) == 0 {
				continue
			}
			patch := must[BoardPatch](t)(m.svc.BoardPatch(scope, StatusGroups{}, sections, ids, client.held()))
			if patch.Totals != nil {
				withTotals++
			} else {
				without++
			}
			client = applyPatch(client, ids, patch)
			if rec != nil {
				rec.Steps = append(rec.Steps, patchStep{IDs: ids, Patch: &patch, Expect: digest(heldCanon(client))})
			}
		}
		if mt.rng.IntN(5) == 0 {
			// Load one more page of a section, at the carried revision.
			i := mt.rng.IntN(len(client.sections))
			if sec := &client.sections[i]; sec.next != nil {
				page := must[SectionPage](t)(m.svc.SectionRows(scope, StatusGroups{}, sec.section, sec.next))
				sec.rows, sec.next = append(sec.rows, page.Rows...), page.Next
				if rec != nil {
					rec.Steps = append(rec.Steps, patchStep{Page: &i, Rows: &page, Expect: digest(heldCanon(client))})
				}
			}
		}
		full := pageOn(t, m.svc, scope, read())
		if want := prefixOf(full, client); !client.sameRows(want) {
			t.Fatalf("round %d: the patched client holds\n%s\nreading again gives\n%s", round, client.describe(), want.describe())
		}
	}
	if rec != nil {
		rec.Final = heldCanon(client)
	}
	if got, want := pageOn(t, m.svc, scope, client), pageOn(t, m.svc, scope, read()); !got.sameRows(want) {
		t.Fatalf("paged on, the patched client holds\n%s\nreading again gives\n%s", got.describe(), want.describe())
	}
	if withTotals == 0 || without == 0 {
		t.Fatalf("%d patches read the totals and %d found them held; want some of each", withTotals, without)
	}
}
