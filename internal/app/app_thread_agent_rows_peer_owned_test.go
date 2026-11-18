package app

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// A paired computer's spawn row shows what the computer that holds its
// thread reports: where the thread is placed there, whether it is
// archived, and what prompt it waits on, the person's, a thread's there, or
// its own spawner's here.

// askOn opens a prompt on threadID as its provider does: an approval, or a
// question when question is set.
func askOn(t *testing.T, app *App, threadID, requestID string, question bool) {
	t.Helper()
	event := provider.ProviderEvent{ThreadID: threadID, ItemID: requestID, Timestamp: time.Now()}
	var payload any = provider.ApprovalRequest{RequestID: requestID, ThreadID: threadID, ToolName: "Bash", Description: "make deploy", Kind: "command"}
	event.Kind = provider.EventApprovalRequest
	if question {
		payload = provider.UserInputRequest{
			RequestID: requestID, ThreadID: threadID, ToolName: "user_input",
			Questions: []provider.UserInputQuestion{{ID: "q", Header: "Scope", Question: "Which module?"}},
		}
		event.Kind = provider.EventUserInputRequest
	}
	meta, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	event.Meta = meta
	if err := app.triage.Handle(event); err != nil {
		t.Fatalf("open a prompt on %s: %v", threadID, err)
	}
}

// routeUnheld routes app's prompts as it does, a linked worker's to its
// spawner's computer, and closes the prompt queue that would hold them for
// that computer, so they stay routed and the rows read the route alone.
func routeUnheld(app *App) {
	app.workerPrompts.gate.Lock()
	app.workerPrompts.closed = true
	app.workerPrompts.gate.Unlock()
	app.triage.SetPromptRouter(app.routeWorkerPrompt)
}

