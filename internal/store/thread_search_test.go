package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// searchIndexRows reads the mapping table, which is the only enumerable half
// of a contentless index. Every hit joins one of these rows, so a mapping row
// that should not exist is a hit that should not exist.
func searchIndexRows(t *testing.T, s *Store, threadID string) map[string]string {
	t.Helper()
	rows, err := s.db.Query(
		`SELECT item_id, source || ':' || kind FROM thread_search_rows WHERE thread_id = ?`, threadID)
	if err != nil {
		t.Fatalf("read index rows: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var itemID, describe string
		if err := rows.Scan(&itemID, &describe); err != nil {
			t.Fatalf("scan index row: %v", err)
		}
		out[itemID] = describe
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate index rows: %v", err)
	}
	return out
}

// timelineIndexable lists what the logical timeline says should be indexed:
// settled rows of an indexed kind, on whichever arm currently answers.
func timelineIndexable(t *testing.T, s *Store, threadID string) map[string]string {
	t.Helper()
	rows, err := s.db.Query(
		`SELECT id, kind, status FROM timeline_items WHERE thread_id = ?`, threadID)
	if err != nil {
		t.Fatalf("read timeline items: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, kind, status string
		if err := rows.Scan(&id, &kind, &status); err != nil {
			t.Fatalf("scan timeline item: %v", err)
		}
		indexed, ok := threadSearchItemKinds[kind]
		if !ok || !settledItemStatus(status) {
			continue
		}
		out[id] = indexed
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate timeline items: %v", err)
	}
	return out
}

func mustSearch(t *testing.T, s *Store, query string, filter ThreadSearchFilter) []ThreadSearchHit {
	t.Helper()
	hits, err := s.SearchThreads(query, filter)
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	return hits
}

func hitIDs(hits []ThreadSearchHit) []string {
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.Source+":"+hit.ThreadID+":"+hit.ItemID)
	}
	sort.Strings(out)
	return out
}

