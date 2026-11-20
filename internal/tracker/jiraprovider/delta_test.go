package jiraprovider

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
)

// boardIndex is a fake of board 3's issue list as the delta queries it.
// It reads the two JQL shapes the delta sends with the measured meaning
// of their time literal, milliseconds since the epoch, and the ID list a
// write's board check sends, and answers the
// matching issues by update time then ID, two to a page, with a page
// token while more match.
type boardIndex struct {
	t       *testing.T
	mu      sync.Mutex
	issues  []indexed
	warn    string
	reverse bool // answer each page in the wrong order
	token   bool // name a next page whatever matched
	jql     []string
}

type indexed struct {
	id      int64
	updated time.Time
}

var (
	startJQL  = regexp.MustCompile(`^updated >= (\d+) ORDER BY updated ASC, id ASC$`)
	resumeJQL = regexp.MustCompile(`^\(updated > (\d+) OR \(updated >= (\d+) AND id > (\d+)\)\) ORDER BY updated ASC, id ASC$`)
	idsJQL    = regexp.MustCompile(`^id in \((\d+(?:, \d+)*)\)$`)
)

func (x *boardIndex) set(id int64, updated time.Time) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.issues = slices.DeleteFunc(x.issues, func(i indexed) bool { return i.id == id })
	x.issues = append(x.issues, indexed{id, updated})
	slices.SortFunc(x.issues, func(a, b indexed) int {
		return cmp.Or(cmp.Compare(a.updated.UnixMilli(), b.updated.UnixMilli()), cmp.Compare(a.id, b.id))
	})
}

// match is the predicate jql states, or false when it is not one the
// delta sends.
func (x *boardIndex) match(jql string) (func(indexed) bool, bool) {
	num := func(s string) int64 {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			x.t.Error(err)
		}
		return n
	}
	if m := startJQL.FindStringSubmatch(jql); m != nil {
		ms := num(m[1])
		return func(is indexed) bool { return is.updated.UnixMilli() >= ms }, true
	}
	if m := idsJQL.FindStringSubmatch(jql); m != nil {
		var listed []int64
		for s := range strings.SplitSeq(m[1], ", ") {
			listed = append(listed, num(s))
		}
		return func(is indexed) bool { return slices.Contains(listed, is.id) }, true
	}
	if m := resumeJQL.FindStringSubmatch(jql); m != nil && m[1] == m[2] {
		ms, id := num(m[1]), num(m[3])
		return func(is indexed) bool {
			at := is.updated.UnixMilli()
			return at > ms || at >= ms && is.id > id
		}, true
	}
	return nil, false
}

func (x *boardIndex) serve(w http.ResponseWriter, r *http.Request, b []byte) {
	x.mu.Lock()
	defer x.mu.Unlock()
	q := r.URL.Query()
	keep, ok := x.match(q.Get("jql"))
	if !ok || q.Get("nextPageToken") != "" {
		x.t.Errorf("delta JQL %q token %q", q.Get("jql"), q.Get("nextPageToken"))
		jiratest.Reply(400, `{}`)(w, r, b)
		return
	}
	x.jql = append(x.jql, q.Get("jql"))
	var match []indexed
	for _, is := range x.issues {
		if keep(is) {
			match = append(match, is)
		}
	}
	end := min(2, len(match))
	var out []map[string]any
	for _, is := range match[:end] {
		id := decimal(is.id)
		out = append(out, issue(id, "WEB-"+id[len(id)-1:], is.updated.Format("2006-01-02T15:04:05.000-0700"), nil))
	}
	if x.reverse {
		slices.Reverse(out)
	}
	v := map[string]any{"issues": out, "isLast": end == len(match) && !x.token}
	if end < len(match) || x.token {
		v["nextPageToken"] = "next"
	}
	if x.warn != "" {
		v["warningMessages"] = []string{x.warn}
	}
	jiratest.Reply(200, mustJSON(v))(w, r, b)
}

func deltaSite(t *testing.T) (*jiratest.Server, *boardIndex) {
	f, _ := newSite(t)
	x := &boardIndex{t: t}
	f.On("GET", board3Issues, x.serve)
	return f, x
}

func ids(page tracker.RowPage) string {
	out := make([]string, len(page.Rows))
	for i, r := range page.Rows {
		out[i] = string(r.Card.ID)
	}
	return strings.Join(out, " ")
}

// walk reads board 3's delta from page on, each page by a Provider made
// afresh as after a restart, until it reports no more, returning the
// pages' rows and the last page.
func walk(t *testing.T, f *jiratest.Server, page tracker.RowPage) ([]string, tracker.RowPage) {
	t.Helper()
	var all []string
	for range 10 {
		var err error
		page, err = newProvider(t, f, nil).ChangedRows(t.Context(), tracker.DeltaQuery{Board: "3", From: page.Next})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, ids(page))
		if !page.More {
			return all, page
		}
	}
	t.Fatal("the walk did not end")
	return nil, page
}

