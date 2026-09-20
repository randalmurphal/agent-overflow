package threadtools

import (
	"strings"
	"testing"
	"time"
)

func hit(id, title string, live LiveState) Hit {
	return Hit{
		Thread: Thread{ID: id, Title: title, Provider: "claude", Model: "opus-5", ProjectID: "p1", Project: "agent-overflow", LastActivity: time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC).UnixMilli()},
		Live:   live,
	}
}

// TestSearchWithoutPairingIsAFlatRowList, with nothing about computers in
// it at all.
func TestSearchWithoutPairingIsAFlatRowList(t *testing.T) {
	app := newFakeApp("Laptop")
	app.hits = []Hit{hit(localThreadID, "Parser work", LiveState{ActiveTurn: true}), hit(twinThreadID, "Idle one", LiveState{})}

	result := call(t, New(app), localCaller(), "thread_search", `{}`)
	list := rows(t, result["rows"])
	if len(list) != 2 {
		t.Fatalf("rows = %v", result["rows"])
	}
	first := list[0].(map[string]any)
	if first["state"] != StateRunning || first["title"] != "Parser work" || first["provider"] != "claude" {
		t.Fatalf("row = %v", first)
	}
	if first["last_activity"] != "2026-09-19T10:00:00Z" {
		t.Errorf("last_activity = %v", first["last_activity"])
	}
	for _, key := range []string{"computer_id", "computer", "computers", "errors"} {
		if _, present := result[key]; present {
			t.Errorf("an unpaired result carries %q", key)
		}
		if _, present := first[key]; present {
			t.Errorf("an unpaired row carries %q", key)
		}
	}
}

// TestSearchAppliesTheDocumentedDefaults: a listing and a query differ in
// row count and in whether archived threads are included.
func TestSearchAppliesTheDocumentedDefaults(t *testing.T) {
	app := newFakeApp("Laptop")
	server := New(app)

	call(t, server, localCaller(), "thread_search", `{}`)
	listing := app.searches[len(app.searches)-1]
	if listing.Limit != DefaultListLimit {
		t.Errorf("listing limit = %d, want %d", listing.Limit, DefaultListLimit)
	}
	if listing.Archived == nil || *listing.Archived {
		t.Errorf("a listing must exclude archived threads by default, got %v", listing.Archived)
	}

	call(t, server, localCaller(), "thread_search", `{"query":"parser"}`)
	ranked := app.searches[len(app.searches)-1]
	if ranked.Limit != DefaultSearchLimit || ranked.Query != "parser" {
		t.Errorf("query search = %+v", ranked)
	}
	if ranked.Archived != nil {
		t.Errorf("a query must include archived threads by default, got %v", *ranked.Archived)
	}

	call(t, server, localCaller(), "thread_search", `{"spawned_by_me":true,"archived":true,"since":"2026-09-01T00:00:00Z","provider":"codex","state":"awaiting-input","kind":"tool","project_id":"p1"}`)
	filtered := app.searches[len(app.searches)-1]
	if filtered.SpawnedBy != localCaller().ThreadID {
		t.Errorf("spawned_by_me did not become the caller's thread: %q", filtered.SpawnedBy)
	}
	if filtered.Archived == nil || !*filtered.Archived {
		t.Errorf("archived did not reach the app: %v", filtered.Archived)
	}
	if filtered.SinceUnixMs != time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixMilli() {
		t.Errorf("since = %d", filtered.SinceUnixMs)
	}
	if filtered.State != "awaiting-input" || filtered.Kind != "tool" || filtered.Provider != "codex" || filtered.ProjectID != "p1" {
		t.Errorf("filters = %+v", filtered)
	}
}

