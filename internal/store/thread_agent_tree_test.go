package store

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

func agentTreeRead(root string, depth int) ThreadAgentTreeRead {
	return ThreadAgentTreeRead{Root: root, SettledSince: 10_000, Depth: depth, Limit: 64, AnswerHead: 16}
}

// The tree lists each row then the rows its spawn made, siblings oldest
// first, with each row's depth; a local spawn at the bound whose thread has
// rows says so.
func TestListThreadAgentTreeNestsInTreeOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "child", "grand", "great", "peer"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "l-c", "lead", ThreadRequestSpawn, "child", false, 100)
	seedAgentRequest(t, s, "l-p", "lead", ThreadRequestSend, "peer", true, 101)
	seedAgentRequest(t, s, "c-g", "child", ThreadRequestSpawn, "grand", false, 110)
	seedAgentRequest(t, s, "c-p", "child", ThreadRequestAsk, "peer", true, 105)
	seedAgentRequest(t, s, "g-p", "grand", ThreadRequestSend, "peer", true, 120)
	seedAgentRequest(t, s, "g-l", "grand", ThreadRequestSpawn, "great", false, 121)

	read := func(depth int) ([]string, []int, []bool) {
		t.Helper()
		rows, more, err := s.ListThreadAgentTree(agentTreeRead("lead", depth))
		if err != nil || more {
			t.Fatalf("ListThreadAgentTree(depth %d): more=%v err=%v", depth, more, err)
		}
		var tokens []string
		var depths []int
		var hidden []bool
		for _, row := range rows {
			tokens, depths, hidden = append(tokens, row.Token), append(depths, row.Depth), append(hidden, row.NestedHidden)
		}
		return tokens, depths, hidden
	}
	tokens, depths, hidden := read(4)
	if want := []string{"l-c", "c-p", "c-g", "g-p", "g-l", "l-p"}; !slices.Equal(tokens, want) {
		t.Fatalf("tree order %v; want %v", tokens, want)
	}
	if want := []int{0, 1, 1, 2, 2, 0}; !slices.Equal(depths, want) {
		t.Fatalf("depths %v; want %v", depths, want)
	}
	if slices.Contains(hidden, true) {
		t.Fatalf("a row inside the bound is marked hidden: %v", hidden)
	}
	// At two levels the child's spawn nests nothing, and says its thread
	// has rows.
	tokens, _, hidden = read(2)
	if want := []string{"l-c", "c-p", "c-g", "l-p"}; !slices.Equal(tokens, want) {
		t.Fatalf("two-level tree %v; want %v", tokens, want)
	}
	if want := []bool{false, false, true, false}; !slices.Equal(hidden, want) {
		t.Fatalf("two-level hidden marks %v; want %v", hidden, want)
	}
}

