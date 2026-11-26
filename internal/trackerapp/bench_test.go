package trackerapp

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/tracker"
)

// Benchmarks of the hot paths at realistic sizes, against the real store.

// realisticRow is issue n as a board row: a title of 60 characters, a
// type, a priority, two labels, a sprint, an epic, an assignee, one of
// three statuses and an estimate.
func realisticRow(n int, updated time.Time) tracker.Row {
	five := 5.0
	return tracker.Row{
		Card: tracker.Card{
			ID: tracker.IssueID(fmt.Sprint(30000 + n)), Key: fmt.Sprintf("WEB-%d", n+1), Project: "10000",
			Type:   tracker.IssueType{ID: "10002", Name: "Task"},
			Title:  fmt.Sprintf("%-60s", fmt.Sprintf("Make the checkout page load faster, part %d", n)),
			Status: tracker.StatusID([]string{"s1", "s2", "s3"}[n%3]), Assignee: "acc-user1",
			Priority: tracker.Choice{ID: "3", Name: "Medium"},
			Labels:   []tracker.Choice{{ID: "frontend", Name: "frontend"}, {ID: "performance", Name: "performance"}},
			Parent:   "29999", Sprints: []tracker.SprintID{"1"},
			Created: time.UnixMilli(1_790_000_000_000).UTC(), Updated: updated,
		},
		Order:    tracker.OrderKey(fmt.Sprintf("0|i%05d:", n)),
		Estimate: &five,
	}
}

func realisticRows(from, n int, updated time.Time) []tracker.Row {
	out := make([]tracker.Row, n)
	for i := range out {
		out[i] = realisticRow(from+i, updated)
	}
	return out
}

// benchService is a Service over a cloned store with one Jira connection
// whose provider is scripted.
func benchService(b testing.TB) (*Service, *scripted, Scope) {
	b.Helper()
	p := &scripted{t: b}
	st := storetest.Clone(b)
	svc, err := New(testCallbacks(Deps{
		Store: st, Changed: func() {}, CredentialsDir: filepath.Join(b.TempDir(), keyfile.DirName), ConfigDir: b.TempDir(),
		Origin: APIOrigin, Version: "1", Now: time.Now, Life: b.Context(),
	}, newEventLog()))
	if err != nil {
		b.Fatal(err)
	}
	svc.makeProvider = func(store.TrackerConnection, *clients, *clients) (tracker.Provider, error) { return p, nil }
	conn, err := svc.Add(NewConnection{Kind: store.TrackerKindJira, Site: jiraSiteIn, Account: "user1@example.com"})
	if err != nil {
		b.Fatal(err)
	}
	listBoards(b, st, conn.ID, testBoard)
	p.structure = func(bd tracker.BoardID, _ time.Time) (tracker.Structure, error) { return webStructure(bd), nil }
	b.Cleanup(svc.Stop)
	return svc, p, Scope{Connection: conn.ID, Board: testBoard}
}

func benchTick(b *testing.B, svc *Service, scope Scope, due BoardDue) Outcome {
	b.Helper()
	out, err := svc.syncTick(b.Context(), scope, due, Background)
	if err != nil || out.Failure != nil || out.Ended {
		b.Fatalf("tick: %+v, %v", out, err)
	}
	return out
}

func BenchmarkMirrorRows100(b *testing.B) {
	rows := realisticRows(0, 100, time.UnixMilli(1_799_000_000_000).UTC())
	catalog := webStructure(testBoard).Catalog
	b.ReportAllocs()
	for b.Loop() {
		if _, err := mirrorRows(testBoard, rows, nil, catalog, store.MirrorRead{}); err != nil {
			b.Fatal(err)
		}
	}
}

// A delta tick storing a page of 100 rows that all changed, end to end:
// the hold, the provider call, the mapping, the page's transaction and
// the sync state.
func BenchmarkDeltaTick100Changed(b *testing.B) {
	svc, p, scope := benchService(b)
	benchTick(b, svc, scope, BoardDue{Structure: true})
	at := time.UnixMilli(1_799_000_000_000).UTC()
	p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
		at = at.Add(time.Second)
		return tracker.RowPage{Rows: realisticRows(0, 100, at), Next: cursorAfter("x"), Catalog: webStructure(testBoard).Catalog}, nil
	}
	b.ReportAllocs()
	for b.Loop() {
		if out := benchTick(b, svc, scope, BoardDue{Delta: true}); len(out.Changes().Cards) != 100 {
			b.Fatalf("%d cards changed", len(out.Changes().Cards))
		}
	}
}

// The same with a page identical to what the mirror holds: nothing
// changes.
func BenchmarkDeltaTick100Unchanged(b *testing.B) {
	svc, p, scope := benchService(b)
	benchTick(b, svc, scope, BoardDue{Structure: true})
	rows := realisticRows(0, 100, time.UnixMilli(1_799_000_000_000).UTC())
	p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
		return tracker.RowPage{Rows: rows, Next: cursorAfter("x"), Catalog: webStructure(testBoard).Catalog}, nil
	}
	benchTick(b, svc, scope, BoardDue{Delta: true})
	b.ReportAllocs()
	for b.Loop() {
		if out := benchTick(b, svc, scope, BoardDue{Delta: true}); !out.Changes().Empty() {
			b.Fatalf("an unchanged page changed %+v", out.Changes())
		}
	}
}