// TestSearchValidatesItsFilters rather than passing nonsense to the store
// and returning an empty list the agent cannot explain.
func TestSearchValidatesItsFilters(t *testing.T) {
	server := New(newFakeApp("Laptop"))
	callErr(t, server, localCaller(), "thread_search", `{"kind":"anything"}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_search", `{"state":"busy"}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_search", `{"since":"yesterday"}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_search", `{"limit":500}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_search", `{"computers":["studio"]}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_search", `{"cursor":"garbage"}`, CodeInvalidRequest)
}

// TestSearchReportsIndexing so a thin result is explained rather than
// read as "there is nothing here".
func TestSearchReportsIndexing(t *testing.T) {
	app := newFakeApp("Laptop")
	app.indexing = true
	app.hits = []Hit{hit(localThreadID, "Parser work", LiveState{})}
	result := call(t, New(app), localCaller(), "thread_search", `{"query":"parser"}`)
	if result["indexing"] != true {
		t.Fatalf("indexing = %v", result["indexing"])
	}
	if note, _ := result["note"].(string); !strings.Contains(note, "still building its search index") {
		t.Errorf("note = %q", note)
	}
}

// TestSearchPagesWithAnOpaqueCursor.
func TestSearchPagesWithAnOpaqueCursor(t *testing.T) {
	app := newFakeApp("Laptop")
	for index := range 25 {
		app.hits = append(app.hits, hit(localThreadID[:len(localThreadID)-2]+string(rune('a'+index/10))+string(rune('a'+index%10)), "Thread", LiveState{}))
	}
	server := New(app)

	first := call(t, server, localCaller(), "thread_search", `{"limit":10}`)
	if len(rows(t, first["rows"])) != 10 || first["more"] != true || first["cursor"] == nil {
		t.Fatalf("first page = %v", first)
	}
	second := call(t, server, localCaller(), "thread_search", mustJSON(t, map[string]any{"limit": 10, "cursor": first["cursor"]}))
	if len(rows(t, second["rows"])) != 10 {
		t.Fatalf("second page has %d rows", len(rows(t, second["rows"])))
	}
	if app.searches[1].Offset != 10 {
		t.Fatalf("the second page asked the app for offset %d, want 10", app.searches[1].Offset)
	}
	third := call(t, server, localCaller(), "thread_search", mustJSON(t, map[string]any{"limit": 10, "cursor": second["cursor"]}))
	if len(rows(t, third["rows"])) != 5 || third["more"] != nil || third["cursor"] != nil {
		t.Fatalf("last page = %v", third)
	}
	if firstID := field(t, rows(t, second["rows"])[0], "thread_id"); firstID == field(t, rows(t, first["rows"])[0], "thread_id") {
		t.Fatal("the second page repeats the first")
	}
}

// TestSearchGroupsRowsPerComputer and never merges ranks from separate
// indexes into one order.
func TestSearchGroupsRowsPerComputer(t *testing.T) {
	p := newPair(t)
	p.local.hits = []Hit{hit(localThreadID, "Local work", LiveState{})}
	p.remote.hits = []Hit{hit(remoteThreadID, "Remote work", LiveState{PendingApprovals: 1})}

	result := call(t, p.server, localCaller(), "thread_search", `{"query":"work"}`)
	groups := rows(t, result["computers"])
	if len(groups) != 2 {
		t.Fatalf("computers = %v", result["computers"])
	}
	if field(t, groups[0], "computer") != "Laptop" || field(t, groups[1], "computer") != "Studio" {
		t.Fatalf("groups are not [caller, peer]: %v", result["computers"])
	}
	remoteRows := rows(t, field(t, groups[1], "rows"))
	if len(remoteRows) != 1 {
		t.Fatalf("remote rows = %v", field(t, groups[1], "rows"))
	}
	row := remoteRows[0].(map[string]any)
	if row["computer_id"] != "studio" || row["computer"] != "Studio" {
		t.Fatalf("a forwarded row is not stamped with the computer that holds it: %v", row)
	}
	if row["state"] != StatePendingApproval {
		t.Errorf("state = %v", row["state"])
	}
	if _, present := result["rows"]; present {
		t.Error("a grouped result must not also carry a flat rows list")
	}
	// The destination is told to answer about itself only, so it cannot
	// fan out again and loop.
	if computers := p.remote.searches[0]; computers.Query != "work" {
		t.Errorf("the peer received %+v", computers)
	}
	if p.remote.peers != nil && len(p.remote.computers) != 0 {
		t.Error("the destination fanned out again")
	}
}

// TestSearchTurnsASilentComputerIntoAnErrorRow and still answers with
// what the rest of the computers hold.
func TestSearchTurnsASilentComputerIntoAnErrorRow(t *testing.T) {
	p := newPair(t)
	p.local.hits = []Hit{hit(localThreadID, "Local work", LiveState{})}
	p.local.peers["studio"] = brokenPeer{computer: Computer{ID: "studio", Name: "Studio"}, err: publicf(CodeUnreachable, "Studio is offline.")}

	result := call(t, p.server, localCaller(), "thread_search", `{"query":"work"}`)
	groups := rows(t, result["computers"])
	if len(groups) != 1 || field(t, groups[0], "computer") != "Laptop" {
		t.Fatalf("computers = %v", result["computers"])
	}
	errs := rows(t, result["errors"])
	if len(errs) != 1 {
		t.Fatalf("errors = %v", result["errors"])
	}
	row := errs[0].(map[string]any)
	if row["computer_id"] != "studio" || row["error_code"] != CodeUnreachable || !strings.Contains(row["error"].(string), "offline") {
		t.Fatalf("error row = %v", row)
	}
	if note, _ := result["note"].(string); !strings.Contains(note, "did not answer") {
		t.Errorf("note = %q", note)
	}
}

// TestSearchCarriesADestinationRefusalAsItsErrorRow: a peer answers about
// itself only, so when its own index rejects the query it reports an
// error row and no group. That must come back as the peer's failure, not
// as an empty computer that reads "no threads there".
func TestSearchCarriesADestinationRefusalAsItsErrorRow(t *testing.T) {
	p := newPair(t)
	p.local.hits = []Hit{hit(localThreadID, "Local work", LiveState{})}
	p.remote.onSearch = func(SearchQuery) (SearchPage, error) {
		return SearchPage{}, publicf(CodeInvalidRequest, "That search query is not valid.")
	}

	result := call(t, p.server, localCaller(), "thread_search", `{"query":"tt-mr"}`)
	groups := rows(t, result["computers"])
	if len(groups) != 1 || field(t, groups[0], "computer") != "Laptop" {
		t.Fatalf("computers = %v, want the caller's group only", result["computers"])
	}
	errs := rows(t, result["errors"])
	if len(errs) != 1 {
		t.Fatalf("errors = %v", result["errors"])
	}
	row := errs[0].(map[string]any)
	if row["computer_id"] != "studio" || row["error_code"] != CodeInvalidRequest || !strings.Contains(row["error"].(string), "not valid") {
		t.Fatalf("error row = %v", row)
	}
}

// TestSearchLocalOnlyFilterSkipsThePeersEntirely.
func TestSearchLocalOnlyFilterSkipsThePeersEntirely(t *testing.T) {
	p := newPair(t)
	p.local.hits = []Hit{hit(localThreadID, "Local work", LiveState{})}

	result := call(t, p.server, localCaller(), "thread_search", `{"computers":["local"]}`)
	if len(rows(t, result["computers"])) != 1 {
		t.Fatalf("computers = %v", result["computers"])
	}
	if len(p.peer.queries) != 0 {
		t.Fatalf("the peer was searched anyway: %v", p.peer.queries)
	}
}

// TestSearchByThreadIdRunsOnlyWhereThatThreadLives.
func TestSearchByThreadIdRunsOnlyWhereThatThreadLives(t *testing.T) {
	p := newPair(t)
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Remote"})
	p.remote.hits = []Hit{hit(remoteThreadID, "Remote", LiveState{})}

	result := call(t, p.server, localCaller(), "thread_search", `{"query":"panic","thread_id":"`+remoteThreadID+`"}`)
	groups := rows(t, result["computers"])
	if len(groups) != 1 || field(t, groups[0], "computer_id") != "studio" {
		t.Fatalf("computers = %v", result["computers"])
	}
	if len(p.local.searches) != 0 {
		t.Fatal("the caller searched its own database for a thread it does not hold")
	}
	if p.remote.searches[0].ThreadID != remoteThreadID {
		t.Errorf("the destination was not given the thread filter: %+v", p.remote.searches[0])
	}
}

// TestSearchPagesEveryComputerPastItsOwnRows. A computer that ran out of
// rows on page one still has to resume past them, or its whole first page
// comes back again beside the other computer's second.
func TestSearchPagesEveryComputerPastItsOwnRows(t *testing.T) {
	p := newPair(t)
	for index := range 25 {
		p.local.hits = append(p.local.hits, hit(localThreadID[:len(localThreadID)-2]+string(rune('a'+index/10))+string(rune('a'+index%10)), "Local", LiveState{}))
	}
	p.remote.hits = []Hit{hit(remoteThreadID, "Remote", LiveState{})}

	first := call(t, p.server, localCaller(), "thread_search", `{"limit":10}`)
	if first["cursor"] == nil {
		t.Fatalf("first page = %v", first)
	}
	if got := len(rows(t, field(t, rows(t, first["computers"])[1], "rows"))); got != 1 {
		t.Fatalf("the peer's first page has %d rows, want 1", got)
	}

	second := call(t, p.server, localCaller(), "thread_search", mustJSON(t, map[string]any{"limit": 10, "cursor": first["cursor"]}))
	groups := rows(t, second["computers"])
	if len(groups) != 2 {
		t.Fatalf("computers = %v", second["computers"])
	}
	if got := len(rows(t, field(t, groups[1], "rows"))); got != 0 {
		t.Fatalf("the peer repeated %d rows on the second page", got)
	}
	if p.remote.searches[1].Offset != 1 {
		t.Fatalf("the peer's second page asked for offset %d, want 1", p.remote.searches[1].Offset)
	}
	if p.local.searches[1].Offset != 10 {
		t.Fatalf("this computer's second page asked for offset %d, want 10", p.local.searches[1].Offset)
	}
}

// TestSearchCursorCarriesTheFiltersItContinues. A cursor is a row offset
// into one filtered set; spending it on another set would skip rows and
// say nothing about it.
func TestSearchCursorCarriesTheFiltersItContinues(t *testing.T) {
	app := newFakeApp("Laptop")
	for index := range 25 {
		app.hits = append(app.hits, hit(localThreadID[:len(localThreadID)-2]+string(rune('a'+index/10))+string(rune('a'+index%10)), "Thread", LiveState{}))
	}
	server := New(app)

	first := call(t, server, localCaller(), "thread_search", `{"query":"parser","limit":10}`)
	if first["cursor"] == nil {
		t.Fatalf("first page = %v", first)
	}
	message := callErr(t, server, localCaller(), "thread_search",
		mustJSON(t, map[string]any{"query": "parser", "project_id": "p2", "limit": 10, "cursor": first["cursor"]}), CodeInvalidRequest)
	if !strings.Contains(message, "carries the filters") {
		t.Errorf("message = %q", message)
	}
	callErr(t, server, localCaller(), "thread_search",
		mustJSON(t, map[string]any{"query": "other", "limit": 10, "cursor": first["cursor"]}), CodeInvalidRequest)

	// The same filters with a different page size are the same search.
	call(t, server, localCaller(), "thread_search", mustJSON(t, map[string]any{"query": "parser", "limit": 5, "cursor": first["cursor"]}))
}

// TestSearchByThreadIdSaysWhenTheFanOutWasIncomplete: the id resolved to
// this computer only because another one never answered, so the result has
// to say a thread of that id there was not considered.
func TestSearchByThreadIdSaysWhenTheFanOutWasIncomplete(t *testing.T) {
	p := newPair(t)
	p.local.addThread(Thread{ID: localThreadID, Title: "Local"})
	p.local.hits = []Hit{hit(localThreadID, "Local", LiveState{})}
	p.local.peers["studio"] = brokenPeer{computer: Computer{ID: "studio", Name: "Studio"}, err: publicf(CodeUnreachable, "Studio is offline.")}

	result := call(t, p.server, localCaller(), "thread_search", mustJSON(t, map[string]any{"thread_id": localThreadID[:12]}))
	note, _ := result["note"].(string)
	if !strings.Contains(note, "Partial:") || !strings.Contains(note, "Studio") {
		t.Fatalf("note = %q", note)
	}
}

// TestSearchQueryIsBounded: past a few kilobytes a query is not a search
// term, and the index refuses it far less clearly than this does.
func TestSearchQueryIsBounded(t *testing.T) {
	server := New(newFakeApp("Laptop"))
	message := callErr(t, server, localCaller(), "thread_search",
		mustJSON(t, map[string]any{"query": strings.Repeat("q", MaxQueryBytes+1)}), CodeInvalidRequest)
	if !strings.Contains(message, "at most 4096 bytes") {
		t.Errorf("message = %q", message)
	}
	call(t, server, localCaller(), "thread_search", mustJSON(t, map[string]any{"query": strings.Repeat("q", MaxQueryBytes)}))
}