// The retention floor does not take a spawn out of the tree while work is
// open under it: a local spawn settled long ago with an open request below
// its target, and a remote spawn whose computer reported open work, list the
// same after a sweep, by listing and by name. Once that work settles, the
// next sweep keeps both as their lineage: the remote one, its thread there
// continued, lists again under its caller and in the marks above it, and
// goes once that computer refuses the thread as gone.
func TestRetentionKeepsSpawnsWithOpenWorkUnderThem(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "child", "grand"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "l-c", "lead", ThreadRequestSpawn, "child", false, 100)
	settleAgentRequest(t, s, "l-c", 200)
	seedAgentRequest(t, s, "c-g", "child", ThreadRequestSpawn, "grand", false, 150)
	if err := s.InsertThreadRequest(ThreadRequest{Token: "remote", CallerThreadID: "lead", Kind: ThreadRequestSpawn,
		TargetComputerID: "desk", TargetThreadID: "far", State: ThreadRequestRunning, CreatedAt: 101, UpdatedAt: 101}); err != nil {
		t.Fatal(err)
	}
	settleAgentRequest(t, s, "remote", 200)
	if ok, err := s.SetThreadRequestNestedOpen("remote", true); err != nil || !ok {
		t.Fatalf("nested open: %v %v", ok, err)
	}
	listed := func(tokens ...string) []string {
		t.Helper()
		read := agentTreeRead("lead", 4)
		read.Tokens = tokens
		rows, _, err := s.ListThreadAgentTree(read)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, row := range rows {
			out = append(out, row.Token)
		}
		return out
	}
	want := []string{"l-c", "c-g", "remote"}
	if got := listed(); !slices.Equal(got, want) {
		t.Fatalf("before the sweep the tree = %v, want %v", got, want)
	}
	if _, err := s.DeleteThreadRequestsBefore(5_000); err != nil {
		t.Fatal(err)
	}
	if got := listed(); !slices.Equal(got, want) {
		t.Fatalf("after the sweep the tree = %v, want %v", got, want)
	}
	if got := listed("c-g", "remote"); !slices.Equal(got, []string{"c-g", "remote"}) {
		t.Fatalf("after the sweep the named rows = %v", got)
	}
	if _, found, err := s.GetThreadRequest("l-c"); err != nil || !found {
		t.Fatalf("the kept spawn's token after the sweep = %v %v", found, err)
	}

	settleAgentRequest(t, s, "c-g", 6_000)
	if ok, err := s.SetThreadRequestNestedOpen("remote", false); err != nil || !ok {
		t.Fatalf("nested closed: %v %v", ok, err)
	}
	if _, err := s.DeleteThreadRequestsBefore(8_000); err != nil {
		t.Fatal(err)
	}
	if got := listed(); len(got) != 0 {
		t.Fatalf("after the work settled and the next sweep the tree = %v, want none", got)
	}
	if row := rawThreadRequest(t, s, "l-c"); !row.LineageOnly {
		t.Fatal("the local spawn kept no lineage once the work under it settled")
	}
	remote := rawThreadRequest(t, s, "remote")
	if !remote.LineageOnly || len(remote.Answer) != 0 || remote.Polling || remote.TargetThreadID != "far" {
		t.Fatalf("the remote spawn after the work settled = %+v; want its lineage, naming its thread there", remote)
	}
	if _, found, err := s.GetThreadRequest("remote"); err != nil || found {
		t.Fatalf("the remote lineage's token = %v %v; want unknown", found, err)
	}
	if resting, err := s.ListRestingRemoteSpawns(0, "", 8); err != nil || !slices.Equal(agentRequestTokens(resting), []string{"remote"}) {
		t.Fatalf("the rest pass's spawns = %v, %v; want the remote lineage", agentRequestTokens(resting), err)
	}

	// Its thread there is continued: the lineage lists again, and counts
	// in the marks of the spawns above its caller.
	seedAgentRequest(t, s, "g-r", "grand", ThreadRequestSpawn, "", false, 7_000)
	if _, err := s.db.Exec(`UPDATE thread_requests SET target_computer_id = 'desk', target_thread_id = 'far-2' WHERE token = 'g-r'`); err != nil {
		t.Fatal(err)
	}
	settleAgentRequest(t, s, "g-r", 7_100)
	if _, err := s.DeleteThreadRequestsBefore(9_000); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetThreadRequestNestedOpen("remote", true); err != nil || !ok {
		t.Fatalf("the lineage's open work: %v %v", ok, err)
	}
	if ok, err := s.SetThreadRequestNestedOpen("g-r", true); err != nil || !ok {
		t.Fatalf("the deep lineage's open work: %v %v", ok, err)
	}
	if got := listed(); !slices.Equal(got, []string{"l-c", "c-g", "g-r", "remote"}) {
		t.Fatalf("with the remote threads continued the tree = %v", got)
	}
	if got := listed("remote"); !slices.Equal(got, []string{"remote"}) {
		t.Fatalf("the remote lineage by name = %v", got)
	}
	if marks := agentTreeMarks(t, s.db)["l-c"]; marks[1] != 1 {
		t.Fatalf("the lead's spawn marks %v; want the remote lineage two spawns down counted", marks)
	}
	assertAgentTreeMarks(t, s.db, "a remote lineage's open work")

	// That computer refuses one thread as gone, and is forgotten with the
	// other: both lineages go.
	if gone, err := s.DeleteRemoteSpawnLineage("remote"); err != nil || !gone {
		t.Fatalf("delete the refused lineage = %v, %v", gone, err)
	}
	if gone, err := s.DeleteRemoteSpawnLineage("l-c"); err != nil || gone {
		t.Fatalf("delete a local lineage as remote = %v, %v; want it kept", gone, err)
	}
	callers, err := s.ForgetRemoteSpawnsOnComputer("desk")
	if err != nil || !slices.Equal(callers, []string{"grand"}) {
		t.Fatalf("forget the computer = %v, %v; want the deep lineage's caller", callers, err)
	}
	var left int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM thread_requests WHERE target_computer_id = 'desk'`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("remote rows left = %d, %v", left, err)
	}
	if marks := agentTreeMarks(t, s.db)["l-c"]; marks != [agentOpenDepth]int64{} {
		t.Fatalf("the lead's spawn marks %v once the lineages went; want none", marks)
	}
	assertAgentTreeMarks(t, s.db, "the remote lineages gone")
}

// A spawn the retention floor reduced to its lineage still links its
// thread to its caller: a worker reused after the sweep nests the work it
// opens under its owner's tree, by listing, by name, in the frames, the
// summary candidates and the marks, as before the sweep. The expired
// spawn's own token stays unknown and its answer stays gone.
func TestAnExpiredSpawnStillNestsItsThreadsNewWork(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "child", "grand", "peer"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "l-c", "lead", ThreadRequestSpawn, "child", false, 100)
	settleAgentRequest(t, s, "l-c", 200)
	if _, err := s.DeleteThreadRequestsBefore(5_000); err != nil {
		t.Fatal(err)
	}
	if row := rawThreadRequest(t, s, "l-c"); !row.LineageOnly || row.Answer != nil {
		t.Fatalf("the sweep kept %+v; want a lineage row with no answer", row)
	}
	// The reused worker spawns again and sends with its wake armed.
	seedAgentRequest(t, s, "c-g", "child", ThreadRequestSpawn, "grand", false, 6_000)
	seedAgentRequest(t, s, "c-p", "child", ThreadRequestSend, "peer", true, 6_001)
	assertAgentTreeMarks(t, s.db, "after the reuse")

	rows, _, err := s.ListThreadAgentTree(agentTreeRead("lead", 4))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := agentTreeEntries(rows), []string{"l-c@0", "c-g@1", "c-p@1"}; !slices.Equal(got, want) {
		t.Fatalf("the owner's tree %v; want %v", got, want)
	}
	if len(rows[0].AnswerHead) != 0 {
		t.Fatalf("the expired spawn's row carries answer %q", rows[0].AnswerHead)
	}
	named := agentTreeRead("lead", 4)
	named.Tokens = []string{"c-p", "l-c"}
	if rows, _, err = s.ListThreadAgentTree(named); err != nil {
		t.Fatal(err)
	}
	if got, want := agentTreeEntries(rows), []string{"l-c@0", "c-p@1"}; !slices.Equal(got, want) {
		t.Fatalf("the named rows %v; want %v", got, want)
	}
	if ids, _, err := s.ThreadsWithOpenAgentRows(nil, nil, "", 10); err != nil || !slices.Contains(ids, "lead") {
		t.Fatalf("summary candidates %v, %v; want lead among them", ids, err)
	}
	frames, _, err := s.ThreadAgentRowFrames([]string{"c-p"}, nil, nil, "local", 4)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(frames, ThreadAgentRowFrame{Token: "c-p", ThreadID: "lead"}) {
		t.Fatalf("frames %+v; want one for lead", frames)
	}
	if _, found, err := s.GetThreadRequest("l-c"); err != nil || found {
		t.Fatalf("the expired spawn's token reads found=%v, %v; want unknown", found, err)
	}
	// Once the reused work settles, the expired spawn leaves the tree again.
	settleAgentRequest(t, s, "c-g", 7_000)
	settleAgentRequest(t, s, "c-p", 7_000)
	assertAgentTreeMarks(t, s.db, "after the work settled")
	if rows, _, err = s.ListThreadAgentTree(ThreadAgentTreeRead{Root: "lead", SettledSince: 9_000, Depth: 4, Limit: 64, AnswerHead: 16}); err != nil || len(rows) != 0 {
		t.Fatalf("the settled tree %v, %v; want none", agentTreeEntries(rows), err)
	}
}

// A quiet send or ask a call is parked on is open work under the spawns
// above its caller, as an armed one is: a worker's listing shows it while
// its owner waits, so the owner's listing nests it under the worker's
// settled spawn, by listing and by name, says a spawn at the depth bound
// hides it, and names the owner a summary candidate. Once the call stops
// waiting the row and the spawn above it leave.
func TestAParkedQuietRequestKeepsTheSpawnsAboveIt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "child", "grand", "peer"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "l-c", "lead", ThreadRequestSpawn, "child", false, 100)
	settleAgentRequest(t, s, "l-c", 200)
	seedAgentRequest(t, s, "c-g", "child", ThreadRequestSpawn, "grand", false, 110)
	settleAgentRequest(t, s, "c-g", 210)
	seedAgentRequest(t, s, "g-p", "grand", ThreadRequestAsk, "peer", false, 120)
	read := func(waiting []string, depth int, tokens []string) []string {
		t.Helper()
		r := agentTreeRead("lead", depth)
		r.Waiting, r.Tokens = waiting, tokens
		rows, _, err := s.ListThreadAgentTree(r)
		if err != nil {
			t.Fatal(err)
		}
		return agentTreeEntries(rows)
	}
	parked := []string{"g-p"}
	// The worker's own listing shows the parked row.
	if rows, _, err := s.ListThreadAgentTree(ThreadAgentTreeRead{Root: "grand", Waiting: parked, SettledSince: 10_000, Depth: 4, Limit: 8, AnswerHead: 1}); err != nil || len(rows) != 1 {
		t.Fatalf("the worker's own listing %v, %v; want its parked ask", agentTreeEntries(rows), err)
	}
	if got, want := read(parked, 4, nil), []string{"l-c@0", "c-g@1", "g-p@2"}; !slices.Equal(got, want) {
		t.Fatalf("the owner's tree %v; want %v", got, want)
	}
	if got, want := read(parked, 4, []string{"g-p", "c-g"}), []string{"c-g@1", "g-p@2"}; !slices.Equal(got, want) {
		t.Fatalf("the named rows %v; want %v", got, want)
	}
	if got, want := read(parked, 2, nil), []string{"l-c@0", "c-g@1+hidden"}; !slices.Equal(got, want) {
		t.Fatalf("the two-level tree %v; want %v", got, want)
	}
	if ids, _, err := s.ThreadsWithOpenAgentRows(parked, nil, "", 10); err != nil || !slices.Equal(ids, []string{"child", "grand", "lead"}) {
		t.Fatalf("summary candidates %v, %v; want every thread up the chain", ids, err)
	}
	if got := read(nil, 4, nil); len(got) != 0 {
		t.Fatalf("with no call parked the tree %v; want none", got)
	}
	if got := read(nil, 4, []string{"g-p", "c-g"}); len(got) != 0 {
		t.Fatalf("with no call parked the named rows %v; want none", got)
	}
	if ids, _, err := s.ThreadsWithOpenAgentRows(nil, nil, "", 10); err != nil || len(ids) != 0 {
		t.Fatalf("with no call parked the candidates %v, %v; want none", ids, err)
	}
	// An armed wake below is counted once: the parked spawn is the marks'.
	if ok, err := s.SetThreadRequestNotify("g-p", true); err != nil || !ok {
		t.Fatalf("arm: %v %v", ok, err)
	}
	if got, want := read(parked, 4, nil), []string{"l-c@0", "c-g@1", "g-p@2"}; !slices.Equal(got, want) {
		t.Fatalf("armed and parked, the owner's tree %v; want %v", got, want)
	}
}

// A live thread keeps the spawn that made it, its link, and the spawns
// above it, settled long before the window, in every read that lists
// rows: by listing and by name, in the summary candidates and in a Stop
// All's rows; Past leaves the live spawns it is given out. A live thread
// only sent to, or never spawned, keeps nothing; a superseded spawn of a
// live thread is not its link.
func TestALiveThreadKeepsItsSpawnAndTheSpawnsAboveIt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "child", "grand", "peer", "alone"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "l-c", "lead", ThreadRequestSpawn, "child", false, 100)
	settleAgentRequest(t, s, "l-c", 200)
	seedAgentRequest(t, s, "c-g", "child", ThreadRequestSpawn, "grand", false, 110)
	settleAgentRequest(t, s, "c-g", 210)
	seedAgentRequest(t, s, "l-p", "lead", ThreadRequestSend, "peer", true, 120)
	settleAgentRequest(t, s, "l-p", 220)
	read := func(live []string, depth int, tokens []string) []string {
		t.Helper()
		r := agentTreeRead("lead", depth)
		r.Live, r.Tokens = live, tokens
		rows, _, err := s.ListThreadAgentTree(r)
		if err != nil {
			t.Fatal(err)
		}
		return agentTreeEntries(rows)
	}
	stops := func(root string, live []string) []string {
		t.Helper()
		rows, err := s.ListThreadAgentStopRows(ThreadAgentStopRead{Root: root, Live: live, SettledSince: 10_000, Page: 8})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, row := range rows {
			out = append(out, row.Token)
		}
		return out
	}
	past := func(root string, live []string) []string {
		t.Helper()
		page, err := s.ListThreadPastAgentRequests(root, PastAgentRowsCursor{}, 8, 0, nil, live)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, row := range page.Requests {
			out = append(out, row.Token)
		}
		if page.Total != len(out) {
			t.Fatalf("past of %s total %d, rows %v", root, page.Total, out)
		}
		return out
	}
	if got := read(nil, 4, nil); len(got) != 0 {
		t.Fatalf("at rest the tree %v; want none", got)
	}
	// The worker child is live: its spawn is listed, and nothing below it.
	if got, want := read([]string{"child"}, 4, nil), []string{"l-c@0"}; !slices.Equal(got, want) {
		t.Fatalf("child live, the tree %v; want %v", got, want)
	}
	if got, want := stops("lead", []string{"child"}), []string{"l-c"}; !slices.Equal(got, want) {
		t.Fatalf("child live, Stop All's rows %v; want %v", got, want)
	}
	// The nested worker grand is live: both spawns above it are listed.
	live := []string{"grand"}
	if got, want := read(live, 4, nil), []string{"l-c@0", "c-g@1"}; !slices.Equal(got, want) {
		t.Fatalf("grand live, the tree %v; want %v", got, want)
	}
	if got, want := read(live, 4, []string{"c-g"}), []string{"c-g@1"}; !slices.Equal(got, want) {
		t.Fatalf("grand live, the named rows %v; want %v", got, want)
	}
	if got, want := read(live, 1, nil), []string{"l-c@0+hidden"}; !slices.Equal(got, want) {
		t.Fatalf("grand live, the one-level tree %v; want %v", got, want)
	}
	if ids, _, err := s.ThreadsWithOpenAgentRows(nil, live, "", 10); err != nil || !slices.Equal(ids, []string{"child", "lead"}) {
		t.Fatalf("grand live, the summary candidates %v, %v; want every spawner up the chain", ids, err)
	}
	if got, want := stops("child", live), []string{"c-g"}; !slices.Equal(got, want) {
		t.Fatalf("grand live, child's Stop All rows %v; want %v", got, want)
	}
	// Past leaves out a spawn whose thread is live or has a live worker
	// below it.
	for _, live := range [][]string{{"child"}, {"grand"}} {
		if got, want := past("lead", live), []string{"l-p"}; !slices.Equal(got, want) {
			t.Fatalf("past with %v live %v; want %v", live, got, want)
		}
	}
	if got, want := past("lead", nil), []string{"l-p", "l-c"}; !slices.Equal(got, want) {
		t.Fatalf("past at rest %v; want %v", got, want)
	}
	// A live thread the lead only sent to, or one nothing spawned, keeps
	// nothing.
	for _, other := range [][]string{{"peer"}, {"alone"}} {
		if got := read(other, 4, nil); len(got) != 0 {
			t.Fatalf("%v live, the tree %v; want none", other, got)
		}
		if ids, _, err := s.ThreadsWithOpenAgentRows(nil, other, "", 10); err != nil || len(ids) != 0 {
			t.Fatalf("%v live, the candidates %v, %v; want none", other, ids, err)
		}
	}
	// A superseded spawn of child, as migration v154 leaves an older one,
	// is lineage, not its link.
	mustExec(t, s.db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, state, created_at, updated_at, settled_at, superseded, lineage_only)
		VALUES ('p-c', 'peer', 'spawn', 'child', 'finished', 90, 190, 190, 1, 1)`)
	if ids, _, err := s.ThreadsWithOpenAgentRows(nil, []string{"child"}, "", 10); err != nil || !slices.Equal(ids, []string{"lead"}) {
		t.Fatalf("child live, the candidates %v, %v; want its link's caller alone", ids, err)
	}
	if link, found, err := s.SpawnLinkOf("child"); err != nil || !found || link != "l-c" {
		t.Fatalf("child's link %q, %v, %v; want l-c", link, found, err)
	}
	if link, found, err := s.SpawnLinkOf("alone"); err != nil || found || link != "" {
		t.Fatalf("alone's link %q, %v, %v; want none", link, found, err)
	}
}