// The index answers for the logical timeline, not for one physical table: an
// imported row is indexed on the import arm, an override moves it to the item
// arm, and preparation moves completed content to the import arm.
func TestSearchIndexTracksTheLogicalTimeline(t *testing.T) {
	s := newTestStore(t)
	newImportTargetThread(t, s, "t-index")
	if err := s.ApplyImportBatch("t-index", ImportBatch{
		Turns: []Turn{{TurnID: "turn-index", ThreadID: "t-index", TurnIndex: 0, StartedAt: 1}},
		Rows: []ImportRow{
			{Item: Item{ID: "imported-user", TurnIndex: 0, ItemIndex: 0, Kind: "user_text",
				Role: "user", Status: "completed", Summary: "imported question about quokkas",
				CreatedAt: 1, UpdatedAt: 1}},
			{Item: Item{ID: "imported-think", TurnIndex: 0, ItemIndex: 1, Kind: "thinking",
				Role: "assistant", Status: "completed", Summary: "quokkas are not indexed",
				CreatedAt: 1, UpdatedAt: 1}},
			{Item: Item{ID: "imported-answer", TurnIndex: 0, ItemIndex: 2, Kind: "assistant_text",
				Role: "assistant", Status: "completed", Summary: "imported answer about quokkas",
				CreatedAt: 1, UpdatedAt: 1}},
		},
	}); err != nil {
		t.Fatalf("apply import batch: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "local-tool", ThreadID: "t-index", TurnIndex: 1, ItemIndex: 0, Kind: "tool_call",
		Role: "assistant", Status: "completed", Summary: "Read quokkas.md", ToolName: "Read",
		CreatedAt: 2, UpdatedAt: 2,
	}); err != nil {
		t.Fatalf("insert local tool call: %v", err)
	}

	indexed := searchIndexRows(t, s, "t-index")
	// The title row rides along with an empty item id.
	if indexed[""] != "item:title" {
		t.Errorf("title index row = %q, want item:title", indexed[""])
	}
	delete(indexed, "")
	want := map[string]string{
		"imported-user":   "import:user",
		"imported-answer": "import:assistant",
		"local-tool":      "item:tool",
	}
	if fmt.Sprint(indexed) != fmt.Sprint(want) {
		t.Errorf("index rows = %v, want %v", indexed, want)
	}
	for id, kind := range timelineIndexable(t, s, "t-index") {
		if got := indexed[id]; !strings.HasSuffix(got, ":"+kind) {
			t.Errorf("timeline row %s (%s) indexed as %q", id, kind, got)
		}
	}

	hits := mustSearch(t, s, "quokkas", ThreadSearchFilter{})
	if got := hitIDs(hits); len(got) != 3 {
		t.Fatalf("hits = %v, want three rows", got)
	}
	for _, hit := range hits {
		if hit.Snippet == "" {
			t.Errorf("hit %s/%s has no snippet", hit.ThreadID, hit.ItemID)
		}
		if hit.Kind == ThreadSearchKindTitle {
			t.Errorf("title matched a body-only term: %+v", hit)
		}
	}

	// Editing an imported row localizes it: the index row has to move arms,
	// or the same message answers twice.
	meta := `{"edited":true}`
	if _, err := s.UpdateItemFields("t-index", "imported-answer", ItemPartialUpdate{Meta: &meta}); err != nil {
		t.Fatalf("localize imported item: %v", err)
	}
	moved := searchIndexRows(t, s, "t-index")
	if moved["imported-answer"] != "item:assistant" {
		t.Errorf("localized row indexed as %q, want item:assistant", moved["imported-answer"])
	}
	if len(mustSearch(t, s, "quokkas", ThreadSearchFilter{ThreadIDs: []string{"t-index"}})) != 3 {
		t.Error("localizing a row changed the number of matches")
	}

	// Truncation preserves shared survivors; both arms describe the kept rows.
	if _, _, err := s.DeleteConversationFromTurn("t-index", 1); err != nil {
		t.Fatalf("delete conversation from turn: %v", err)
	}
	after := searchIndexRows(t, s, "t-index")
	delete(after, "")
	wantAfter := map[string]string{
		"imported-user":   "import:user",
		"imported-answer": "item:assistant",
	}
	if fmt.Sprint(after) != fmt.Sprint(wantAfter) {
		t.Errorf("index rows after truncation = %v, want %v", after, wantAfter)
	}
	for _, hit := range mustSearch(t, s, "quokkas", ThreadSearchFilter{}) {
		if hit.ItemID == "local-tool" {
			t.Error("a deleted row still matches")
		}
	}
}

// Streaming writes must not touch the index: a delta per token would
// re-tokenize the whole row at streaming rate. The settle write indexes once.
func TestSearchIndexIgnoresStreamingUntilSettlement(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-stream")
	streaming := Item{
		ID: "stream-1", ThreadID: "t-stream", TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant", Status: "streaming",
		Summary: "", CreatedAt: 1, UpdatedAt: 1,
	}
	if _, err := s.UpsertItem(streaming, nil); err != nil {
		t.Fatalf("insert streaming item: %v", err)
	}
	for i := 0; i < 200; i++ {
		if _, err := s.AppendItemSummary("t-stream", "stream-1", "wombat delta ", int64(i+2)); err != nil {
			t.Fatalf("append delta %d: %v", i, err)
		}
		if rows := searchIndexRows(t, s, "t-stream"); len(rows) != 1 {
			// Only the title row may exist while the message streams.
			t.Fatalf("index rows during streaming = %v", rows)
		}
	}
	if hits := mustSearch(t, s, "wombat", ThreadSearchFilter{}); len(hits) != 0 {
		t.Fatalf("streaming text is searchable: %v", hits)
	}

	settled := streaming
	settled.Status = "completed"
	settled.Summary = strings.Repeat("wombat delta ", 200)
	settled.UpdatedAt = 500
	if _, err := s.UpsertItem(settled, nil); err != nil {
		t.Fatalf("settle item: %v", err)
	}
	hits := mustSearch(t, s, "wombat", ThreadSearchFilter{})
	if len(hits) != 1 || hits[0].ItemID != "stream-1" {
		t.Fatalf("settled hits = %v", hitIDs(hits))
	}
	if !strings.Contains(hits[0].Snippet, "wombat") {
		t.Errorf("snippet %q does not contain the match", hits[0].Snippet)
	}
}