// TestChangedRowsResumesWithinOneMillisecond proves a run of issues
// sharing one update millisecond, longer than a page, is delivered once
// each across pages whose cursors were stored between them; that an
// issue a millisecond later with a lower ID follows the run; that the
// JQL sent is exactly the documented shapes; and that only the end of a
// walk steps back to deliver its last minute again.
func TestChangedRowsResumesWithinOneMillisecond(t *testing.T) {
	f, x := deltaSite(t)
	at := time.Date(2026, 11, 10, 15, 0, 30, 250*int(time.Millisecond), time.UTC)
	x.set(10009, at.Add(-2*time.Hour))
	for id := int64(10002); id <= 10006; id++ {
		x.set(id, at)
	}
	x.set(10001, at.Add(time.Millisecond))
	start := at.Add(-time.Hour)

	first, err := newProvider(t, f, nil).ChangedRows(t.Context(), tracker.DeltaQuery{Board: "3", Start: start})
	if err != nil || ids(first) != "10002 10003" || !first.More {
		t.Fatalf("page 1: %s more %v %v", ids(first), first.More, err)
	}
	rest, last := walk(t, f, first)
	if got := strings.Join(append([]string{ids(first)}, rest...), " | "); got != "10002 10003 | 10004 10005 | 10006 10001" {
		t.Errorf("the walk delivered %s", got)
	}
	ms := strconv.FormatInt(at.UnixMilli(), 10)
	want := []string{
		"updated >= " + strconv.FormatInt(start.UnixMilli(), 10) + " ORDER BY updated ASC, id ASC",
		"(updated > " + ms + " OR (updated >= " + ms + " AND id > 10003)) ORDER BY updated ASC, id ASC",
		"(updated > " + ms + " OR (updated >= " + ms + " AND id > 10005)) ORDER BY updated ASC, id ASC",
	}
	if !slices.Equal(x.jql, want) {
		t.Errorf("sent\n%s\nwant\n%s", strings.Join(x.jql, "\n"), strings.Join(want, "\n"))
	}
	// The walk ended: the next one starts a minute before the newest row.
	var c cursor
	if err := jsonUnmarshal(last.Next, &c); err != nil || c.AfterMS != at.Add(time.Millisecond-time.Minute).UnixMilli() || c.AfterID != 0 {
		t.Errorf("end cursor %+v %v", c, err)
	}
	page, err := newProvider(t, f, nil).ChangedRows(t.Context(), tracker.DeltaQuery{Board: "3", From: last.Next})
	if err != nil || ids(page) != "10002 10003" {
		t.Errorf("the next walk: %s %v", ids(page), err)
	}
}

// TestChangedRowsDeliversUpdatesDuringWalk proves an issue delivered
// early in a walk and updated during it is delivered again after the
// rows before its new time.
func TestChangedRowsDeliversUpdatesDuringWalk(t *testing.T) {
	f, x := deltaSite(t)
	at := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	for id := int64(10001); id <= 10004; id++ {
		x.set(id, at)
	}
	first, err := newProvider(t, f, nil).ChangedRows(t.Context(), tracker.DeltaQuery{Board: "3", Start: at})
	if err != nil || ids(first) != "10001 10002" {
		t.Fatalf("page 1: %s %v", ids(first), err)
	}
	x.set(10001, at.Add(time.Second))
	rest, _ := walk(t, f, first)
	if got := strings.Join(rest, " | "); got != "10003 10004 | 10001" {
		t.Errorf("the rest of the walk delivered %s", got)
	}
}

// TestChangedRowsIdleCursorStays proves a cursor at the start of a walk
// that finds nothing keeps its position across several reads and reports
// no more, even when Jira names a next page.
func TestChangedRowsIdleCursorStays(t *testing.T) {
	f, x := deltaSite(t)
	x.token = true
	start := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	want := cursor{V: cursorVersion, Board: "3", AfterMS: start.UnixMilli()}.bytes()
	q := tracker.DeltaQuery{Board: "3", Start: start}
	for i := range 3 {
		page, err := newProvider(t, f, nil).ChangedRows(t.Context(), q)
		if err != nil || len(page.Rows) != 0 || page.More || string(page.Next) != string(want) {
			t.Fatalf("read %d: %+v %v", i, page, err)
		}
		q = tracker.DeltaQuery{Board: "3", From: page.Next}
	}
}

// TestChangedRowsEmptyPageEndsWalk proves a walk whose last page is
// empty, after a page that named a next one, steps back by walkOverlap
// like any other walk's end, so a row the index took late, updated just
// before the walk's newest row, is delivered by the next walk.
func TestChangedRowsEmptyPageEndsWalk(t *testing.T) {
	f, x := deltaSite(t)
	x.token = true
	at := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	x.set(10001, at)
	x.set(10002, at)
	first, err := newProvider(t, f, nil).ChangedRows(t.Context(), tracker.DeltaQuery{Board: "3", Start: at})
	if err != nil || ids(first) != "10001 10002" || !first.More {
		t.Fatalf("page 1: %s more %v %v", ids(first), first.More, err)
	}
	rest, last := walk(t, f, first)
	if len(rest) != 1 || rest[0] != "" {
		t.Fatalf("the rest of the walk delivered %q", rest)
	}
	var c cursor
	if err := jsonUnmarshal(last.Next, &c); err != nil || c.AfterMS != at.Add(-walkOverlap).UnixMilli() || c.AfterID != 0 {
		t.Fatalf("end cursor %+v %v", c, err)
	}
	x.set(10003, at.Add(-30*time.Second))
	page, err := newProvider(t, f, nil).ChangedRows(t.Context(), tracker.DeltaQuery{Board: "3", From: last.Next})
	if err != nil || ids(page) != "10003 10001" {
		t.Errorf("the next walk: %s %v", ids(page), err)
	}
}