// A settled spawn's receipt is what lets the computer that made the spawn
// list and stop the work under its thread, so the retention floor keeps it
// while that work is open: an open request its thread made, open work below
// a spawn its thread made, or a remote spawn its thread made that reported
// open work. The first sweep after the work settles deletes it. A send's
// receipt authorizes nothing and goes at the floor.
func TestRetentionKeepsSpawnReceiptsWithOpenWorkUnderThem(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"own", "nests", "nests-child", "far", "sent"} {
		mustCreateThread(t, s, id)
	}
	receipt := func(token, kind, target string) {
		t.Helper()
		if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{Token: token, OwnerDeviceID: "desk", Kind: kind, TargetThreadID: target, CreatedAt: 10}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SettleLocalThreadRequest(token, ThreadReceiptOpenStates(), ThreadRequestSettlement{State: ThreadReceiptFinished, SettledAt: 20}, ThreadRequestFinished); err != nil {
			t.Fatal(err)
		}
	}
	receipt("in-own", ThreadRequestSpawn, "own")
	receipt("in-nests", ThreadRequestSpawn, "nests")
	receipt("in-far", ThreadRequestSpawn, "far")
	receipt("in-sent", ThreadRequestSend, "sent")
	// own's open send; an open ask two spawns below nests; far's settled
	// remote spawn whose computer reports open work; sent's open send.
	seedAgentRequest(t, s, "own-open", "own", ThreadRequestSend, "x", true, 100)
	seedAgentRequest(t, s, "nests-spawn", "nests", ThreadRequestSpawn, "nests-child", false, 100)
	settleAgentRequest(t, s, "nests-spawn", 200)
	seedAgentRequest(t, s, "deep-open", "nests-child", ThreadRequestAsk, "x", true, 110)
	if err := s.InsertThreadRequest(ThreadRequest{Token: "far-remote", CallerThreadID: "far", Kind: ThreadRequestSpawn,
		TargetComputerID: "other", TargetThreadID: "there", State: ThreadRequestRunning, CreatedAt: 100, UpdatedAt: 100}); err != nil {
		t.Fatal(err)
	}
	settleAgentRequest(t, s, "far-remote", 200)
	if ok, err := s.SetThreadRequestNestedOpen("far-remote", true); err != nil || !ok {
		t.Fatalf("nested open: %v %v", ok, err)
	}
	seedAgentRequest(t, s, "sent-open", "sent", ThreadRequestSend, "x", true, 100)
	receipts := func() []string {
		t.Helper()
		var out []string
		for _, token := range []string{"in-own", "in-nests", "in-far", "in-sent"} {
			if _, found, err := s.GetThreadRequestReceipt(token); err != nil {
				t.Fatal(err)
			} else if found {
				out = append(out, token)
			}
		}
		return out
	}
	if _, err := s.DeleteThreadRequestsBefore(5_000); err != nil {
		t.Fatal(err)
	}
	if got, want := receipts(), []string{"in-own", "in-nests", "in-far"}; !slices.Equal(got, want) {
		t.Fatalf("after the sweep the receipts = %v, want %v", got, want)
	}

	settleAgentRequest(t, s, "own-open", 6_000)
	settleAgentRequest(t, s, "deep-open", 6_000)
	if ok, err := s.SetThreadRequestNestedOpen("far-remote", false); err != nil || !ok {
		t.Fatalf("nested closed: %v %v", ok, err)
	}
	if _, err := s.DeleteThreadRequestsBefore(5_000); err != nil {
		t.Fatal(err)
	}
	if got := receipts(); len(got) != 0 {
		t.Fatalf("after the work settled and the next sweep the receipts = %v, want none", got)
	}
}