// seedBoard stores n realistic rows on scope's board.
func seedBoard(b *testing.B, svc *Service, p *scripted, scope Scope, n int) {
	b.Helper()
	benchTick(b, svc, scope, BoardDue{Structure: true})
	at := time.UnixMilli(1_799_000_000_000).UTC()
	var pages [][]tracker.Row
	for from := 0; from < n; from += readBatch {
		pages = append(pages, realisticRows(from, min(readBatch, n-from), at))
	}
	p.changedRows = pagedDelta(b, pages...)
	benchTick(b, svc, scope, BoardDue{Delta: true})
}

// The view of a board of 500 cards with the first page of its three
// columns: 300 rows read, decoded and named.
func BenchmarkBoardView500(b *testing.B) {
	svc, p, scope := benchService(b)
	seedBoard(b, svc, p, scope, 500)
	sections := []Section{{Kind: SectionColumn, Column: 0}, {Kind: SectionColumn, Column: 1}, {Kind: SectionColumn, Column: 2}}
	b.ReportAllocs()
	for b.Loop() {
		v, err := svc.Board(scope, StatusGroups{}, sections)
		if err != nil || v.All.Rows != 500 || len(v.Sections[0].Rows) != 100 {
			b.Fatalf("view: %d rows, %v", v.All.Rows, err)
		}
	}
}

// One section page of 100 rows of a 500-card board.
func BenchmarkSectionPage100(b *testing.B) {
	svc, p, scope := benchService(b)
	seedBoard(b, svc, p, scope, 500)
	b.ReportAllocs()
	for b.Loop() {
		page, err := svc.SectionRows(scope, StatusGroups{}, Section{Kind: SectionColumn, Column: 1}, nil)
		if err != nil || len(page.Rows) != 100 {
			b.Fatalf("page: %d rows, %v", len(page.Rows), err)
		}
	}
}

// The Status groups page's read of a project of 20 read boards, each of
// 12 columns of 5 statuses (half of each board's shared with the next),
// over a catalog of 1,000 statuses, with 30 statuses regrouped, 10 of
// them on no board.
func BenchmarkProjectStatuses20Boards(b *testing.B) {
	svc, p, scope := benchService(b)
	statuses := make([]tracker.Status, 1000)
	for i := range statuses {
		statuses[i] = tracker.Status{ID: tracker.StatusID(fmt.Sprintf("s%d", i)), Name: fmt.Sprintf("Status %d", i), Group: tracker.GroupInProgress}
	}
	boards := make([]tracker.BoardID, 20)
	for i := range boards {
		boards[i] = tracker.BoardID(fmt.Sprint(i + 1))
	}
	p.structure = func(bd tracker.BoardID, _ time.Time) (tracker.Structure, error) {
		st := webStructure(bd)
		n, err := strconv.Atoi(string(bd))
		if err != nil {
			return tracker.Structure{}, err
		}
		st.Columns = make([]tracker.Column, 12)
		for c := range st.Columns {
			col := tracker.Column{Name: fmt.Sprintf("Column %d", c)}
			for k := range 5 {
				col.Statuses = append(col.Statuses, statuses[(n*30+c*5+k)%len(statuses)].ID)
			}
			st.Columns[c] = col
		}
		st.Statuses = statuses
		return st, nil
	}
	listBoards(b, svc.store.(*store.Store), scope.Connection, boards...)
	for _, bd := range boards {
		benchTick(b, svc, Scope{Connection: scope.Connection, Board: bd}, BoardDue{Structure: true})
	}
	project := "boards-" + scope.Connection
	row, err := svc.get(scope.Connection)
	if err != nil {
		b.Fatal(err)
	}
	groups := map[string]string{}
	for i := range 20 {
		groups[fmt.Sprintf("s%d", i*20)] = "done"
	}
	for i := range 10 {
		groups[fmt.Sprintf("s%d", 900+i)] = "left_out"
	}
	view, err := svc.ProjectSetup(project)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := svc.SetProjectSetup(project, view.Revision, Setup{StatusGroups: []SiteStatusGroups{{Tracker: store.TrackerKindJira, Site: row.Site, Statuses: groups}}}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		sites, err := svc.ProjectStatuses(project)
		if err != nil || len(sites) != 1 || len(sites[0].Statuses) == 0 || len(sites[0].Unread) != 0 {
			b.Fatalf("statuses: %+v, %v", sites, err)
		}
	}
}

func BenchmarkCardBlobDecode(b *testing.B) {
	m, err := mirrorCard(realisticRow(1, time.UnixMilli(1_799_000_000_000).UTC()).Card)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := cardOf(m); !ok {
			b.Fatal("a miss")
		}
	}
}

