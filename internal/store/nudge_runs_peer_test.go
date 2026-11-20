package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// v161 adds the table of the nudge runs kept for a paired computer's links,
// with its indexes. A receipt from before it keeps every column it had and
// has no nudge.
func TestReceiptNudgeRunsMigration(t *testing.T) {
	db := migrateThrough(t, receiptNudgeRunsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO thread_request_receipts (token, owner_device_id, source_computer_id, source_thread_id, kind, state, created_at, updated_at)
		VALUES ('old-link', 'dev', 'pc', 'lead', 'spawn', 'finished', 5, 5)`)

	migrateFrom(t, db, receiptNudgeRunsMigrationVersion-1)

	var state string
	if err := db.QueryRow(`SELECT state FROM thread_request_receipts WHERE token = 'old-link'`).Scan(&state); err != nil || state != "finished" {
		t.Fatalf("the old receipt reads %q, %v; want it as it was", state, err)
	}
	var nudges, columns int
	if err := db.QueryRow(`SELECT count(*) FROM thread_receipt_nudges`).Scan(&nudges); err != nil || nudges != 0 {
		t.Fatalf("thread_receipt_nudges holds %d, %v; want none", nudges, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('thread_request_receipts') WHERE name LIKE 'nudge\_%' ESCAPE '\'`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("thread_request_receipts has %d nudge columns, %v; want none", columns, err)
	}
	for _, name := range []string{"idx_thread_receipt_nudges_open", "idx_thread_receipt_nudges_link", "idx_thread_receipt_nudges_ended"} {
		var index string
		if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&index); err != nil || index == "" {
			t.Fatalf("%s: %q, %v", name, index, err)
		}
	}
}

// peerRestedWorker is w, owned through a paired computer's link "link" whose
// spawn's run ended with the worker at 3 calls, 30 tokens and 300 ms.
func peerRestedWorker(t *testing.T, s *Store) {
	t.Helper()
	mustCreateThread(t, s, "w")
	acceptLink(t, s, "link", "dev", "w", 10)
	if _, err := s.SetThreadPlacement([]string{"w"}, PlacementOwned); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `INSERT INTO thread_tallies (thread_id, tool_calls, tokens, turn_ms) VALUES ('w', 3, 30, 300)
		ON CONFLICT(thread_id) DO UPDATE SET tool_calls = 3, tokens = 30, turn_ms = 300`)
	mustExec(t, s.db, `UPDATE thread_request_receipts SET state = 'finished', settled_at = 20,
		run_open = 0, run_tool_calls = 3, run_tokens = 30, run_turn_ms = 300 WHERE token = 'link'`)
}

// peerNudges reads the link's nudge runs as its owner's answer does.
func peerNudges(t *testing.T, s *Store) []NudgeRun {
	t.Helper()
	run, err := s.ReceiptRunOf("dev", "lead-link", "w")
	if err != nil {
		t.Fatal(err)
	}
	return run.Nudges
}

// peerNudge reads the link's one nudge run, nil for none.
func peerNudge(t *testing.T, s *Store) *NudgeRun {
	t.Helper()
	nudges := peerNudges(t, s)
	switch len(nudges) {
	case 0:
		return nil
	case 1:
		return &nudges[0]
	}
	t.Fatalf("the link reports nudges %+v; want one", nudges)
	return nil
}

// restPeerWorker brings w to rest with calls more tool calls than before,
// ending the runs its rest ends.
func restPeerWorker(t *testing.T, s *Store, calls int) {
	t.Helper()
	mustExec(t, s.db, `UPDATE thread_tallies SET tool_calls = tool_calls + ? WHERE thread_id = 'w'`, calls)
	rest := &RestGuard{Roots: []string{"w"}, AtRest: func(SubtreeRest) []string { return []string{"w"} }}
	if ended, err := s.EndThreadRequestRunsAtRest("w", rest); err != nil || !ended {
		t.Fatalf("EndThreadRequestRunsAtRest = %v, %v", ended, err)
	}
}

// nudgeTokens is the tokens of nudges, in order.
func nudgeTokens(nudges []NudgeRun) []string {
	out := []string{}
	for _, n := range nudges {
		out = append(out, n.Token)
	}
	return out
}