// A row carries the head of its newer answer and whether a late reply came,
// and a read by token reads that one row.
func TestListThreadAgentTreeReadsTheAnswerHead(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	seedAgentRequest(t, s, "first", "lead", ThreadRequestSend, "t", true, 100)
	seedAgentRequest(t, s, "late", "lead", ThreadRequestSend, "t", true, 101)
	for _, token := range []string{"first", "late"} {
		if ok, err := s.SettleThreadRequest(token, ThreadRequestOpenStates(), ThreadRequestSettlement{
			State: ThreadRequestFinished, Answer: []byte(strings.Repeat("a", 64)), AnswerKind: ThreadAnswerFinal, SettledAt: 20_000,
		}); err != nil || !ok {
			t.Fatalf("settle %s: %v %v", token, ok, err)
		}
	}
	if ok, err := s.StoreThreadRequestLateReply("late", []byte(strings.Repeat("b", 64)), 21_000, nil, false); err != nil || !ok {
		t.Fatalf("late reply: %v %v", ok, err)
	}
	rows, _, err := s.ListThreadAgentTree(agentTreeRead("lead", 1))
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %d, %v", len(rows), err)
	}
	if got := rows[0]; string(got.AnswerHead) != strings.Repeat("a", 16) || got.HasLateReply || got.Answer != nil || got.LateReply != nil {
		t.Fatalf("first row %+v", got)
	}
	if got := rows[1]; string(got.AnswerHead) != strings.Repeat("b", 16) || !got.HasLateReply || got.LateReply != nil {
		t.Fatalf("late row %+v", got)
	}
	one := agentTreeRead("lead", 1)
	one.Tokens = []string{"late"}
	if rows, _, err := s.ListThreadAgentTree(one); err != nil || len(rows) != 1 || rows[0].Token != "late" {
		t.Fatalf("the read of one token = %+v, %v", rows, err)
	}
}