func needYouOf(t *testing.T, app *App, threadID string) int {
	t.Helper()
	summary, err := app.GetThreadWorkerSummary(t.Context(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	return summary.NeedYou
}

// The spawn row of a thread on a paired computer carries that thread's
// placement and archive state there, and the prompt it waits on as that
// computer reads it: an approval the person owns, counted as needing the
// person here too, and a question routed back to the spawner here, named
// as this computer names it and counted as no one's but that spawner's.
func TestARemoteRowReportsWhereItsThreadIsAndWhatItWaitsOn(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
	second := pair.spawnThere(t, "more over there", map[string]any{"notify": true})
	routed := pair.receipt(t, second.Token).TargetThreadID
	routeUnheld(pair.dest)

	rowOf := func(rows ThreadAgentRows, token string) ThreadAgentRow {
		row, _ := agentRowByToken(rows.Rows, token)
		return row
	}
	pair.listRowsUntil(t, func(rows ThreadAgentRows) bool {
		return rowOf(rows, pair.spawn.Token).Placement == store.PlacementOwned && rowOf(rows, second.Token).Placement == store.PlacementOwned
	})
	// Shown in the sidebar there, the first worker's prompts are the
	// person's there.
	if _, err := pair.dest.store.SetThreadPlacement([]string{pair.child}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}

	askOn(t, pair.dest, pair.child, "approval-1", false)
	askOn(t, pair.dest, routed, "question-1", true)
	listing := pair.listRowsUntil(t, func(rows ThreadAgentRows) bool {
		pair.poll(t)
		return rowOf(rows, pair.spawn.Token).BlockedOn == "approval" && rowOf(rows, second.Token).BlockedOn == "question"
	})
	person, back := rowOf(listing, pair.spawn.Token), rowOf(listing, second.Token)
	if person.BlockedDetail != "Bash: make deploy" || person.BlockedRoutedTo != "" || person.BlockedRoutedHere {
		t.Fatalf("the row blocked on the person's approval = %+v", person)
	}
	if back.BlockedDetail != "Which module?" || back.BlockedRoutedTo != pair.caller.Title || !back.BlockedRoutedHere {
		t.Fatalf("the row whose question came back here = %+v; want it routed to %q here", back, pair.caller.Title)
	}
	if got := needYouOf(t, pair.source, pair.caller.ID); got != 1 {
		t.Fatalf("the summary counts %d rows needing the person, want the approval alone", got)
	}

	if _, _, err := pair.dest.store.ArchiveThread(pair.child); err != nil {
		t.Fatal(err)
	}
	pair.listRowsUntil(t, func(rows ThreadAgentRows) bool { return rowOf(rows, pair.spawn.Token).Archived })
}

// A paired computer's poll answer says when the prompt its target waits on
// went back to the asking device's spawner, and the spawn row here then
// names that spawner as this computer does and needs no one else; another
// device's poll of the same thread is told the prompt is a thread's there.
func TestAPolledPromptRoutedBackToTheAskerIsRoutedHere(t *testing.T) {
	t.Parallel()
	there, _ := newAppForFlushQueueRPC(t)
	there.triage = triage.NewRouter(there.store, there.emit)
	there.configureTriageQueueCallbacks()
	worker := noticeTestThread(t, there, "linked-worker")
	here := newRequestFixture(t)
	token, device := "linked-spawn", reachableDevice(t, there.store)
	if _, _, err := there.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: device, SourceComputerID: "source-computer", SourceComputerName: "Desk",
		SourceThreadID: here.caller.ID, SourceThreadTitle: "the lead as it was", Kind: store.ThreadRequestSpawn,
		SpawnLink: true, State: store.ThreadReceiptRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if named, _, err := there.store.SetThreadReceiptTarget(token, worker.ID, true); err != nil || !named {
		t.Fatalf("name the link: %v, %v", named, err)
	}
	routeUnheld(there)
	askOn(t, there, worker.ID, "approval-1", false)

	// Routed to the asker only for the link's own device, and only to the
	// link's source thread.
	for name, tc := range map[string]struct {
		route  provider.PromptRoute
		device string
		want   bool
	}{
		"the link's device": {provider.PromptRoute{ThreadID: here.caller.ID}, device, true},
		"another device":    {provider.PromptRoute{ThreadID: here.caller.ID}, "other-device", false},
		"another thread":    {provider.PromptRoute{ThreadID: "elsewhere"}, device, false},
		"no asking device":  {provider.PromptRoute{ThreadID: here.caller.ID}, "", false},
	} {
		route := tc.route
		if got, err := there.routedToAsker(worker.ID, &route, tc.device); err != nil || (got == here.caller.ID) != tc.want || (got != "" && got != here.caller.ID) {
			t.Errorf("%s: routed to the asker's thread %q, %v; want %v", name, got, err, tc.want)
		}
	}
	state, err := there.threadPeerRequestState(t.Context(), device, token)
	if err != nil || !state.Blocked || !state.BlockedRoutedToCaller || state.BlockedRoutedToThread != here.caller.ID || state.Placement != store.PlacementOwned {
		t.Fatalf("the owner's poll = %+v, %v; want blocked, routed back to it, owned", state, err)
	}
	// The listing says routed to the asker only to the thread it is routed
	// to; the same device's other thread is told it is a thread's there.
	for source, want := range map[string]bool{here.caller.ID: true, entityid.New(): false} {
		listing, err := there.answerThreadAgentRows(t.Context(), device, source, threadAgentPeerArgs{ThreadID: worker.ID, Depth: 1, StateOnly: true})
		if err != nil || listing.RoutedToAsker != want || listing.BlockedRoutedTo == "" {
			t.Fatalf("the listing for %s = routed to the asker %v (routed to %q), %v; want %v", source, listing.RoutedToAsker, listing.BlockedRoutedTo, err, want)
		}
	}
	wire, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var answer ThreadPeerRequest
	if err := json.Unmarshal(wire, &answer); err != nil {
		t.Fatal(err)
	}
	computer := "linked-computer"
	if err := here.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: here.caller.ID, Kind: store.ThreadRequestSpawn, State: store.ThreadRequestUnconfirmed,
		TargetComputerID: computer, TargetThreadID: worker.ID, Notify: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := here.app.applyThreadPeerRequest(token, computer, answer, threadPeerHearing{at: time.Now()}); err != nil {
		t.Fatal(err)
	}
	listing, err := here.app.ListThreadAgentRows(t.Context(), here.caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := agentRowByToken(listing.Rows, token)
	if !ok || row.State != threadtools.RequestBlocked || row.BlockedRoutedTo != here.caller.Title || !row.BlockedRoutedHere {
		t.Fatalf("the spawn row = %+v (listed %v), want blocked routed to %q here", row, ok, here.caller.Title)
	}
	if got := needYouOf(t, here.app, here.caller.ID); got != 0 {
		t.Fatalf("the summary counts %d rows needing the person, want none", got)
	}
}

// Across three computers, the first's caller spawned on the second, whose
// thread spawned on the third: the first computer's rows for both threads
// keep the placement and archive state the computer that holds each
// reported, through the second computer's nesting. A computer that does
// not report them leaves them empty; nothing here fills them in.
func TestNestedRowsAcrossThreeComputersKeepTheirPlacement(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		older bool
	}{
		{name: "reported"},
		{name: "an older second computer", older: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newAgentChain(t, 3)
			c.peers[1].unplaced.Store(tc.older)
			for i := 1; i < 3; i++ {
				installMockClaudeTurns(t, c.apps[i], [][]string{{mockClaudeInitLine}})
			}
			c.spawnAlong(t, 0, "on the second")
			c.spawnAlong(t, 1, "on the third")
			if _, _, err := c.apps[2].store.ArchiveThread(c.threads[1]); err != nil {
				t.Fatal(err)
			}
			var first, nested ThreadAgentRow
			waitUntil(t, 15*time.Second, func() bool {
				listing, err := c.apps[0].ListThreadAgentRows(t.Context(), c.caller.ID)
				if err != nil {
					return false
				}
				var ok bool
				first, _ = agentRowByToken(listing.Rows, c.spawns[0])
				nested, ok = agentRowByToken(listing.Rows, c.spawns[1])
				return ok && (tc.older || nested.Archived)
			})
			want := store.PlacementOwned
			if tc.older {
				want = ""
			}
			if first.Placement != want || first.Archived || nested.Placement != want || nested.Archived == tc.older {
				t.Fatalf("the rows read %s (archived %v) and nested %s (archived %v); want %q, the nested one archived: %v",
					first.Placement, first.Archived, nested.Placement, nested.Archived, want, !tc.older)
			}
		})
	}
}

// A prompt the third computer routes back to the second computer's thread
// that spawned there is routed here as the second computer reads it, and
// to that thread on the second computer as the first one reads it.
func TestAPromptRoutedBackOneComputerUpIsAThreadsThereFurtherUp(t *testing.T) {
	t.Parallel()
	c := newAgentChain(t, 3)
	for i := 1; i < 3; i++ {
		installMockClaudeTurns(t, c.apps[i], [][]string{{mockClaudeInitLine}})
	}
	c.spawnAlong(t, 0, "on the second")
	c.spawnAlong(t, 1, "on the third")
	lead, err := c.apps[1].store.GetThread(c.threads[0])
	if err != nil {
		t.Fatal(err)
	}
	routeUnheld(c.apps[2])
	askOn(t, c.apps[2], c.threads[1], "question-1", true)
	rowOn := func(app *App, threadID, token string) ThreadAgentRow {
		listing, err := app.ListThreadAgentRows(t.Context(), threadID)
		if err != nil {
			return ThreadAgentRow{}
		}
		row, _ := agentRowByToken(listing.Rows, token)
		return row
	}
	var second, first, onSecond ThreadAgentRow
	waitUntil(t, 15*time.Second, func() bool {
		second = rowOn(c.apps[1], c.threads[0], c.spawns[1])
		first = rowOn(c.apps[0], c.caller.ID, c.spawns[1])
		onSecond = rowOn(c.apps[0], c.caller.ID, c.spawns[0])
		return second.BlockedOn == "question" && first.BlockedOn == "question" && onSecond.Computer != ""
	})
	if !second.BlockedRoutedHere || second.BlockedRoutedTo != lead.Title {
		t.Fatalf("the second computer's row = %+v; want routed here, to %q", second, lead.Title)
	}
	// The first computer names the second as its own pairing does.
	if want := lead.Title + " on " + onSecond.Computer; first.BlockedRoutedHere || first.BlockedRoutedTo != want {
		t.Fatalf("the first computer's row = %+v; want routed to %q", first, want)
	}
	// The third computer's own row names the second as the link does.
	link, found, err := c.apps[2].store.GetThreadRequestReceipt(c.spawns[1])
	if err != nil || !found {
		t.Fatalf("the third computer's link: %v, %v", found, err)
	}
	if waiting := c.apps[2].threadWaitingOn(c.threads[1]); waiting.routedTo() != link.SourceThreadTitle+" on "+link.SourceComputerName {
		t.Fatalf("the third computer's row reads routed to %q, want the link's thread on %q", waiting.routedTo(), link.SourceComputerName)
	}
}