// realisticDetail is a ticket with a 2 KB description, 10 fields, a
// page of 20 comments, 3 links, 2 attachments, 50 events and 4 moves.
func realisticDetail() tracker.Detail {
	at := time.UnixMilli(1_799_000_000_000).UTC()
	d := tracker.Detail{
		Card:        realisticRow(1, at).Card,
		Description: strings.Repeat("The checkout page waits on three requests before it paints. ", 34),
	}
	for i := range 10 {
		d.Fields = append(d.Fields, tracker.Field{ID: tracker.FieldID(fmt.Sprintf("customfield_1%04d", i)), Name: fmt.Sprintf("Field %d", i),
			Kind: tracker.KindText, Editable: true, Value: tracker.TextValue(fmt.Sprintf("value %d", i))})
	}
	for i := range 20 {
		d.Comments.Comments = append(d.Comments.Comments, tracker.Comment{ID: tracker.CommentID(fmt.Sprint(i)), Author: "acc-user1",
			Body: strings.Repeat("A comment of a few sentences about the change. ", 4), Created: at})
	}
	for i := range 3 {
		d.Links = append(d.Links, tracker.Link{ID: tracker.LinkID(fmt.Sprint(i)), Type: tracker.LinkType{ID: "10000", Name: "Blocks", Outward: "blocks", Inward: "is blocked by"},
			Outward: true, Issue: tracker.IssueRef{ID: "30002", Key: "WEB-3", Title: "Another task", Status: "s1"}})
	}
	for i := range 2 {
		d.Attachments = append(d.Attachments, tracker.Attachment{ID: tracker.AttachmentID(fmt.Sprint(i)), Name: "screenshot.png", MimeType: "image/png", Size: 120_000, Created: at, Author: "acc-user1"})
	}
	for i := range 50 {
		d.History = append(d.History, tracker.Event{At: at.Add(time.Duration(i) * time.Minute), Author: "acc-user1", Field: "status", From: "To Do", To: "In Progress"})
	}
	for i := range 4 {
		d.Moves = append(d.Moves, tracker.Move{ID: tracker.MoveID(fmt.Sprint(i)), Name: "Start", To: "s2"})
	}
	return d
}

func BenchmarkBodyBlobDecode(b *testing.B) {
	d := realisticDetail()
	data, err := mirrorBody(d)
	if err != nil {
		b.Fatal(err)
	}
	card, err := mirrorCard(d.Card)
	if err != nil {
		b.Fatal(err)
	}
	m := store.MirrorBody{IssueID: string(d.Card.ID), Card: card, Data: data}
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := bodyOf(m); !ok {
			b.Fatal("a miss")
		}
	}
}

// benchmarks are the package's benchmarks, which the test suite runs once
// each (TestBenchmarksRun).
var benchmarks = map[string]func(*testing.B){
	"BenchmarkMirrorRows100":              BenchmarkMirrorRows100,
	"BenchmarkDeltaTick100Changed":        BenchmarkDeltaTick100Changed,
	"BenchmarkDeltaTick100Unchanged":      BenchmarkDeltaTick100Unchanged,
	"BenchmarkBoardView500":               BenchmarkBoardView500,
	"BenchmarkSectionPage100":             BenchmarkSectionPage100,
	"BenchmarkProjectStatuses20Boards":    BenchmarkProjectStatuses20Boards,
	"BenchmarkCardBlobDecode":             BenchmarkCardBlobDecode,
	"BenchmarkBodyBlobDecode":             BenchmarkBodyBlobDecode,
	"BenchmarkLinkedCardsAllChanged10000": BenchmarkLinkedCardsAllChanged10000,
	"BenchmarkFoldByMerge10000":           BenchmarkFoldByMerge10000,
	"BenchmarkFoldByLog10000":             BenchmarkFoldByLog10000,
}

// Every benchmark of the package runs one iteration without failing, so a
// benchmark broken by a change fails the suite instead of the next
// measurement. The list is checked against the package's source.
func TestBenchmarksRun(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := gotoken.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Benchmark") && benchmarks[fn.Name.Name] == nil {
				t.Errorf("%s (%s) is not in benchmarks", fn.Name.Name, path)
			}
		}
	}
	// One iteration a benchmark; the flag is the testing package's own and
	// only benchmarks read it.
	before := flag.Lookup("test.benchtime").Value.String()
	if err := flag.Set("test.benchtime", "1x"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := flag.Set("test.benchtime", before); err != nil {
			t.Error(err)
		}
	})
	for _, name := range slices.Sorted(maps.Keys(benchmarks)) {
		// testing.Benchmark discards a failed benchmark's output and returns
		// no iterations.
		if r := testing.Benchmark(benchmarks[name]); r.N != 1 {
			t.Errorf("%s ran %d iterations; run it with -bench to see why", name, r.N)
		}
	}
}