// A confirmed queued answer names the row its echo named, while the
// caller's rows hold it: a merge's survivor replaces it, an echo naming no
// row leaves it, a give-back clears it, and a row gone names nothing. The
// tool rows' read says the same, in the order asked, for the caller alone.
func TestConfirmedWakesNameTheirRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	seedSettledThreadRequest(t, s, "tok", "lead", ThreadRequestSend, 100, "the answer")
	seedSettledThreadRequest(t, s, "other", "lead", ThreadRequestSend, 100, "the answer")
	for i, id := range []string{"row-a", "row-b"} {
		if _, err := s.UpsertItem(Item{ID: id, ThreadID: "lead", TurnIndex: i + 1, Kind: "user_text", Role: "user",
			Status: "completed", Summary: "x", Meta: "{}", CreatedAt: 1, UpdatedAt: 1}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.MarkThreadRequestDelivered("tok", ThreadWakeQueued, 200, false); err != nil || !ok {
		t.Fatalf("mark: %v %v", ok, err)
	}
	wake := func() string {
		t.Helper()
		got, err := s.ThreadRequestDeliveries("lead", []string{"other", "tok", "missing"})
		if err != nil || len(got) != 2 || got[0].Token != "other" || got[1].Token != "tok" {
			t.Fatalf("deliveries %+v, %v", got, err)
		}
		rows, _, err := s.ListThreadAgentTree(ThreadAgentTreeRead{Root: "lead", SettledSince: 0, Depth: 1, Limit: 8, AnswerHead: 4})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Token == "tok" && row.WakeItemID != got[1].WakeItemID {
				t.Fatalf("the listing names %q, the states %q", row.WakeItemID, got[1].WakeItemID)
			}
		}
		return got[1].WakeItemID
	}
	if id := wake(); id != "" {
		t.Fatalf("an unconfirmed wake names %q", id)
	}
	if ok, err := s.ConfirmThreadWake("tok", false, "row-a", 300); err != nil || !ok {
		t.Fatalf("confirm: %v %v", ok, err)
	}
	if id := wake(); id != "row-a" {
		t.Fatalf("confirmed wake names %q; want row-a", id)
	}
	if ok, err := s.ConfirmThreadWake("tok", false, "", 310); err != nil || ok {
		t.Fatalf("an echo naming no row changed it: %v %v", ok, err)
	}
	if ok, err := s.ConfirmThreadWake("tok", false, "row-b", 320); err != nil || !ok {
		t.Fatalf("re-point: %v %v", ok, err)
	}
	if row := mustGetThreadRequest(t, s, "tok"); row.ConfirmedAt != 300 {
		t.Fatalf("the re-point moved the confirmation to %d", row.ConfirmedAt)
	}
	if id := wake(); id != "row-b" {
		t.Fatalf("re-pointed wake names %q; want row-b", id)
	}
	// Moved to another caller, as New Thread, Same Assistant moves it, the
	// request names no row of the thread it left.
	mustCreateThread(t, s, "successor")
	if _, err := s.db.Exec(`UPDATE thread_requests SET caller_thread_id = 'successor' WHERE token = 'tok'`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ThreadRequestDeliveries("successor", []string{"tok"}); err != nil || len(got) != 1 || got[0].WakeItemID != "" {
		t.Fatalf("the moved request reads %+v, %v; want no wake row", got, err)
	}
	if _, err := s.db.Exec(`UPDATE thread_requests SET caller_thread_id = 'lead' WHERE token = 'tok'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM items WHERE thread_id = 'lead' AND id = 'row-b'`); err != nil {
		t.Fatal(err)
	}
	if id := wake(); id != "" {
		t.Fatalf("a wake whose row is gone names %q", id)
	}
	// An archive that finds the queue row of a confirmed wake drops the
	// row and keeps the wake, and the row it names.
	if err := s.InsertFlushQueueItem(FlushQueueItem{ID: "queue:tok", ThreadID: "lead", SendID: "thread-wake:tok", Message: "the answer", EnqueuedAt: 330}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WithdrawThreadWake("tok", false, "queue:tok", WithdrawQueued); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	var kept int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM thread_requests WHERE token = 'tok' AND wake_item_id = 'row-b' AND confirmed_at = 300`).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("the give-back took back a confirmed wake: %d, %v", kept, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM flush_queue_items WHERE id = 'queue:tok'`).Scan(&kept); err != nil || kept != 0 {
		t.Fatalf("the queue row stayed: %d, %v", kept, err)
	}
	if got, err := s.ThreadRequestDeliveries("other-thread", []string{"tok"}); err != nil || len(got) != 0 {
		t.Fatalf("another thread read %+v, %v", got, err)
	}
}

// A change frame names the caller of each changed request, of each open
// request this computer made of a changed target and of the target's spawn
// link, and each changed caller; every link above those up to the root is
// framed for its caller, as its thread's rest follows the work below it;
// and each frame goes again to every thread up the local spawns above its
// caller within the depth, and no further.
func TestThreadAgentRowFramesNameCallersAndTheThreadsAboveThem(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "peer", "z0", "z1", "z2", "out"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "direct", "lead", ThreadRequestSend, "peer", true, 100)
	seedAgentRequest(t, s, "running", "peer", ThreadRequestSend, "lead", true, 101)
	seedThreadRequest(t, s, "remind", "lead", ThreadRequestRemind, 102)
	for _, spawn := range [][2]string{{"out", "z0"}, {"z0", "z1"}, {"z1", "z2"}, {"z2", "lead"}} {
		seedAgentRequest(t, s, spawn[0]+"-"+spawn[1], spawn[0], ThreadRequestSpawn, spawn[1], false, 90)
	}
	for _, receipt := range []ThreadRequestReceipt{
		{Token: "running", OwnerDeviceID: "local", SourceThreadID: "peer", Kind: ThreadRequestSend, TargetThreadID: "lead", State: ThreadReceiptRunning},
		{Token: "foreign", OwnerDeviceID: "device-2", SourceThreadID: "x", Kind: ThreadRequestSend, TargetThreadID: "lead", State: ThreadReceiptRunning},
	} {
		if _, _, err := s.AcceptThreadRequestReceipt(receipt); err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := s.ThreadAgentRowFrames([]string{"direct", "remind", "missing"}, []string{"lead"}, []string{"peer", "z1"}, "local", 4)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, func(a, b ThreadAgentRowFrame) int {
		return strings.Compare(a.Token+"/"+a.ThreadID, b.Token+"/"+b.ThreadID)
	})
	want := []ThreadAgentRowFrame{
		{"", "out"}, {"", "peer"}, {"", "z0"}, {"", "z1"},
		{"direct", "lead"}, {"direct", "z0"}, {"direct", "z1"}, {"direct", "z2"},
		// The links above lead and z1, each for its caller and up.
		{"out-z0", "out"},
		{"running", "peer"},
		{"z0-z1", "out"}, {"z0-z1", "z0"},
		{"z1-z2", "out"}, {"z1-z2", "z0"}, {"z1-z2", "z1"},
		// lead's link, whose row shows lead's rest, for z2 and up.
		{"z2-lead", "out"}, {"z2-lead", "z0"}, {"z2-lead", "z1"}, {"z2-lead", "z2"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("frames %+v; want %+v", got, want)
	}
}

// The frames' read also names the changed requests' callers that are
// workers, whose rest an answer on its way to them moves: a thread a spawn
// here made and one a paired computer's spawn made, each once; not one no
// spawn made.
func TestThreadAgentRowFramesNameTheWorkersAmongTheCallers(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "worker", "peer", "other"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "lead-worker", "lead", ThreadRequestSpawn, "worker", false, 90)
	if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{Token: "their-spawn", OwnerDeviceID: "device-2", SourceThreadID: "x",
		Kind: ThreadRequestSpawn, TargetThreadID: "peer", State: ThreadReceiptRunning}); err != nil {
		t.Fatal(err)
	}
	for i, caller := range []string{"lead", "worker", "worker", "peer"} {
		seedAgentRequest(t, s, fmt.Sprintf("ask-%d", i), caller, ThreadRequestSend, "other", true, int64(100+i))
	}
	_, rest, err := s.ThreadAgentRowFrames([]string{"ask-0", "ask-1", "ask-2", "ask-3", "missing"}, nil, nil, "local", 4)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(rest)
	if want := []string{"peer", "worker"}; !slices.Equal(rest, want) {
		t.Fatalf("rest %v; want %v", rest, want)
	}
}