// The background build walks rows written before the index existed. It has to
// resume from the cursor after a restart, skip streaming rows, and stop
// reporting itself as indexing when it finishes.
func TestBuildSearchIndexResumesFromTheCursor(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-build")

	// Written with SQL so the rows arrive unindexed, which is what an
	// upgraded database looks like before the build runs.
	const total = 620
	for i := 0; i < total; i++ {
		status := "completed"
		if i == total-1 {
			status = "streaming"
		}
		if _, err := s.db.Exec(`
			INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
				summary, meta, created_at, updated_at)
			VALUES (?, 't-build', 0, ?, 'user_text', 'user', ?, ?, '{}', 1, 1)`,
			fmt.Sprintf("build-%04d", i), i, status,
			fmt.Sprintf("pangolin message %d", i),
		); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	if _, err := s.db.Exec(`DELETE FROM thread_search_rows`); err != nil {
		t.Fatalf("clear index rows: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM thread_search`); err != nil {
		t.Fatalf("clear index text: %v", err)
	}

	indexing, err := s.SearchIndexing()
	if err != nil {
		t.Fatalf("probe indexing: %v", err)
	}
	if !indexing {
		t.Fatal("a database that has never built its index must report indexing")
	}

	progress, found, err := s.searchBuildProgress()
	if err != nil || !found {
		t.Fatalf("read build progress: found=%v err=%v", found, err)
	}
	if _, err := s.buildSearchIndexItems(progress); err != nil {
		t.Fatalf("first build batch: %v", err)
	}
	if got := len(searchIndexRows(t, s, "t-build")); got != threadSearchBuildBatch {
		t.Fatalf("rows after one batch = %d, want %d", got, threadSearchBuildBatch)
	}
	resumed, found, err := s.searchBuildProgress()
	if err != nil || !found {
		t.Fatalf("read resumed progress: found=%v err=%v", found, err)
	}
	if resumed.cursorItemID != fmt.Sprintf("build-%04d", threadSearchBuildBatch-1) {
		t.Fatalf("cursor = %q after one batch", resumed.cursorItemID)
	}

	// A restart re-reads the progress row and finishes the walk.
	if err := s.BuildSearchIndex(context.Background()); err != nil {
		t.Fatalf("finish build: %v", err)
	}
	rows := searchIndexRows(t, s, "t-build")
	if len(rows) != total { // total-1 settled messages plus the title row
		t.Fatalf("indexed rows = %d, want %d", len(rows), total)
	}
	if _, streaming := rows[fmt.Sprintf("build-%04d", total-1)]; streaming {
		t.Error("the build indexed a row that is still streaming")
	}
	indexing, err = s.SearchIndexing()
	if err != nil {
		t.Fatalf("probe indexing after build: %v", err)
	}
	if indexing {
		t.Error("a finished build must stop reporting indexing")
	}
	if err := s.BuildSearchIndex(context.Background()); err != nil {
		t.Fatalf("rerun finished build: %v", err)
	}

	hits := mustSearch(t, s, "pangolin", ThreadSearchFilter{Limit: 5})
	if len(hits) != 5 {
		t.Fatalf("hits = %d, want the requested page of 5", len(hits))
	}
}

// Visibility is applied at query time by joining threads: a scratch thread
// is invisible unless the caller names it, and a workflow thread is work an
// agent may find (docs/specs/agent-thread-tools.md, thread_search defaults).
func TestSearchThreadsIncludesWorkflowThreadsAndHidesUnnamedScratch(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-plain")
	for id, mode := range map[string]string{"t-wf": "workflow", "t-scratch": "scratch"} {
		thread := makeThread(id, "claude")
		thread.Mode = mode
		if err := s.CreateThread(thread); err != nil {
			t.Fatalf("create %s thread: %v", mode, err)
		}
	}
	for _, id := range []string{"t-plain", "t-wf", "t-scratch"} {
		if err := s.InsertItem(Item{
			ID: id + "-msg", ThreadID: id, TurnIndex: 0, ItemIndex: 0, Kind: "user_text",
			Role: "user", Status: "completed", Summary: "numbat sighting", CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("insert message in %s: %v", id, err)
		}
	}

	hits := hitIDs(mustSearch(t, s, "numbat", ThreadSearchFilter{}))
	joined := strings.Join(hits, ",")
	if len(hits) != 2 || !strings.Contains(joined, "t-plain") || !strings.Contains(joined, "t-wf") {
		t.Fatalf("default hits = %v, want the plain and the workflow thread", hits)
	}
	if strings.Contains(joined, "t-scratch") {
		t.Fatalf("default hits = %v, want no scratch thread", hits)
	}

	named := mustSearch(t, s, "numbat", ThreadSearchFilter{ScratchThreadIDs: []string{"t-scratch"}})
	if len(named) != 3 {
		t.Fatalf("hits with the scratch thread named = %v, want three", hitIDs(named))
	}

	// Promotion is a mode change with no reindex; the same rows answer.
	if err := s.InsertScratchThread(ScratchThread{
		ThreadID: "t-scratch", SourceThreadID: "t-plain", ReturnMode: "chat",
	}); err != nil {
		t.Fatalf("insert scratch record: %v", err)
	}
	if _, err := s.PromoteScratchThread("t-scratch"); err != nil {
		t.Fatalf("promote scratch thread: %v", err)
	}
	if got := mustSearch(t, s, "numbat", ThreadSearchFilter{}); len(got) != 3 {
		t.Fatalf("hits after promotion = %v, want three", hitIDs(got))
	}
}

// Titles are indexed with the message text, updated when they change, and
// dropped with the thread.
func TestSearchThreadsIndexesTitlesAndThreadDeletion(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("t-title", "claude")
	thread.Title = "Aardvark planning"
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	hits := mustSearch(t, s, "aardvark", ThreadSearchFilter{Kinds: []string{ThreadSearchKindTitle}})
	if len(hits) != 1 || hits[0].ItemID != "" || hits[0].Snippet != "Aardvark planning" {
		t.Fatalf("title hits = %+v", hits)
	}

	if err := s.UpdateTitle("t-title", "Bilby planning"); err != nil {
		t.Fatalf("rename thread: %v", err)
	}
	if got := mustSearch(t, s, "aardvark", ThreadSearchFilter{}); len(got) != 0 {
		t.Errorf("the old title still matches: %v", got)
	}
	if got := mustSearch(t, s, "bilby", ThreadSearchFilter{}); len(got) != 1 {
		t.Errorf("the new title does not match: %v", got)
	}

	if err := s.DeleteThread("t-title"); err != nil {
		t.Fatalf("delete thread: %v", err)
	}
	if got := mustSearch(t, s, "bilby", ThreadSearchFilter{}); len(got) != 0 {
		t.Errorf("a deleted thread still matches: %v", got)
	}
	var orphans int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM thread_search_rows WHERE thread_id = 't-title'`).Scan(&orphans); err != nil {
		t.Fatalf("count orphan index rows: %v", err)
	}
	if orphans != 0 {
		t.Errorf("index rows left behind = %d", orphans)
	}
}

// A malformed FTS5 query is the caller's to report, not an empty result that
// looks like "no matches".
func TestSearchThreadsRejectsMalformedQueries(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SearchThreads("   ", ThreadSearchFilter{}); err == nil {
		t.Error("an empty query must be refused")
	}
	for _, query := range []string{`"unbalanced`, "matched AND", "((", "OR"} {
		if _, err := s.SearchThreads(query, ThreadSearchFilter{}); err == nil {
			t.Errorf("malformed match expression %q must be refused", query)
		}
	}
	if _, err := s.SearchThreads("quokka", ThreadSearchFilter{Kinds: []string{"nonsense"}}); err == nil {
		t.Error("an unknown kind filter must be refused")
	}
}

// seedTransfer writes one transfer row directly, so the agreement test can
// stand every combination of direction, kind and phase beside the probe
// without driving the phase machine to reach each one.
func seedTransfer(t *testing.T, s *Store, threadID, direction, kind, phase string, archiveSize int64) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO thread_transfers (id, thread_id, target_thread_id, peer_backend_id, kind,
		     direction, phase, activation_hash, private_state, archive_size, created_at, updated_at)
		 VALUES (?, ?, '', 'peer', ?, ?, ?, '', '{}', ?, 1, 1)`,
		threadID+":"+phase, threadID, kind, direction, phase, archiveSize,
	); err != nil {
		t.Fatalf("insert %s %s transfer in phase %s: %v", direction, kind, phase, err)
	}
}

// TestThreadTransferFilterMatchesTheAccessProbe pins the SQL half of the
// transfer rule against CheckThreadTransferAccess. A listing that offered a
// row the read then refused would leave the caller's page offset counting
// rows it never received.
func TestThreadTransferFilterMatchesTheAccessProbe(t *testing.T) {
	index := 0
	for _, direction := range []string{"incoming", "outgoing"} {
		for _, kind := range []string{"move", "copy"} {
			for _, phase := range []string{"preparing", "prepared", "committed", "complete", "canceled"} {
				for _, archiveSize := range []int64{0, 128} {
					index++
					id := fmt.Sprintf("t-%02d", index)
					t.Run(fmt.Sprintf("%s-%s-%s-%d", direction, kind, phase, archiveSize), func(t *testing.T) {
						s := newTestStore(t)
						mustCreateThread(t, s, id)
						seedTransfer(t, s, id, direction, kind, phase, archiveSize)

						readable := s.CheckThreadTransferAccess(id) == nil
						listed, err := s.ListThreadsByActivity(ThreadSearchFilter{Limit: 10})
						if err != nil {
							t.Fatalf("ListThreadsByActivity: %v", err)
						}
						if kept := len(listed) == 1; kept != readable {
							t.Fatalf("the listing %s the thread, the probe %s it",
								transferVerdict(kept), transferVerdict(readable))
						}
					})
				}
			}
		}
	}
}

func transferVerdict(kept bool) string {
	if kept {
		return "kept"
	}
	return "refused"
}

// TestSearchThreadsPagesByOffset pins the paging both halves promise: the
// offset counts the rows the caller received, so consecutive pages neither
// repeat a row nor skip one.
func TestSearchThreadsPagesByOffset(t *testing.T) {
	s := newTestStore(t)
	for index := range 5 {
		id := fmt.Sprintf("t-page-%d", index)
		mustCreateThread(t, s, id)
		if err := s.InsertItem(Item{
			ID: id + "-msg", ThreadID: id, TurnIndex: 0, ItemIndex: 0, Kind: "user_text",
			Role: "user", Status: "completed", Summary: "bilby report", CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("insert message in %s: %v", id, err)
		}
		seedCompletedTurn(t, s, id, 0, int64(1_000+index*10), int64(1_005+index*10))
	}

	whole := hitIDs(mustSearch(t, s, "bilby", ThreadSearchFilter{Limit: 10}))
	if len(whole) != 5 {
		t.Fatalf("unpaged hits = %v, want five", whole)
	}
	var paged []string
	for offset := 0; offset < 5; offset += 2 {
		page := mustSearch(t, s, "bilby", ThreadSearchFilter{Limit: 2, Offset: offset})
		if want := min(2, 5-offset); len(page) != want {
			t.Fatalf("page at offset %d = %d hits, want %d", offset, len(page), want)
		}
		paged = append(paged, hitIDs(page)...)
	}
	sort.Strings(paged)
	if strings.Join(paged, ",") != strings.Join(whole, ",") {
		t.Fatalf("paged hits = %v, want %v with no repeat and no skip", paged, whole)
	}

	// The listing pages the same way, in its own order.
	var listed []string
	for offset := 0; offset < 5; offset += 2 {
		page, err := s.ListThreadsByActivity(ThreadSearchFilter{Limit: 2, Offset: offset})
		if err != nil {
			t.Fatalf("ListThreadsByActivity(offset %d): %v", offset, err)
		}
		listed = append(listed, threadIDsOf(page)...)
	}
	if strings.Join(listed, ",") != "t-page-4,t-page-3,t-page-2,t-page-1,t-page-0" {
		t.Fatalf("paged listing = %v", listed)
	}
}
