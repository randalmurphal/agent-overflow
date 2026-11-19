package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// seedSettledAgentRow writes a request and settles it at settledAt.
func seedSettledAgentRow(t *testing.T, a *App, row store.ThreadRequest, settledAt int64, answer string) {
	t.Helper()
	row.State = store.ThreadRequestRunning
	if err := a.store.InsertThreadRequest(row); err != nil {
		t.Fatalf("insert %s: %v", row.Token, err)
	}
	settled, err := a.store.SettleThreadRequest(row.Token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadRequestReplied, Answer: []byte(answer), AnswerKind: store.ThreadAnswerReply, SettledAt: settledAt,
	})
	if err != nil || !settled {
		t.Fatalf("settle %s: settled=%v err=%v", row.Token, settled, err)
	}
}

// Past lists a thread's settled rows newest settled first across pages,
// ties by token, each once, flat, with the facts an open row carries: an
// answered ask opens the thread it asked about and shows its answer, a
// spawn the retention floor reduced to lineage shows none, and a spawn's
// row says where its thread is placed and whether it is archived. Open
// rows and quiet sends are not Past.
func TestPastAgentRowsPageTheSettledRowsNewestFirst(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	lead := noticeTestThread(t, a, "past-lead")
	old := noticeTestThread(t, a, "past-old")
	shelved := noticeTestThread(t, a, "past-shelved")
	fresh := noticeTestThread(t, a, "past-fresh")
	asked := noticeTestThread(t, a, "past-asked")
	open := noticeTestThread(t, a, "past-open")
	seedSettledAgentRow(t, a, store.ThreadRequest{Token: "old", CallerThreadID: lead.ID, Kind: store.ThreadRequestSpawn, TargetThreadID: old.ID, CreatedAt: 1}, 2, "long ago")
	if _, _, err := a.sweepThreadRequestsBefore(10); err != nil {
		t.Fatal(err)
	}
	seedSettledAgentRow(t, a, store.ThreadRequest{Token: "b-shelved", CallerThreadID: lead.ID, Kind: store.ThreadRequestSpawn, TargetThreadID: shelved.ID, CreatedAt: 20}, 300, "shelved it")
	seedSettledAgentRow(t, a, store.ThreadRequest{Token: "a-fresh", CallerThreadID: lead.ID, Kind: store.ThreadRequestSpawn, TargetThreadID: fresh.ID, CreatedAt: 20}, 300, "fresh one")
	seedSettledAgentRow(t, a, store.ThreadRequest{
		Token: "ask", CallerThreadID: lead.ID, Kind: store.ThreadRequestAsk, TargetThreadID: "gone-scratch-copy",
		TargetThreadTitle: "Asked", OriginThreadID: asked.ID, CreatedAt: 20,
	}, 200, "it says yes")
	seedSettledAgentRow(t, a, store.ThreadRequest{Token: "told", CallerThreadID: lead.ID, Kind: store.ThreadRequestSend, TargetThreadID: fresh.ID, Notify: true, CreatedAt: 20}, 100, "noted")
	seedSettledAgentRow(t, a, store.ThreadRequest{Token: "quiet", CallerThreadID: lead.ID, Kind: store.ThreadRequestSend, TargetThreadID: asked.ID, CreatedAt: 20}, 250, "not owed")
	if err := a.store.InsertThreadRequest(store.ThreadRequest{
		Token: "open", CallerThreadID: lead.ID, Kind: store.ThreadRequestSpawn, TargetThreadID: open.ID, State: store.ThreadRequestRunning, CreatedAt: 30,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.store.ArchiveThread(shelved.ID); err != nil {
		t.Fatal(err)
	}

	var rows []ThreadAgentRow
	after := store.PastAgentRowsCursor{}
	for range 5 {
		page, err := a.ListThreadPastAgentRows(t.Context(), lead.ID, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 5 {
			t.Fatalf("total %d, want 5", page.Total)
		}
		rows = append(rows, page.Rows...)
		if page.Next == nil {
			break
		}
		after = *page.Next
	}
	if got, want := agentRowTokens(rows), []string{"b-shelved", "a-fresh", "ask", "told", "old"}; !slices.Equal(got, want) {
		t.Fatalf("past rows %q, want %q", got, want)
	}
	byToken := map[string]ThreadAgentRow{}
	for _, row := range rows {
		byToken[row.Token] = row
		if row.Depth != 0 || row.open() || row.SettledAt == 0 {
			t.Errorf("past row %+v is not a flat settled row", row)
		}
	}
	stored := func(id string) string {
		t.Helper()
		thread, err := a.store.GetThread(id)
		if err != nil {
			t.Fatal(err)
		}
		return thread.Placement
	}
	if row := byToken["b-shelved"]; row.Placement != stored(shelved.ID) || row.Placement == "" || !row.Archived || row.Answer != "shelved it" {
		t.Errorf("the archived spawn's row = %+v", row)
	}
	if row := byToken["a-fresh"]; row.Placement != stored(fresh.ID) || row.Archived {
		t.Errorf("the live spawn's row = %+v", row)
	}
	if row := byToken["ask"]; row.OpenThreadID != "" || row.AskedThreadID != asked.ID || row.Answer != "it says yes" || row.Title != asked.Title || row.Placement != "" || row.State != threadtools.RequestReplied {
		t.Errorf("the answered ask's row = %+v", row)
	}
	// A send to a spawned thread is not the spawn that placed it.
	if row := byToken["told"]; row.Placement != "" || row.Answer != "noted" {
		t.Errorf("the send's row = %+v", row)
	}
	if row := byToken["old"]; row.Answer != "" || row.ThreadID != old.ID {
		t.Errorf("the lineage spawn's row = %+v", row)
	}

	// The open list carries the same two facts for its spawn rows.
	listing, err := a.ListThreadAgentRows(t.Context(), lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Rows) != 1 || listing.Rows[0].Token != "open" || listing.Rows[0].Placement != stored(open.ID) || listing.Rows[0].Archived {
		t.Errorf("open rows = %+v", listing.Rows)
	}
}

// A spawn row's placement is what the sidebar can do with its thread: an
// assistant's helper, outside every project, has none, open or past, so
// its row offers no sidebar row the backend would refuse.
func TestAgentRowsGiveNoPlacementToAThreadThatTakesNone(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	lead := noticeTestThread(t, a, "helper-lead")
	testAssistant(t, a, lead.ID)
	helper := testThread("helper-worker")
	helper.ProjectID, helper.WorkspacePath, helper.HelperOfAgentID = "", t.TempDir(), lead.ID
	if err := a.store.CreateThread(helper); err != nil {
		t.Fatal(err)
	}
	done := testThread("helper-done")
	done.ProjectID, done.WorkspacePath, done.HelperOfAgentID = "", t.TempDir(), lead.ID
	if err := a.store.CreateThread(done); err != nil {
		t.Fatal(err)
	}
	if err := a.store.InsertThreadRequest(store.ThreadRequest{
		Token: "helper-open", CallerThreadID: lead.ID, Kind: store.ThreadRequestSpawn, TargetThreadID: helper.ID, State: store.ThreadRequestRunning, CreatedAt: 10,
	}); err != nil {
		t.Fatal(err)
	}
	seedSettledAgentRow(t, a, store.ThreadRequest{Token: "helper-past", CallerThreadID: lead.ID, Kind: store.ThreadRequestSpawn, TargetThreadID: done.ID, CreatedAt: 10}, 20, "done")
	for _, id := range []string{helper.ID, done.ID} {
		if _, err := a.SetThreadPlacement([]string{id}, store.PlacementStandalone); !errors.Is(err, store.ErrPlacementOutsideProjects) {
			t.Fatalf("Show in Sidebar on helper %s = %v, want it refused", id, err)
		}
	}
	listing, err := a.ListThreadAgentRows(t.Context(), lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Rows) != 1 || listing.Rows[0].Token != "helper-open" || listing.Rows[0].Placement != "" {
		t.Errorf("open helper rows = %+v, want no placement", listing.Rows)
	}
	page, err := a.ListThreadPastAgentRows(t.Context(), lead.ID, store.PastAgentRowsCursor{}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 || page.Rows[0].Token != "helper-past" || page.Rows[0].Placement != "" {
		t.Errorf("past helper rows = %+v, want no placement", page.Rows)
	}
}

// An ask's row presents the thread that was asked, its title and its
// provider, while its copy runs and after the copy is deleted; the row's
// kind says it was an ask. It opens the copy, its worker, only while the
// ask is open and the copy is there; the thread asked is named as that.
func TestAgentRowsPresentAnAskAsTheThreadAsked(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	lead := noticeTestThread(t, a, "ask-lead")
	asked := testThread("ask-asked")
	asked.Title, asked.Provider, asked.WorkspacePath = "Release notes", string(provider.Claude), t.TempDir()
	if err := a.store.CreateThread(asked); err != nil {
		t.Fatal(err)
	}
	copyRow := testThread("ask-copy")
	copyRow.Title, copyRow.Provider, copyRow.WorkspacePath = "Ask: Release notes", string(provider.Claude), t.TempDir()
	if err := a.store.CreateThread(copyRow); err != nil {
		t.Fatal(err)
	}
	if err := a.store.InsertThreadRequest(store.ThreadRequest{
		Token: "ask-open", CallerThreadID: lead.ID, Kind: store.ThreadRequestAsk, TargetThreadID: copyRow.ID,
		OriginThreadID: asked.ID, Notify: true, State: store.ThreadRequestRunning, CreatedAt: 10,
	}); err != nil {
		t.Fatal(err)
	}
	// An open ask whose copy is already gone, as between the copy's delete
	// and the ask's settlement.
	if err := a.store.InsertThreadRequest(store.ThreadRequest{
		Token: "ask-gone", CallerThreadID: lead.ID, Kind: store.ThreadRequestAsk, TargetThreadID: "gone-copy",
		OriginThreadID: asked.ID, Notify: true, State: store.ThreadRequestRunning, CreatedAt: 11,
	}); err != nil {
		t.Fatal(err)
	}
	seedSettledAgentRow(t, a, store.ThreadRequest{
		Token: "ask-past", CallerThreadID: lead.ID, Kind: store.ThreadRequestAsk, TargetThreadID: "deleted-copy",
		OriginThreadID: asked.ID, Notify: true, CreatedAt: 10,
	}, 20, "yes")
	listing, err := a.ListThreadAgentRows(t.Context(), lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	page, err := a.ListThreadPastAgentRows(t.Context(), lead.ID, store.PastAgentRowsCursor{}, 8)
	if err != nil {
		t.Fatal(err)
	}
	var rows []ThreadAgentRow
	rows = append(rows, listing.Rows...)
	rows = append(rows, page.Rows...)
	if got := agentRowTokens(rows); !slices.Contains(got, "ask-open") || !slices.Contains(got, "ask-gone") || !slices.Contains(got, "ask-past") {
		t.Fatalf("rows %q, want the open, the copyless and the past ask", got)
	}
	opens := map[string]string{"ask-open": copyRow.ID, "ask-gone": "", "ask-past": ""}
	for _, row := range rows {
		if row.Kind != store.ThreadRequestAsk {
			continue
		}
		if row.Title != asked.Title || row.Provider != asked.Provider || row.AskedThreadID != asked.ID {
			t.Errorf("ask row %s = title %q provider %q asked %q, want the thread asked (%q, %q, %q)",
				row.Token, row.Title, row.Provider, row.AskedThreadID, asked.Title, asked.Provider, asked.ID)
		}
		if want := opens[row.Token]; row.OpenThreadID != want {
			t.Errorf("ask row %s opens %q, want %q (its copy while it runs and is there, else nothing)", row.Token, row.OpenThreadID, want)
		}
	}
}

// approvalOn opens an approval prompt in threadID, as its provider would.
func approvalOn(t *testing.T, a *App, threadID, requestID string) {
	t.Helper()
	meta, err := json.Marshal(provider.ApprovalRequest{RequestID: requestID, ThreadID: threadID, ToolName: "Bash", Kind: "command"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventApprovalRequest, ThreadID: threadID, ItemID: requestID, Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

// A sidebar row's summary counts the open rows its Agents listing shows,
// nested ones included, and those blocked on a prompt the user must answer:
// a worker's prompt routed to its spawner is not the user's, one handed
// back is, and one answered is no longer counted. Only threads with open
// rows are listed.
func TestWorkerSummariesCountOpenRowsAndThoseWaitingOnTheUser(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	a.triage.SetPromptRouter(a.routeWorkerPrompt)
	lead := noticeTestThread(t, a, "sum-lead")
	worker := noticeTestThread(t, a, "sum-worker")
	grand := noticeTestThread(t, a, "sum-grand")
	done := noticeTestThread(t, a, "sum-done")
	idle := noticeTestThread(t, a, "sum-idle")
	for _, spawn := range []struct{ token, caller, target string }{
		{"lead-worker", lead.ID, worker.ID}, {"worker-grand", worker.ID, grand.ID},
	} {
		if err := a.store.InsertThreadRequest(store.ThreadRequest{
			Token: spawn.token, CallerThreadID: spawn.caller, TargetThreadID: spawn.target,
			Kind: store.ThreadRequestSpawn, State: store.ThreadRequestRunning,
		}); err != nil {
			t.Fatal(err)
		}
	}
	seedSettledAgentRow(t, a, store.ThreadRequest{Token: "done-idle", CallerThreadID: done.ID, Kind: store.ThreadRequestSpawn, TargetThreadID: idle.ID}, time.Now().UnixMilli(), "ok")
	summary := func(threadID string) ThreadWorkerSummary {
		t.Helper()
		got, err := a.GetThreadWorkerSummary(t.Context(), threadID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	want := func(threadID string, open, needYou int) {
		t.Helper()
		if got := summary(threadID); got != (ThreadWorkerSummary{ThreadID: threadID, Open: open, NeedYou: needYou}) {
			t.Errorf("summary of %s = %+v, want open %d need you %d", threadID, got, open, needYou)
		}
	}
	want(lead.ID, 2, 0)
	want(worker.ID, 1, 0)

	approvalOn(t, a, grand.ID, "grand-approval")
	if pending := a.triage.PendingInteractiveRequests(grand.ID).Approvals; len(pending) != 1 || pending[0].RoutedTo == nil {
		t.Fatalf("the grand worker's prompt = %+v, want it routed", pending)
	}
	want(lead.ID, 2, 0)
	want(worker.ID, 1, 0)

	if !a.triage.RejectPrompt(worker.ID, openPromptRef(t, a.triage, grand.ID, "grand-approval")) {
		t.Fatal("the prompt did not go back to the user")
	}
	want(lead.ID, 2, 1)
	want(worker.ID, 1, 1)

	all := allWorkerSummaries(t, a)
	if want := []ThreadWorkerSummary{{ThreadID: lead.ID, Open: 2, NeedYou: 1}, {ThreadID: worker.ID, Open: 1, NeedYou: 1}}; !slices.Equal(all, want) {
		t.Errorf("summaries = %+v, want %+v", all, want)
	}

	resolve, err := json.Marshal(map[string]any{"requestId": "grand-approval", "decision": "approved"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventApprovalResolved, ThreadID: grand.ID, ItemID: "grand-approval", Meta: resolve, Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	want(lead.ID, 2, 0)
	want(worker.ID, 1, 0)

	settled, err := a.store.SettleThreadRequest("worker-grand", store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadRequestFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal, SettledAt: time.Now().UnixMilli(),
	})
	if err != nil || !settled {
		t.Fatalf("settle: %v %v", settled, err)
	}
	want(worker.ID, 0, 0)
	want(lead.ID, 1, 0)
	all = allWorkerSummaries(t, a)
	if want := []ThreadWorkerSummary{{ThreadID: lead.ID, Open: 1}}; !slices.Equal(all, want) {
		t.Errorf("summaries after the grand worker settled = %+v, want %+v", all, want)
	}
}

// allWorkerSummaries reads every page of ListThreadWorkerSummaries, as the
// sidebar's bulk read does.
func allWorkerSummaries(t *testing.T, a *App) []ThreadWorkerSummary {
	t.Helper()
	out := []ThreadWorkerSummary{}
	for after, pages := "", 0; ; pages++ {
		if pages > 100 {
			t.Fatal("the summary pages never end")
		}
		page, err := a.ListThreadWorkerSummaries(t.Context(), after)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, page.Summaries...)
		if page.Next == "" {
			return out
		}
		if page.Next <= after {
			t.Fatalf("page after %q continues at %q", after, page.Next)
		}
		after = page.Next
	}
}

// More callers than a page holds are summarized across pages, each once,
// in id order, an owned worker with its own workers among them, and a
// count waiting on the user on the last page arrives with it.
func TestWorkerSummariesPageEveryCaller(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	a.triage.SetPromptRouter(a.routeWorkerPrompt)
	callers := 2*threadWorkerSummaryPage + 10
	spawn := func(token, caller, target string) {
		t.Helper()
		if err := a.store.InsertThreadRequest(store.ThreadRequest{
			Token: token, CallerThreadID: caller, TargetThreadID: target,
			Kind: store.ThreadRequestSpawn, State: store.ThreadRequestRunning,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var want []ThreadWorkerSummary
	for i := range callers {
		caller, worker := fmt.Sprintf("page-%03d", i), fmt.Sprintf("page-%03d-worker", i)
		noticeTestThread(t, a, caller)
		noticeTestThread(t, a, worker)
		spawn(caller+"-spawn", caller, worker)
		want = append(want, ThreadWorkerSummary{ThreadID: caller, Open: 1})
	}
	last := want[len(want)-1].ThreadID
	noticeTestThread(t, a, last+"-grand")
	spawn(last+"-worker-spawn", last+"-worker", last+"-grand")
	approvalOn(t, a, last+"-grand", "grand-approval")
	if !a.triage.RejectPrompt(last+"-worker", openPromptRef(t, a.triage, last+"-grand", "grand-approval")) {
		t.Fatal("the prompt did not go back to the user")
	}
	want[len(want)-1] = ThreadWorkerSummary{ThreadID: last, Open: 2, NeedYou: 1}
	want = append(want, ThreadWorkerSummary{ThreadID: last + "-worker", Open: 1, NeedYou: 1})
	if got := allWorkerSummaries(t, a); !slices.Equal(got, want) {
		t.Fatalf("summaries (%d) = %+v\nwant (%d) %+v", len(got), got, len(want), want)
	}
	first, err := a.ListThreadWorkerSummaries(t.Context(), "")
	if err != nil || len(first.Summaries) != threadWorkerSummaryPage || first.Next != want[threadWorkerSummaryPage-1].ThreadID {
		t.Fatalf("first page = %d summaries, next %q, %v", len(first.Summaries), first.Next, err)
	}
}

// A summary is counted from the rows' states and what they are blocked on
// alone: the counting view reads no answer head, no target names and no
// last messages, which the pane's listing of the same rows shows.
// A thread that is gone has no open worker: the summary of one deleted
// since a frame named it answers zero, not an error the client would read
// again.
func TestWorkerSummaryOfADeletedThreadIsEmpty(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	got, err := a.GetThreadWorkerSummary(t.Context(), "gone")
	if err != nil || got != (ThreadWorkerSummary{ThreadID: "gone"}) {
		t.Fatalf("summary of a missing thread = %+v, %v; want zero counts", got, err)
	}
}

func TestWorkerSummariesReadNoRowText(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	lead := noticeTestThread(t, a, "text-lead")
	worker := noticeTestThread(t, a, "text-worker")
	done := noticeTestThread(t, a, "text-done")
	if err := a.store.InsertThreadRequest(store.ThreadRequest{
		Token: "lead-worker", CallerThreadID: lead.ID, TargetThreadID: worker.ID,
		Kind: store.ThreadRequestSpawn, State: store.ThreadRequestRunning,
	}); err != nil {
		t.Fatal(err)
	}
	seedSettledAgentRow(t, a, store.ThreadRequest{Token: "lead-done", CallerThreadID: lead.ID, Kind: store.ThreadRequestSpawn, TargetThreadID: done.ID}, time.Now().UnixMilli(), "the answer")
	if err := a.store.InsertTurn(store.Turn{TurnID: "turn", ThreadID: worker.ID, TurnIndex: 0, StartedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.InsertItem(store.Item{ID: "said", ThreadID: worker.ID, Kind: "assistant_text", Role: "assistant",
		Status: "completed", Summary: "the last word", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	read := func(counts bool) []ThreadAgentRow {
		t.Helper()
		view := newThreadAgentView(a)
		view.counts = counts
		listing, _, err := a.threadAgentTreeDepth(t.Context(), view, lead.ID, threadAgentRowDepth, threadAgentPeersCached)
		if err != nil || len(listing.Rows) != 2 {
			t.Fatalf("listing (counts %v) = %+v, %v", counts, listing.Rows, err)
		}
		return listing.Rows
	}
	shown := read(false)
	if shown[0].Title == "" || shown[0].LastMessage == "" || shown[1].Answer == "" {
		t.Fatalf("the pane's rows carry no text: %+v", shown)
	}
	for _, row := range read(true) {
		if row.Title != "" || row.LastMessage != "" || row.Answer != "" || row.Provider != "" {
			t.Fatalf("a counted row carries text: %+v", row)
		}
	}
	if summary, err := a.GetThreadWorkerSummary(t.Context(), lead.ID); err != nil || summary != (ThreadWorkerSummary{ThreadID: lead.ID, Open: 1}) {
		t.Fatalf("summary %+v, %v", summary, err)
	}
}

// A change to a nested caller's request is framed for that caller and for
// every thread above it whose listing nests its rows, so each of their
// sidebar summaries is read again; a thread past the listing's depth is
// not told.
func TestAgentRowFramesReachTheThreadsAboveTheCaller(t *testing.T) {
	t.Parallel()
	a, rec := newAppForFlushQueueRPC(t)
	ids := []string{"frame-0", "frame-1", "frame-2", "frame-3", "frame-4"}
	for _, id := range ids {
		noticeTestThread(t, a, id)
	}
	for i := range len(ids) - 1 {
		if err := a.store.InsertThreadRequest(store.ThreadRequest{
			Token: ids[i] + "-spawn", CallerThreadID: ids[i], TargetThreadID: ids[i+1],
			Kind: store.ThreadRequestSpawn, State: store.ThreadRequestRunning,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.store.InsertThreadRequest(store.ThreadRequest{
		Token: "deep-send", CallerThreadID: ids[4], TargetThreadID: ids[0], Kind: store.ThreadRequestSend, Notify: true,
		State: store.ThreadRequestRunning,
	}); err != nil {
		t.Fatal(err)
	}
	framed := func(id string) []string {
		var out []string
		for _, call := range rec.snapshot() {
			if call.Channel != eventchan.ThreadAgentRowsChanged.String() {
				continue
			}
			if change, ok := call.Data.(ThreadAgentRowChange); ok && change.ID == id {
				out = append(out, change.ThreadID)
			}
		}
		slices.Sort(out)
		return out
	}
	a.noteThreadRequestChanged(store.ThreadRequestChange{Token: "deep-send", CallerThreadID: ids[4]})
	drained := func() bool {
		e := &a.threadAgentEmits
		e.mu.Lock()
		defer e.mu.Unlock()
		return !e.draining
	}
	want := []string{"frame-1", "frame-2", "frame-3", "frame-4"}
	waitUntil(t, 10*time.Second, func() bool { return len(framed("deep-send")) >= len(want) && drained() })
	if got := framed("deep-send"); !slices.Equal(got, want) {
		t.Fatalf("frames for the deep send = %q, want %q", got, want)
	}
}