// referenceAgentTree is the walk the listing replaced, one thread's own
// rows at a time: each row, then, for a local spawn whose thread no
// earlier row reached, that thread's rows one level down; a spawn at the
// depth bound says whether its thread has rows.
func referenceAgentTree(t *testing.T, s *Store, root string, depth int) []string {
	t.Helper()
	own := func(caller string) []ThreadAgentRequest {
		rows, _, err := s.ListThreadAgentTree(ThreadAgentTreeRead{Root: caller, SettledSince: 10_000, Depth: 1, Limit: 1000, AnswerHead: 1})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	visited := map[string]bool{root: true}
	var out []string
	var walk func(caller string, level int)
	walk = func(caller string, level int) {
		for _, row := range own(caller) {
			index := len(out)
			out = append(out, fmt.Sprintf("%s@%d", row.Token, level))
			child := row.TargetThreadID
			if row.Kind != ThreadRequestSpawn || child == "" || row.TargetComputerID != "" || visited[child] {
				continue
			}
			visited[child] = true
			if level+1 >= depth {
				if len(own(child)) > 0 {
					out[index] += "+hidden"
				}
				continue
			}
			walk(child, level+1)
		}
	}
	walk(root, 0)
	return out
}

// The listing matches the walk it replaced on random forests of spawns,
// each thread reached by at most one, with sends between any threads and
// settled rows, and lists every request once.
func TestListThreadAgentTreeMatchesTheWalkOnRandomForests(t *testing.T) {
	t.Parallel()
	random := rand.New(rand.NewPCG(7, 155))
	for graph := range 150 {
		s := newTestStore(t)
		threads := make([]string, 3+random.IntN(6))
		for i := range threads {
			threads[i] = fmt.Sprintf("g%d-t%d", graph, i)
			mustCreateThread(t, s, threads[i])
		}
		edges := 2 + random.IntN(3*len(threads))
		spawned := map[int]bool{}
		for e := range edges {
			token := fmt.Sprintf("g%d-r%02d", graph, e)
			from, to := random.IntN(len(threads)), random.IntN(len(threads))
			caller, target := threads[from], threads[to]
			kind := ThreadRequestSend
			if random.IntN(3) > 0 && to > from && !spawned[to] {
				kind, spawned[to] = ThreadRequestSpawn, true
			}
			seedAgentRequest(t, s, token, caller, kind, target, random.IntN(2) == 0, int64(100+random.IntN(50)))
			if random.IntN(3) == 0 {
				settleAgentRequest(t, s, token, int64(5_000+random.IntN(10_000)))
			}
		}
		for _, depth := range []int{1, 2, 4} {
			want := referenceAgentTree(t, s, threads[0], depth)
			rows, more, err := s.ListThreadAgentTree(ThreadAgentTreeRead{Root: threads[0], SettledSince: 10_000, Depth: depth, Limit: 1000, AnswerHead: 1})
			if err != nil || more {
				t.Fatalf("graph %d depth %d: more=%v err=%v", graph, depth, more, err)
			}
			var got []string
			seen := map[string]bool{}
			for _, row := range rows {
				entry := fmt.Sprintf("%s@%d", row.Token, row.Depth)
				if row.NestedHidden {
					entry += "+hidden"
				}
				got = append(got, entry)
				if seen[row.Token] {
					t.Fatalf("graph %d depth %d lists %s twice: %v", graph, depth, row.Token, got)
				}
				seen[row.Token] = true
			}
			if !slices.Equal(got, want) {
				t.Fatalf("graph %d depth %d:\n got %v\nwant %v", graph, depth, got, want)
			}
			assertAgentTreeMarks(t, s.db, fmt.Sprintf("graph %d", graph))
			// Bounded, the listing is the walk's first rows; named, the
			// walk's rows among those named, each where the walk put it.
			limited := agentTreeRead(threads[0], depth)
			limited.Limit = 1 + random.IntN(len(want)+1)
			rows, more, err = s.ListThreadAgentTree(limited)
			if err != nil {
				t.Fatal(err)
			}
			if got, wantRows := agentTreeEntries(rows), want[:min(len(want), limited.Limit)]; !slices.Equal(got, wantRows) || more != (len(want) > limited.Limit) {
				t.Fatalf("graph %d depth %d limit %d:\n got %v more %v\nwant %v more %v", graph, depth, limited.Limit, got, more, wantRows, len(want) > limited.Limit)
			}
			named := agentTreeRead(threads[0], depth)
			named.Limit, named.Tokens = 1000, []string{}
			var wantNamed []string
			for e := range edges {
				if random.IntN(2) == 0 {
					named.Tokens = append(named.Tokens, fmt.Sprintf("g%d-r%02d", graph, e))
				}
			}
			for _, entry := range want {
				if slices.Contains(named.Tokens, strings.SplitN(entry, "@", 2)[0]) {
					wantNamed = append(wantNamed, entry)
				}
			}
			if rows, _, err = s.ListThreadAgentTree(named); err != nil {
				t.Fatal(err)
			}
			if got := agentTreeEntries(rows); !slices.Equal(got, wantNamed) {
				t.Fatalf("graph %d depth %d named %v:\n got %v\nwant %v", graph, depth, named.Tokens, got, wantNamed)
			}
		}
	}
}

// agentTreeEntries is rows as referenceAgentTree writes them.
func agentTreeEntries(rows []ThreadAgentRequest) []string {
	out := []string{}
	for _, row := range rows {
		entry := fmt.Sprintf("%s@%d", row.Token, row.Depth)
		if row.NestedHidden {
			entry += "+hidden"
		}
		out = append(out, entry)
	}
	return out
}

// A thread is reached by at most one local spawn: the ledger refuses a
// second, a lineage row included, since a spawn whose payload expired is
// still its target's link, and keeps a superseded row or a remote spawn to
// the same thread.
func TestASecondLocalSpawnOfAThreadIsRefused(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "other", "child"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "first", "lead", ThreadRequestSpawn, "child", false, 100)
	for _, insert := range []string{
		`INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, state, created_at, updated_at)
		VALUES ('second', 'other', 'spawn', 'child', 'running', 101, 101)`,
		`INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, state, lineage_only, created_at, updated_at)
		VALUES ('lineage', 'other', 'spawn', 'child', 'finished', 1, 101, 101)`,
	} {
		if _, err := s.db.Exec(insert); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
			t.Fatalf("a second spawn of child: %v; want a UNIQUE refusal", err)
		}
	}
	mustExec(t, s.db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, state, lineage_only, superseded, created_at, updated_at)
		VALUES ('superseded', 'other', 'spawn', 'child', 'finished', 1, 1, 102, 102)`)
	mustExec(t, s.db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, state, created_at, updated_at)
		VALUES ('remote', 'other', 'spawn', 'pc', 'child', 'running', 103, 103)`)
}