// Each nudge run is kept on its own until the link's owner takes its end:
// two that ended and one open, between two reads, are each reported, the
// open one first, then the ended ones oldest first, each with its own
// figures. A take forgets the ended runs it names and leaves the open one;
// a repeated take, a take of another link's owner or source, and a take of
// an open run change nothing. A restart keeps what was not taken.
func TestAPeerLinkKeepsEachNudgeRunUntilItsOwnerTakesIt(t *testing.T) {
	t.Parallel()
	s := openStoreAt(t)
	peerRestedWorker(t, s)
	for i, token := range []string{"a", "b", "c"} {
		if !startNudge(t, s, token, "") {
			t.Fatalf("nudge %s recorded no run", token)
		}
		if i < 2 {
			restPeerWorker(t, s, i+1)
		}
	}
	nudges := peerNudges(t, s)
	if got := nudgeTokens(nudges); !slices.Equal(got, []string{"c", "a", "b"}) {
		t.Fatalf("the link reports %v; want the open one, then the ended oldest first", got)
	}
	if nudges[0].End != nil || nudges[1].End == nil || nudges[2].End == nil ||
		nudges[1].Base.ToolCalls != 3 || nudges[1].End.ToolCalls != 4 ||
		nudges[2].Base.ToolCalls != 4 || nudges[2].End.ToolCalls != 6 || nudges[0].Base.ToolCalls != 6 {
		t.Fatalf("nudges %+v; want each with its own base and end", nudges)
	}
	for _, other := range []struct{ owner, source string }{{"other-dev", "lead-link"}, {"dev", "another"}} {
		if err := s.TakeNudgeRuns(other.owner, other.source, "w", []string{"a", "b", "c"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := nudgeTokens(peerNudges(t, s)); len(got) != 3 {
		t.Fatalf("a take of another owner or source left %v; want all three", got)
	}
	s = reopenStore(t, s)
	if got := nudgeTokens(peerNudges(t, s)); !slices.Equal(got, []string{"c", "a", "b"}) {
		t.Fatalf("after a restart the link reports %v; want all three", got)
	}
	for range 2 {
		if err := s.TakeNudgeRuns("dev", "lead-link", "w", []string{"a", "b", "c"}); err != nil {
			t.Fatal(err)
		}
		if got := nudgeTokens(peerNudges(t, s)); !slices.Equal(got, []string{"c"}) {
			t.Fatalf("after the take the link reports %v; want the open one alone", got)
		}
	}
}

// One answer reports at most nudgeRunsReported runs; the rest follow once
// the owner takes those.
func TestAPeerLinksNudgeRunsAreReportedInBoundedAnswers(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	peerRestedWorker(t, s)
	total := nudgeRunsReported + 4
	for i := range total {
		if !startNudge(t, s, fmt.Sprintf("n%02d", i), "") {
			t.Fatalf("nudge %d recorded no run", i)
		}
		restPeerWorker(t, s, 1)
	}
	first := peerNudges(t, s)
	if len(first) != nudgeRunsReported || first[0].Token != "n00" {
		t.Fatalf("the first answer reports %v; want %d from the oldest", nudgeTokens(first), nudgeRunsReported)
	}
	if err := s.TakeNudgeRuns("dev", "lead-link", "w", nudgeTokens(first)); err != nil {
		t.Fatal(err)
	}
	if rest := peerNudges(t, s); len(rest) != total-nudgeRunsReported || rest[0].Token != fmt.Sprintf("n%02d", nudgeRunsReported) {
		t.Fatalf("the next answer reports %v; want the remaining %d", nudgeTokens(rest), total-nudgeRunsReported)
	}
}

// An ended nudge run its owner never takes goes at the retention floor,
// read through idx_thread_receipt_nudges_ended; an open one is never past
// it.
func TestRetentionForgetsAnUntakenEndedNudgeRunOnly(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	peerRestedWorker(t, s)
	for _, token := range []string{"ended", "open"} {
		if !startNudge(t, s, token, "") {
			t.Fatalf("nudge %s recorded no run", token)
		}
		if token == "ended" {
			restPeerWorker(t, s, 1)
		}
	}
	// Past the floor the runs alone: the link, made at 10, stands.
	const floor = 5
	mustExec(t, s.db, `UPDATE thread_receipt_nudges SET started_at = 1, ended_at = CASE WHEN run_open = 0 THEN 2 END`)
	if _, _, err := s.DeleteThreadRequestsBefore(floor, nil); err != nil {
		t.Fatal(err)
	}
	if nudge := peerNudge(t, s); nudge == nil || nudge.Token != "open" {
		t.Fatalf("after the sweep the link reports %+v; want the open run alone", nudge)
	}
	if plan := queryPlanText(t, s.db, `EXPLAIN QUERY PLAN SELECT 1 FROM thread_receipt_nudges WHERE `+nudgePastFloorSQL, floor); !strings.Contains(plan, "idx_thread_receipt_nudges_ended") {
		t.Fatalf("the sweep's nudge set reads %s; want idx_thread_receipt_nudges_ended", plan)
	}
}

// The device's last session ending releases its link, which ends its open
// nudge run, and forgets every nudge run kept for its links: no device left
// can take them.
func TestASessionEndForgetsTheNudgeRunsOfItsDevicesLinks(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	peerRestedWorker(t, s)
	for _, token := range []string{"ended", "open"} {
		if !startNudge(t, s, token, "") {
			t.Fatalf("nudge %s recorded no run", token)
		}
		if token == "ended" {
			restPeerWorker(t, s, 1)
		}
	}
	if _, released, err := s.RevokeSession("reach-dev-0", nowMillis()); err != nil || len(released) != 1 {
		t.Fatalf("RevokeSession released %v, %v; want the worker", released, err)
	}
	var kept int
	if err := s.db.QueryRow(`SELECT count(*) FROM thread_receipt_nudges`).Scan(&kept); err != nil || kept != 0 {
		t.Fatalf("%d nudge runs kept, %v; want none for a device that holds no session", kept, err)
	}
	if ending, err := s.ThreadRunsEnding("w"); err != nil || ending {
		t.Fatalf("ThreadRunsEnding = %v, %v; want none open", ending, err)
	}
}

// A paired computer's owned worker here is nudged by the rule a local one
// is: a message from the person or a thread other than its spawner there,
// with no run of the spawner's open on it, records a run on the spawn's
// link, its base the worker's figures then; one while it is open joins it.
// The spawner's own message, a run of the spawner's open on a receipt, and
// a worker with a sidebar row record none. The owner's read reports the
// nudge apart from the spawner's own runs.
func TestAPeerSpawnsLinkRecordsANudgeOfItsWorker(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	peerRestedWorker(t, s)
	start := func(token, by string) bool {
		t.Helper()
		return startNudge(t, s, token, by)
	}
	if start("by-spawner", "lead-link") {
		t.Fatal("the spawner's own message recorded a nudge")
	}
	mustExec(t, s.db, `UPDATE thread_request_receipts SET run_open = 1 WHERE token = 'link'`)
	if start("in-run", "") {
		t.Fatal("a nudge during the spawner's run recorded a run of its own")
	}
	mustExec(t, s.db, `UPDATE thread_request_receipts SET run_open = 0 WHERE token = 'link'`)
	if _, err := s.SetThreadPlacement([]string{"w"}, PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if start("sidebar", "") {
		t.Fatal("a worker with a sidebar row recorded a nudge")
	}
	if _, err := s.SetThreadPlacement([]string{"w"}, PlacementOwned); err != nil {
		t.Fatal(err)
	}
	if nudge := peerNudge(t, s); nudge != nil {
		t.Fatalf("the link reports nudge %+v before any", nudge)
	}

	admitted, reaches, err := s.ReadNudgeStart("w")
	if err != nil || !reaches {
		t.Fatalf("ReadNudgeStart = %+v, %v, %v", admitted, reaches, err)
	}
	// The worker works between the message's admission and its acceptance.
	mustExec(t, s.db, `UPDATE thread_tallies SET tool_calls = 9 WHERE thread_id = 'w'`)
	if started, err := s.StartNudgeRun("nudge", "w", "", admitted); err != nil || !started {
		t.Fatalf("StartNudgeRun = %v, %v; want the person's message's nudge run", started, err)
	}
	nudge := peerNudge(t, s)
	if nudge == nil || nudge.Token != "nudge" || nudge.End != nil || nudge.StartedAt == 0 ||
		nudge.Base != (RunFigures{ToolCalls: 3, Tokens: 30, TurnMs: 300}) {
		t.Fatalf("nudge %+v; want it open from the worker's figures then", nudge)
	}
	if run, err := s.ReceiptRunOf("dev", "lead-link", "w"); err != nil || run.Open || run.End == nil {
		t.Fatalf("the spawner's own runs %+v, %v; want them as they were, ended", run, err)
	}
	if start("joined", "another") {
		t.Fatal("a second nudge while one is open recorded a run of its own")
	}
	if threads, any, err := s.OpenRunThreads(); err != nil || !any || len(threads) != 1 || threads[0] != "w" {
		t.Fatalf("OpenRunThreads = %v %v, %v; want the nudged worker", threads, any, err)
	}
	if ending, err := s.ThreadRunsEnding("w"); err != nil || !ending {
		t.Fatalf("ThreadRunsEnding = %v, %v; want the nudge's run", ending, err)
	}
	if listed, err := s.ListThreadRequestReceiptsForThread("w"); err != nil || len(listed) != 1 {
		t.Fatalf("receipts %+v, %v; want the link alone, a nudge being no request", listed, err)
	}
}

// A link's nudge run ends at its worker's rest, with the worker's figures
// over its life then, and so do the spawner's runs on its receipts.
func TestAPeerNudgeRunEndsAtItsWorkersRest(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	peerRestedWorker(t, s)
	if !startNudge(t, s, "nudge", "") {
		t.Fatal("the nudge recorded no run")
	}
	mustExec(t, s.db, `UPDATE thread_tallies SET tool_calls = 5, tokens = 90, turn_ms = 700 WHERE thread_id = 'w'`)
	rest := &RestGuard{Roots: []string{"w"}, AtRest: func(SubtreeRest) []string { return []string{"w"} }}
	if ended, err := s.EndThreadRequestRunsAtRest("w", rest); err != nil || !ended {
		t.Fatalf("EndThreadRequestRunsAtRest = %v, %v", ended, err)
	}
	nudge := peerNudge(t, s)
	if nudge == nil || nudge.End == nil || *nudge.End != (RunFigures{ToolCalls: 5, Tokens: 90, TurnMs: 700}) {
		t.Fatalf("nudge %+v; want it ended with the worker's figures at its rest", nudge)
	}
	if ending, err := s.ThreadRunsEnding("w"); err != nil || ending {
		t.Fatalf("ThreadRunsEnding = %v, %v; want none open", ending, err)
	}
}

// A link's open nudge run ends, with the worker's figures then, when the
// worker gets a sidebar row here or a group, and when the link ends.
func TestAPeerNudgeRunEndsWithTheWorkersOwnership(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		end  func(t *testing.T, s *Store)
	}{
		{"shown in the sidebar", func(t *testing.T, s *Store) {
			if _, err := s.SetThreadPlacement([]string{"w"}, PlacementStandalone); err != nil {
				t.Fatal(err)
			}
		}},
		{"grouped", func(t *testing.T, s *Store) {
			thread, err := s.GetThread("w")
			if err != nil {
				t.Fatal(err)
			}
			group, err := s.CreateThreadGroup(thread.ProjectID, "Sweep")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetThreadGroup([]string{"w"}, group.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"its link released", func(t *testing.T, s *Store) {
			if released, err := s.ReleaseSpawnLink("link", "dev", "lead-link"); err != nil || len(released) != 1 {
				t.Fatalf("ReleaseSpawnLink = %v, %v", released, err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			peerRestedWorker(t, s)
			if !startNudge(t, s, "nudge", "") {
				t.Fatal("the nudge recorded no run")
			}
			mustExec(t, s.db, `UPDATE thread_tallies SET tool_calls = 4 WHERE thread_id = 'w'`)
			c.end(t, s)
			nudge := peerNudge(t, s)
			if nudge == nil || nudge.End == nil || nudge.End.ToolCalls != 4 {
				t.Fatalf("nudge %+v; want it ended with the worker's figures then", nudge)
			}
			if ending, err := s.ThreadRunsEnding("w"); err != nil || ending {
				t.Fatalf("ThreadRunsEnding = %v, %v; want none open", ending, err)
			}
		})
	}
}

// The spawner's computer records a paired computer's nudge run as the
// spawn's caller's nudge row, a settled send that owes nothing, its base
// what that computer reported; the row ends with the run's figures less
// its base once that computer says the run ended, and an answer repeating
// either writes nothing. A nudge of an archived caller, or older than the
// retention floor, records nothing. An end of the spawner's runs there
// leaves the nudge's row to the nudge's own word, and the end of the
// pairing ends it with nothing more counted.
func TestTheSpawnersComputerRecordsAPeerNudgeRun(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	insertRun(t, s, "spawn", ThreadRequestSpawn, "pc", "w", &RunFigures{}, 10)
	settleAgentRequest(t, s, "spawn", 20)
	if err := s.EndRemoteThreadRequestRuns("pc", "w", RunFigures{ToolCalls: 3, Tokens: 30, TurnMs: 300}); err != nil {
		t.Fatal(err)
	}
	now := nowMillis()
	run := NudgeRun{Token: "nudge", StartedAt: now, Base: RunFigures{ToolCalls: 3, Tokens: 30, TurnMs: 300}}
	record := func(run NudgeRun) bool {
		t.Helper()
		wrote, err := s.RecordRemoteNudgeRun("spawn", run)
		if err != nil {
			t.Fatal(err)
		}
		return wrote
	}
	if !record(run) {
		t.Fatal("the nudge run was not recorded")
	}
	row := rawThreadRequest(t, s, "nudge")
	if !row.Nudge || row.CallerThreadID != "lead" || row.Kind != ThreadRequestSend || row.TargetComputerID != "pc" ||
		row.TargetThreadID != "w" || row.State != ThreadRequestFinished || row.SettledAt == 0 || row.Notify ||
		!row.RunOpen() || row.RunToken != "nudge" || *row.RunBase != run.Base {
		t.Fatalf("nudge row %+v; want a settled, unowed send of the spawner's with its run open from the reported base", row)
	}
	if listed, err := s.ListThreadRequestsByCaller("lead", 10, 0); err != nil || len(listed) != 1 {
		t.Fatalf("ListThreadRequestsByCaller = %+v, %v; want the spawn alone", listed, err)
	}
	if record(run) {
		t.Fatal("a repeated open nudge wrote again")
	}
	if err := s.EndRemoteThreadRequestRuns("pc", "w", RunFigures{ToolCalls: 3, Tokens: 30, TurnMs: 300}); err != nil {
		t.Fatal(err)
	}
	if rawThreadRequest(t, s, "nudge").RunEnded() {
		t.Fatal("the end of the spawner's own runs ended the nudge's")
	}
	run.End = &RunFigures{ToolCalls: 5, Tokens: 90, TurnMs: 700}
	if !record(run) {
		t.Fatal("the nudge's end was not recorded")
	}
	if total := rawThreadRequest(t, s, "nudge").RunTotal; total == nil || *total != (RunFigures{ToolCalls: 2, Tokens: 60, TurnMs: 400}) {
		t.Fatalf("nudge run %+v; want its figures less its base", total)
	}
	if record(run) {
		t.Fatal("a repeated ended nudge wrote again")
	}

	ended := NudgeRun{Token: "seen-ended", StartedAt: now, Base: RunFigures{ToolCalls: 5}, End: &RunFigures{ToolCalls: 6}}
	if !record(ended) || !rawThreadRequest(t, s, "seen-ended").RunEnded() {
		t.Fatal("a nudge first heard of once ended was not recorded ended")
	}
	old := NudgeRun{Token: "old", StartedAt: now - int64(ThreadRequestRetentionDays+1)*24*60*60*1000, End: &RunFigures{}}
	if record(old) {
		t.Fatal("a nudge older than the retention floor was recorded")
	}
	open := NudgeRun{Token: "open-at-the-end", StartedAt: now, Base: RunFigures{ToolCalls: 6}}
	if !record(open) {
		t.Fatal("the open nudge was not recorded")
	}
	if _, err := s.EndPairing("pc", nil, PairingEnd{}); err != nil {
		t.Fatal(err)
	}
	if total := rawThreadRequest(t, s, "open-at-the-end").RunTotal; total == nil || *total != (RunFigures{}) {
		t.Fatalf("after the pairing ended the nudge run is %+v; want it ended with nothing more counted", total)
	}
	mustExec(t, s.db, `UPDATE threads SET archived = 1 WHERE id = 'lead'`)
	if record(NudgeRun{Token: "archived", StartedAt: now}) {
		t.Fatal("a nudge of an archived spawner was recorded")
	}
}