// TestChangedRowsStepBackFloor proves a walk's end never steps the cursor
// back before the epoch's first millisecond, which a cursor needs.
func TestChangedRowsStepBackFloor(t *testing.T) {
	f, _ := deltaSite(t)
	mid := cursor{V: cursorVersion, Board: "3", AfterMS: 1000, AfterID: 10004}
	page, err := newProvider(t, f, nil).ChangedRows(t.Context(), tracker.DeltaQuery{Board: "3", From: mid.bytes()})
	if err != nil || page.More {
		t.Fatalf("got %+v %v", page, err)
	}
	if want := (cursor{V: cursorVersion, Board: "3", AfterMS: 1}).bytes(); string(page.Next) != string(want) {
		t.Errorf("cursor %s, want %s", page.Next, want)
	}
}

// TestChangedRowsRefusesDisorder proves a page whose rows are not in
// update then ID order after the cursor fails the call rather than move
// the cursor where it could skip rows.
func TestChangedRowsRefusesDisorder(t *testing.T) {
	f, x := deltaSite(t)
	at := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	x.set(10001, at)
	x.set(10002, at.Add(time.Millisecond))
	x.reverse = true
	if _, err := newProvider(t, f, nil).ChangedRows(t.Context(), tracker.DeltaQuery{Board: "3", Start: at}); err == nil {
		t.Error("a page out of order was taken")
	}
	// A row before the cursor, which the JQL excludes.
	cur := cursor{V: cursorVersion, Board: "3", AfterMS: at.UnixMilli(), AfterID: 10001}
	f.On("GET", board3Issues, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.Reply(200, page("", issue("10003", "WEB-3", at.Add(-time.Millisecond).Format("2006-01-02T15:04:05.000-0700"), nil)))(w, r, b)
	})
	if _, err := newProvider(t, f, nil).ChangedRows(t.Context(), tracker.DeltaQuery{Board: "3", From: cur.bytes()}); err == nil {
		t.Error("a row before the cursor was taken")
	}
}

// TestChangedRowsRefusesCursors proves a cursor of another board, one
// this package did not write and a query with neither cursor nor start
// are refused before anything is read.
func TestChangedRowsRefusesCursors(t *testing.T) {
	f, _ := deltaSite(t)
	p := newProvider(t, f, nil)
	at := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC).UnixMilli()
	for name, q := range map[string]tracker.DeltaQuery{
		"another board's":   {Board: "3", From: cursor{V: cursorVersion, Board: "4", AfterMS: at}.bytes()},
		"not JSON":          {Board: "3", From: tracker.Cursor("o2")},
		"an unknown key":    {Board: "3", From: tracker.Cursor(`{"v":2,"board":"3","after_ms":1,"token":"o2"}`)},
		"trailing data":     {Board: "3", From: tracker.Cursor(`{"v":2,"board":"3","after_ms":1} {}`)},
		"the first version": {Board: "3", From: tracker.Cursor(`{"v":1,"board":"3","after":"2026-11-10T15:00:00Z"}`)},
		"a later version":   {Board: "3", From: cursor{V: 3, Board: "3", AfterMS: at}.bytes()},
		"no position":       {Board: "3", From: cursor{V: cursorVersion, Board: "3"}.bytes()},
		"a negative ID":     {Board: "3", From: cursor{V: cursorVersion, Board: "3", AfterMS: at, AfterID: -1}.bytes()},
		"no start":          {Board: "3"},
		"a start in 1970":   {Board: "3", Start: time.Unix(0, 0)},
	} {
		if _, err := p.ChangedRows(t.Context(), q); !errors.Is(err, tracker.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := f.Count("GET", board3Issues); n != 0 {
		t.Errorf("%d reads for refused cursors", n)
	}
}

// TestChangedRowsFailsOnWarnings proves a page Jira warned about fails
// the call rather than move the cursor past rows it may lack.
func TestChangedRowsFailsOnWarnings(t *testing.T) {
	f, x := deltaSite(t)
	x.set(10001, time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC))
	x.warn = "The search may be incomplete."
	if _, err := newProvider(t, f, nil).ChangedRows(t.Context(), tracker.DeltaQuery{Board: "3", Start: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)}); err == nil {
		t.Error("a warned page was taken")
	}
}

func jsonUnmarshal(c tracker.Cursor, v *cursor) error {
	got, err := readCursor(tracker.DeltaQuery{Board: "3", From: c})
	if err != nil {
		return fmt.Errorf("cursor %s: %w", c, err)
	}
	*v = got
	return nil
}