// A Stop of every row reads the listing's own level whole, page by page:
// an open row of each kind the listing shows, a settled spawn inside the
// window, a long-settled spawn with open work under it (expired to its
// lineage or not), a parked quiet ask and a remote spawn that reported
// open work, oldest first, and not a send or ask that settled, a quiet
// send, a reminder or a settled spawn with nothing open under it. Every
// page size reads the same rows.
func TestListThreadAgentStopRowsReadsTheListingsLevelWhole(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "w-open", "w-recent", "w-busy", "w-done", "w-gone", "busy-grand", "gone-grand", "peer"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "settled-send", "lead", ThreadRequestSend, "peer", true, 100)
	settleAgentRequest(t, s, "settled-send", 20_000)
	seedAgentRequest(t, s, "open-spawn", "lead", ThreadRequestSpawn, "w-open", false, 101)
	seedAgentRequest(t, s, "recent-spawn", "lead", ThreadRequestSpawn, "w-recent", false, 102)
	settleAgentRequest(t, s, "recent-spawn", 20_000)
	seedAgentRequest(t, s, "busy-spawn", "lead", ThreadRequestSpawn, "w-busy", false, 103)
	settleAgentRequest(t, s, "busy-spawn", 200)
	seedAgentRequest(t, s, "busy-under", "w-busy", ThreadRequestSpawn, "busy-grand", false, 104)
	seedAgentRequest(t, s, "done-spawn", "lead", ThreadRequestSpawn, "w-done", false, 105)
	settleAgentRequest(t, s, "done-spawn", 200)
	seedAgentRequest(t, s, "quiet-send", "lead", ThreadRequestSend, "peer", false, 106)
	seedAgentRequest(t, s, "owed-send", "lead", ThreadRequestSend, "peer", true, 107)
	seedAgentRequest(t, s, "parked-ask", "lead", ThreadRequestAsk, "peer", false, 108)
	seedAgentRequest(t, s, "settled-ask", "lead", ThreadRequestAsk, "peer", true, 109)
	settleAgentRequest(t, s, "settled-ask", 20_000)
	seedAgentRequest(t, s, "remote-spawn", "lead", ThreadRequestSpawn, "", false, 110)
	mustExec(t, s.db, `UPDATE thread_requests SET target_computer_id = 'pc', target_thread_id = 'far' WHERE token = 'remote-spawn'`)
	settleAgentRequest(t, s, "remote-spawn", 200)
	if _, err := s.SetThreadRequestNestedOpen("remote-spawn", true); err != nil {
		t.Fatal(err)
	}
	seedAgentRequest(t, s, "gone-spawn", "lead", ThreadRequestSpawn, "w-gone", false, 111)
	settleAgentRequest(t, s, "gone-spawn", 200)
	if _, err := s.DeleteThreadRequestsBefore(5_000); err != nil {
		t.Fatal(err)
	}
	seedAgentRequest(t, s, "gone-under", "w-gone", ThreadRequestSend, "gone-grand", true, 6_000)
	seedAgentRequest(t, s, "reminder", "lead", ThreadRequestRemind, "", true, 112)

	want := []string{"open-spawn", "recent-spawn", "busy-spawn", "owed-send", "parked-ask", "remote-spawn", "gone-spawn"}
	for _, page := range []int{1, 2, 3, len(want), 64} {
		rows, err := s.ListThreadAgentStopRows(ThreadAgentStopRead{Root: "lead", Waiting: []string{"parked-ask"}, SettledSince: 10_000, Page: page})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if got := agentRequestTokens(rows); !slices.Equal(got, want) {
			t.Fatalf("page %d read %v; want %v", page, got, want)
		}
		for _, row := range rows {
			if row.Answer != nil || row.LateReply != nil {
				t.Fatalf("page %d: %s carries its answer", page, row.Token)
			}
		}
		if gone := rows[len(rows)-1]; !gone.LineageOnly || gone.TargetThreadID != "w-gone" {
			t.Fatalf("the expired spawn reads %+v; want its lineage with its thread", gone)
		}
	}
	if _, err := s.ListThreadAgentStopRows(ThreadAgentStopRead{Root: "lead", Page: 0}); err == nil {
		t.Fatal("a page of no rows was read")
	}
}

// The pages are one snapshot: a request made between two of them, after
// every row read so far, is in none of them.
func TestListThreadAgentStopRowsReadsOneSnapshot(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	mustCreateThread(t, s, "peer")
	for index := range 3 {
		seedAgentRequest(t, s, fmt.Sprintf("owed-%d", index), "lead", ThreadRequestSend, "peer", true, int64(100+index))
	}
	pages := 0
	s.agentStopPageReadForTest = func() {
		pages++
		if pages == 1 {
			seedAgentRequest(t, s, "late", "lead", ThreadRequestSend, "peer", true, 1_000)
		}
	}
	rows, err := s.ListThreadAgentStopRows(ThreadAgentStopRead{Root: "lead", SettledSince: 10_000, Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := agentRequestTokens(rows), []string{"owed-0", "owed-1", "owed-2"}; !slices.Equal(got, want) {
		t.Fatalf("read %v; want %v, the ledger as the first page found it", got, want)
	}
	if pages < 2 {
		t.Fatalf("read %d full pages; the test needs the write between two", pages)
	}
}
