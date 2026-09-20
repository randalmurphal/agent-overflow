package threadtools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func spawnApp() *fakeApp {
	app := newFakeApp("Laptop")
	app.addThread(Thread{ID: "caller-thread", Title: "Caller", Provider: "claude", Model: "opus-5"})
	app.ack = RequestAck{Token: "tok-1", ThreadID: localThreadID, Title: "New work", State: StateRunning, Outcome: OutcomeBackgrounded, Revision: 1}
	return app
}

// TestSpawnPassesThePromptAndSettingsThrough and answers with the token
// and what to do with it.
func TestSpawnPassesThePromptAndSettingsThrough(t *testing.T) {
	app := spawnApp()
	result := call(t, New(app), localCaller(), "thread_spawn", `{"prompt":"port the parser","title":"Port","project_id":"p1","provider":"codex","model":"gpt-5","effort":"high","mode":"plan","runtime_mode":"read-only","worktree":"port","base":"release/2.4","base_local":true,"group":"Port sweep","wait_seconds":120,"notify":true}`)

	if len(app.spawns) != 1 {
		t.Fatalf("spawns = %v", app.spawns)
	}
	spawn := app.spawns[0]
	if spawn.Prompt != "port the parser" || spawn.Title != "Port" || spawn.ProjectID != "p1" {
		t.Fatalf("spawn = %+v", spawn)
	}
	if spawn.Provider != "codex" || spawn.Model != "gpt-5" || spawn.Effort != "high" || spawn.Mode != "plan" || spawn.RuntimeMode != "read-only" {
		t.Fatalf("settings did not reach the app: %+v", spawn)
	}
	if spawn.WorktreeBranch != "port" || spawn.Group != "Port sweep" || spawn.WaitSeconds != 120 || !spawn.Notify {
		t.Fatalf("spawn = %+v", spawn)
	}
	if spawn.WorktreeBase != "release/2.4" || !spawn.WorktreeBaseLocal {
		t.Fatalf("worktree base did not reach the app: %+v", spawn)
	}
	if result["token"] != "tok-1" || result["kind"] != "spawn" || result["outcome"] != OutcomeBackgrounded {
		t.Fatalf("result = %v", result)
	}
	if note, _ := result["note"].(string); !strings.Contains(note, "thread_status with token tok-1") {
		t.Errorf("note = %q", note)
	}
}

// TestSpawnValidatesEverythingItCan, so a bad call costs one refusal and
// never a half-started thread.
func TestSpawnValidatesEverythingItCan(t *testing.T) {
	server := New(spawnApp())
	for name, args := range map[string]string{
		"blank prompt":          `{"prompt":"   "}`,
		"missing prompt":        `{"title":"x"}`,
		"bad mode":              `{"prompt":"go","mode":"planning"}`,
		"bad runtime mode":      `{"prompt":"go","runtime_mode":"yolo"}`,
		"workspace and tree":    `{"prompt":"go","workspace_path":"/src/x","worktree":"feature"}`,
		"worktree not a string": `{"prompt":"go","worktree":true}`,
		"base without worktree": `{"prompt":"go","base":"main"}`,
		"base_local alone":      `{"prompt":"go","base_local":true}`,
		"wait too long":         `{"prompt":"go","wait_seconds":100000}`,
		"negative wait":         `{"prompt":"go","wait_seconds":-1}`,
		"long title":            `{"prompt":"go","title":"` + strings.Repeat("t", MaxTitleRunes+1) + `"}`,
		"unknown field":         `{"prompt":"go","nickname":"bob"}`,
	} {
		t.Run(name, func(t *testing.T) {
			callErr(t, server, localCaller(), "thread_spawn", args, CodeInvalidRequest)
		})
	}
}

// TestSpawnOnAnotherComputerNeedsAProjectAndTheRefusalListsThem: projects
// are registered per computer and cannot be inherited.
func TestSpawnOnAnotherComputerNeedsAProjectAndTheRefusalListsThem(t *testing.T) {
	p := newPair(t)
	p.local.addThread(Thread{ID: "caller-thread", Provider: "claude"})
	p.remote.addThread(Thread{ID: "caller-thread", Provider: "claude"})
	p.remote.catalog = catalogOf("studio")

	message := callErr(t, p.server, localCaller(), "thread_spawn", `{"prompt":"go","computer_id":"studio"}`, CodeInvalidRequest)
	if !strings.Contains(message, "needs project_id") || !strings.Contains(message, "studio repo (p1)") {
		t.Fatalf("the refusal does not list that computer's projects: %q", message)
	}
	if len(p.remote.spawns) != 0 {
		t.Fatal("a refused spawn still reached the destination")
	}

	p.local.ack = RequestAck{Token: "tok-9", ComputerID: "studio", Computer: "Studio", State: StateRunning, Outcome: OutcomeBackgrounded}
	result := call(t, p.server, localCaller(), "thread_spawn", `{"prompt":"go","computer_id":"studio","project_id":"p1"}`)
	if p.local.spawns[0].ComputerID != "studio" {
		t.Fatalf("the destination was lost: %+v", p.local.spawns[0])
	}
	if result["computer_id"] != "studio" {
		t.Fatalf("result = %v", result)
	}
}

// TestSpawnFromAThreadRunsWhereThatThreadLives: a fork runs on its own
// computer, so naming a different one is a contradiction.
func TestSpawnFromAThreadRunsWhereThatThreadLives(t *testing.T) {
	p := newPair(t)
	p.local.addThread(Thread{ID: "caller-thread", Provider: "claude"})
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Origin"})
	p.local.ack = RequestAck{Token: "tok-2", ComputerID: "studio", Computer: "Studio", State: StateRunning, Outcome: OutcomeBackgrounded}

	call(t, p.server, localCaller(), "thread_spawn", `{"prompt":"continue","from_thread":"`+remoteThreadID+`"}`)
	spawn := p.local.spawns[0]
	if spawn.FromThread != remoteThreadID || spawn.FromThreadComputer != "studio" || spawn.ComputerID != "studio" {
		t.Fatalf("fork = %+v", spawn)
	}

	message := callErr(t, p.server, localCaller(), "thread_spawn", `{"prompt":"continue","from_thread":"`+remoteThreadID+`","computer_id":"laptop"}`, CodeInvalidRequest)
	if !strings.Contains(message, "a fork runs on its own computer") {
		t.Errorf("message = %q", message)
	}
}

// TestSendAndAskRefuseTheCallersOwnThread: a thread cannot send to
// itself, and the refusal names what to do instead.
func TestSendAndAskRefuseTheCallersOwnThread(t *testing.T) {
	app := spawnApp()
	server := New(app)
	message := callErr(t, server, localCaller(), "thread_send", `{"thread_id":"caller-thread","message":"hello"}`, CodeSelfSend)
	if !strings.Contains(message, "thread_remind") {
		t.Errorf("message = %q", message)
	}
	callErr(t, server, localCaller(), "thread_ask", `{"thread_id":"caller-thread","question":"who am i"}`, CodeSelfSend)
	callErr(t, server, localCaller(), "thread_status", `{"thread_ids":["caller-thread"]}`, CodeSelfSend)
	if len(app.sends)+len(app.asks) != 0 {
		t.Fatal("a self-send reached the app")
	}
}

// TestAskWaitsByDefaultAndSendDoesNot.
func TestAskWaitsByDefaultAndSendDoesNot(t *testing.T) {
	app := spawnApp()
	app.addThread(Thread{ID: localThreadID, Title: "Worker"})
	server := New(app)

	call(t, server, localCaller(), "thread_send", `{"thread_id":"`+localThreadID+`","message":"go"}`)
	if app.sends[0].WaitSeconds != 0 {
		t.Errorf("send wait = %d, want 0", app.sends[0].WaitSeconds)
	}
	call(t, server, localCaller(), "thread_ask", `{"thread_id":"`+localThreadID+`","question":"status?"}`)
	if app.asks[0].WaitSeconds != DefaultAskWaitSeconds {
		t.Errorf("ask wait = %d, want %d", app.asks[0].WaitSeconds, DefaultAskWaitSeconds)
	}
	call(t, server, localCaller(), "thread_ask", `{"thread_id":"`+localThreadID+`","question":"status?","wait_seconds":0}`)
	if app.asks[1].WaitSeconds != 0 {
		t.Errorf("an explicit zero wait was overridden: %d", app.asks[1].WaitSeconds)
	}
	callErr(t, server, localCaller(), "thread_send", `{"thread_id":"`+localThreadID+`","message":"go","wait_seconds":901}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_send", `{"thread_id":"`+localThreadID+`","message":"  "}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_ask", `{"thread_id":"`+localThreadID+`"}`, CodeInvalidRequest)
}

// TestAckNotesMatchTheOutcome, because the note is the recovery hint.
func TestAckNotesMatchTheOutcome(t *testing.T) {
	app := spawnApp()
	app.addThread(Thread{ID: localThreadID, Title: "Worker"})
	server := New(app)

	app.ack = RequestAck{Token: "t1", Outcome: OutcomeSettled, AnswerKind: AnswerReply, Answer: "done", State: RequestReplied}
	settled := call(t, server, localCaller(), "thread_ask", `{"thread_id":"`+localThreadID+`","question":"q"}`)
	if note := settled["note"].(string); !strings.Contains(note, "no message will arrive") {
		t.Errorf("settled note = %q", note)
	}

	app.ack = RequestAck{Token: "t2", Outcome: OutcomeSettled, AnswerKind: AnswerFinal, Answer: "I started", State: RequestReplied}
	final := call(t, server, localCaller(), "thread_ask", `{"thread_id":"`+localThreadID+`","question":"q"}`)
	if note := final["note"].(string); !strings.Contains(note, "without calling thread_reply") {
		t.Errorf("final note = %q", note)
	}

	app.ack = RequestAck{Token: "t3", Outcome: OutcomeBlocked, State: RequestRunning}
	blocked := call(t, server, localCaller(), "thread_ask", `{"thread_id":"`+localThreadID+`","question":"q"}`)
	if note := blocked["note"].(string); !strings.Contains(note, "waiting on the user") {
		t.Errorf("blocked note = %q", note)
	}

	app.ack = RequestAck{Token: "t4", Outcome: OutcomeUnconfirmed, State: RequestUnconfirmed}
	unconfirmed := call(t, server, localCaller(), "thread_ask", `{"thread_id":"`+localThreadID+`","question":"q"}`)
	if note := unconfirmed["note"].(string); !strings.Contains(note, "Do NOT start it again") {
		t.Errorf("unconfirmed note = %q", note)
	}
}

// TestReplyNeedsATokenAndText and says what happened to it.
func TestReplyNeedsATokenAndText(t *testing.T) {
	app := newFakeApp("Laptop")
	app.replyAck = ReplyAck{Token: "tok-1", Accepted: true, State: RequestReplied, Revision: 3, SourceThreadID: remoteThreadID, SourceComputer: "Studio"}
	server := New(app)

	result := call(t, server, localCaller(), "thread_reply", `{"token":"tok-1","text":"the parser is done"}`)
	if app.replies[0].Token != "tok-1" || app.replies[0].Text != "the parser is done" {
		t.Fatalf("reply = %+v", app.replies[0])
	}
	if result["accepted"] != true || result["source_thread_id"] != remoteThreadID {
		t.Fatalf("result = %v", result)
	}
	if note := result["note"].(string); !strings.Contains(note, "even if its computer is unreachable") {
		t.Errorf("note = %q", note)
	}

	app.replyAck = ReplyAck{Token: "tok-1", Accepted: false, State: RequestReplied}
	repeat := call(t, server, localCaller(), "thread_reply", `{"token":"tok-1","text":"the parser is done"}`)
	if note := repeat["note"].(string); !strings.Contains(note, "already answered") {
		t.Errorf("note = %q", note)
	}

	app.replyAck = ReplyAck{Token: "tok-1", Accepted: true, Late: true}
	late := call(t, server, localCaller(), "thread_reply", `{"token":"tok-1","text":"one more thing"}`)
	if note := late["note"].(string); !strings.Contains(note, "follow-up") {
		t.Errorf("note = %q", note)
	}

	callErr(t, server, localCaller(), "thread_reply", `{"text":"hello"}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_reply", `{"token":"tok-1","text":"  "}`, CodeInvalidRequest)
}

// TestStatusTakesTokensOrThreadIdsButNotBoth, and at most eight.
func TestStatusTakesTokensOrThreadIdsButNotBoth(t *testing.T) {
	app := newFakeApp("Laptop")
	server := New(app)
	callErr(t, server, localCaller(), "thread_status", `{"tokens":["a"],"thread_ids":["`+localThreadID+`"]}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_status", `{"tokens":["a","b","c","d","e","f","g","h","i"]}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_status", `{"tokens":["a","a"]}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_status", `{"tokens":["  "]}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_status", `{"tokens":["a"],"wait_seconds":901}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_status", `{"tokens":["a","b"],"to_file":true}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_status", `{"to_file":true}`, CodeInvalidRequest)

	app.addThread(Thread{ID: localThreadID, Title: "One"})
	app.addThread(Thread{ID: twinThreadID, Title: "Two"})
	callErr(t, server, localCaller(), "thread_status", `{"thread_ids":["`+localThreadID+`","`+localThreadID+`"]}`, CodeInvalidRequest)
}

// TestStatusWithNoArgumentsListsTheCallersRequests, so a lost token is
// never a dead end.
func TestStatusWithNoArgumentsListsTheCallersRequests(t *testing.T) {
	app := newFakeApp("Laptop")
	app.listing = RequestListing{Requests: []RequestState{{Token: "tok-1", Kind: "ask", State: RequestRunning}}, More: true}
	server := New(app)

	result := call(t, server, localCaller(), "thread_status", `{}`)
	if len(rows(t, result["requests"])) != 1 || result["cursor"] == nil {
		t.Fatalf("result = %v", result)
	}
	if note := result["note"].(string); !strings.Contains(note, "never have to remember a token") {
		t.Errorf("note = %q", note)
	}
	next := call(t, server, localCaller(), "thread_status", mustJSON(t, map[string]any{"cursor": result["cursor"]}))
	if app.lists[1].Offset != 1 {
		t.Fatalf("the second page asked for offset %d", app.lists[1].Offset)
	}
	_ = next
}

// TestStatusClipsALongAnswerAndOffersTheRestOfIt.
func TestStatusClipsALongAnswerAndOffersTheRestOfIt(t *testing.T) {
	app := newFakeApp("Laptop")
	app.status = StatusReport{Requests: []RequestState{{Token: "tok-1", Kind: "ask", State: RequestReplied, Answer: strings.Repeat("a", 40_000)}}}
	server := New(app)

	result := call(t, server, localCaller(), "thread_status", `{"tokens":["tok-1"],"max_bytes":2048}`)
	answer := field(t, rows(t, result["requests"])[0], "answer").(string)
	if len(answer) > 2048 {
		t.Fatalf("answer is %d bytes, over the budget", len(answer))
	}
	if result["more"] != true || result["cursor"] == nil {
		t.Fatalf("a clipped answer must offer the rest: %v", result)
	}
	if note := result["note"].(string); !strings.Contains(note, "to_file") {
		t.Errorf("note = %q", note)
	}

	// The cursor continues the same answer where the page stopped.
	second := call(t, server, localCaller(), "thread_status", mustJSON(t, map[string]any{"tokens": []string{"tok-1"}, "max_bytes": 2048, "cursor": result["cursor"]}))
	if got := field(t, rows(t, second["requests"])[0], "answer").(string); len(got) == 0 || len(got) > 2048 {
		t.Fatalf("the second page carried %d bytes", len(got))
	}

	app.export = ExportFile{Path: "/data/answers/tok-1.txt", Size: 40_000}
	file := call(t, server, localCaller(), "thread_status", `{"tokens":["tok-1"],"to_file":true}`)
	if field(t, file["file"], "path") != "/data/answers/tok-1.txt" {
		t.Fatalf("file = %v", file["file"])
	}

	// An answer that fitted whole is never offered a continuation.
	app.status = StatusReport{Requests: []RequestState{{Token: "tok-2", Kind: "ask", State: RequestReplied, Answer: "short"}}}
	whole := call(t, server, localCaller(), "thread_status", `{"tokens":["tok-2"]}`)
	if whole["cursor"] != nil || whole["more"] != nil {
		t.Fatalf("a complete answer was offered a continuation: %v", whole)
	}
}

// TestStatusOnThreadsDerivesRestingFromTheState, so a row cannot report a
// state and a restedness that disagree.
func TestStatusOnThreadsDerivesRestingFromTheState(t *testing.T) {
	app := newFakeApp("Laptop")
	app.addThread(Thread{ID: localThreadID, Title: "Worker"})
	app.status = StatusReport{Threads: []ThreadState{{ThreadID: localThreadID, State: StateRunning, Resting: true}}, TimedOut: true}

	result := call(t, New(app), localCaller(), "thread_status", `{"thread_ids":["`+localThreadID+`"],"wait_seconds":30}`)
	row := rows(t, result["threads"])[0].(map[string]any)
	if row["resting"] != false {
		t.Fatalf("a running thread was reported as resting: %v", row)
	}
	if app.states[0].WaitSeconds != 30 || app.states[0].ThreadIDs[0] != localThreadID {
		t.Fatalf("status call = %+v", app.states[0])
	}
	if note, _ := result["note"].(string); !strings.Contains(note, "within the wait") {
		t.Errorf("note = %q", note)
	}
}

// TestCancelTakesExactlyOneSelector.
func TestCancelTakesExactlyOneSelector(t *testing.T) {
	app := newFakeApp("Laptop")
	app.addThread(Thread{ID: localThreadID, Title: "Worker"})
	app.addThread(Thread{ID: "caller-thread", Title: "Caller"})
	app.cancelled = CancelReport{Token: "tok-1", State: RequestCancelled, Effect: "turn_interrupted"}
	server := New(app)

	callErr(t, server, localCaller(), "thread_cancel", `{}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_cancel", `{"token":"tok-1","thread_id":"`+localThreadID+`"}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_cancel", `{"thread_id":"caller-thread"}`, CodeInvalidRequest)

	result := call(t, server, localCaller(), "thread_cancel", `{"token":"tok-1"}`)
	if app.cancels[0].Token != "tok-1" || app.cancels[0].ThreadID != "" {
		t.Fatalf("cancel = %+v", app.cancels[0])
	}
	if result["effect"] != "turn_interrupted" {
		t.Fatalf("result = %v", result)
	}
	if note := result["note"].(string); !strings.Contains(note, "did not undo anything") {
		t.Errorf("note = %q", note)
	}

	call(t, server, localCaller(), "thread_cancel", `{"thread_id":"`+localThreadID+`"}`)
	if app.cancels[1].ThreadID != localThreadID {
		t.Fatalf("cancel = %+v", app.cancels[1])
	}
}

// TestRemindTakesExactlyOneClockAndResolvesItAgainstNow.
func TestRemindTakesExactlyOneClockAndResolvesItAgainstNow(t *testing.T) {
	app := newFakeApp("Laptop")
	app.ack = RequestAck{Token: "tok-r", Kind: "remind", State: RequestAccepted, Outcome: OutcomeBackgrounded}
	server := New(app)
	fixed := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	server.now = func() time.Time { return fixed }

	result := call(t, server, localCaller(), "thread_remind", `{"after_seconds":600,"note":"check the deploy"}`)
	if app.rem[0].DueAtUnixMs != fixed.Add(10*time.Minute).UnixMilli() || app.rem[0].Note != "check the deploy" {
		t.Fatalf("remind = %+v", app.rem[0])
	}
	if note := result["note"].(string); !strings.Contains(note, "2026-09-19T12:10:00Z") || !strings.Contains(note, "thread_cancel token tok-r") {
		t.Errorf("note = %q", note)
	}

	call(t, server, localCaller(), "thread_remind", `{"at":"2026-09-20T09:00:00Z","note":"standup"}`)
	if app.rem[1].DueAtUnixMs != time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("at did not parse: %d", app.rem[1].DueAtUnixMs)
	}

	callErr(t, server, localCaller(), "thread_remind", `{"note":"soon"}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_remind", `{"after_seconds":60,"at":"2026-09-20T09:00:00Z","note":"soon"}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_remind", `{"after_seconds":60}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_remind", `{"at":"tomorrow","note":"soon"}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_remind", `{"after_seconds":60,"note":"`+strings.Repeat("n", MaxNoteRunes+1)+`"}`, CodeInvalidRequest)
}

// TestUpdateValidatesThePatchOnceBeforeTouchingAnything.
func TestUpdateValidatesThePatchOnceBeforeTouchingAnything(t *testing.T) {
	app := newFakeApp("Laptop")
	app.addThread(Thread{ID: localThreadID, Title: "One"})
	server := New(app)

	callErr(t, server, localCaller(), "thread_update", `{"thread_ids":["`+localThreadID+`"]}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_update", `{"thread_ids":["`+localThreadID+`"],"title":"   "}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_update", `{"thread_ids":["`+localThreadID+`"],"pin":"middle"}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_update", `{"thread_ids":[],"archived":true}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_update", `{"thread_ids":["`+localThreadID+`"],"group":"  "}`, CodeInvalidRequest)
	message := callErr(t, server, localCaller(), "thread_update", `{"thread_ids":["`+localThreadID+`"],"group":"Release","pin":"front"}`, CodeGrouped)
	if !strings.Contains(message, "its group carries it") {
		t.Errorf("message = %q", message)
	}
	if len(app.updates) != 0 {
		t.Fatal("a refused patch reached the app")
	}

	call(t, server, localCaller(), "thread_update", `{"thread_ids":["`+localThreadID+`"],"title":"  Renamed  ","archived":false}`)
	patch := app.updates[0]
	if patch.Title == nil || *patch.Title != "Renamed" || patch.Archived == nil || *patch.Archived {
		t.Fatalf("patch = %+v", patch)
	}
}

// TestUpdateTellsAnExplicitNullGroupFromAnAbsentOne: null ungroups,
// absent leaves the group alone.
func TestUpdateTellsAnExplicitNullGroupFromAnAbsentOne(t *testing.T) {
	app := newFakeApp("Laptop")
	app.addThread(Thread{ID: localThreadID, Title: "One"})
	server := New(app)

	call(t, server, localCaller(), "thread_update", `{"thread_ids":["`+localThreadID+`"],"group":null}`)
	if group := app.updates[0].Group; group == nil || *group != "" {
		t.Fatalf("null group = %v, want the empty ungroup marker", group)
	}
	call(t, server, localCaller(), "thread_update", `{"thread_ids":["`+localThreadID+`"],"group":"Release"}`)
	if group := app.updates[1].Group; group == nil || *group != "Release" {
		t.Fatalf("group = %v", group)
	}
	call(t, server, localCaller(), "thread_update", `{"thread_ids":["`+localThreadID+`"],"archived":true}`)
	if app.updates[2].Group != nil {
		t.Fatalf("an absent group became %v", app.updates[2].Group)
	}
}

// TestUpdateRefusesPerIdAndStillAppliesTheRest.
func TestUpdateRefusesPerIdAndStillAppliesTheRest(t *testing.T) {
	app := newFakeApp("Laptop")
	app.addThread(Thread{ID: localThreadID, Title: "One"})
	app.addThread(Thread{ID: "caller-thread", Title: "Caller"})
	server := New(app)

	result := call(t, server, localCaller(), "thread_update", `{"thread_ids":["`+localThreadID+`","caller-thread","99999999-9999-4999-8999-999999999999"],"archived":true}`)
	list := rows(t, result["results"])
	if len(list) != 3 {
		t.Fatalf("results = %v", result["results"])
	}
	if field(t, list[0], "updated") != true {
		t.Errorf("the valid thread was not updated: %v", list[0])
	}
	if field(t, list[1], "error_code") != CodeIsCaller {
		t.Errorf("a thread archiving itself was allowed: %v", list[1])
	}
	if field(t, list[2], "error_code") != CodeNotFound {
		t.Errorf("an unknown id = %v", list[2])
	}
	if note, _ := result["note"].(string); !strings.Contains(note, "left untouched") {
		t.Errorf("note = %q", note)
	}
	if ids := app.updates[0].ThreadIDs; len(ids) != 1 || ids[0] != localThreadID {
		t.Fatalf("the app was given %v, want only the thread that passed", ids)
	}
}

// TestUpdateGroupsIdsByTheComputerThatHoldsThem, so one unreachable
// computer fails only its own ids.
func TestUpdateGroupsIdsByTheComputerThatHoldsThem(t *testing.T) {
	p := newPair(t)
	p.local.addThread(Thread{ID: localThreadID, Title: "Local"})
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Remote"})

	result := call(t, p.server, localCaller(), "thread_update", `{"thread_ids":["`+localThreadID+`","`+remoteThreadID+`"],"pin":"front"}`)
	list := rows(t, result["results"])
	if len(list) != 2 || field(t, list[0], "updated") != true || field(t, list[1], "updated") != true {
		t.Fatalf("results = %v", result["results"])
	}
	if field(t, list[1], "computer_id") != "studio" {
		t.Fatalf("a remote row is not stamped: %v", list[1])
	}
	if len(p.local.updates) != 1 || len(p.remote.updates) != 1 {
		t.Fatalf("calls: local %d, remote %d", len(p.local.updates), len(p.remote.updates))
	}
	if ids := p.remote.updates[0].ThreadIDs; len(ids) != 1 || ids[0] != remoteThreadID {
		t.Fatalf("the destination received %v", ids)
	}
	if pin := p.remote.updates[0].Pin; pin == nil || *pin != PinFront {
		t.Fatalf("the patch did not survive forwarding: %v", pin)
	}
	if len(p.peer.invokes) != 1 || p.peer.invokes[0] != "thread_update" {
		t.Fatalf("invokes = %v", p.peer.invokes)
	}
}

// TestGroupNamesOneGroupAndDoesOneThingToIt.
func TestGroupNamesOneGroupAndDoesOneThingToIt(t *testing.T) {
	app := newFakeApp("Laptop")
	app.groupRes = GroupReport{GroupID: "g1", Group: "Release", Action: "renamed"}
	server := New(app)

	callErr(t, server, localCaller(), "thread_group", `{"rename":"Later"}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_group", `{"group":"Release","group_id":"g1","rename":"Later"}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_group", `{"group_id":"g1"}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_group", `{"group_id":"g1","rename":"Later","delete":true}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_group", `{"group_id":"g1","pin":"sideways"}`, CodeInvalidRequest)
	if len(app.gcalls) != 0 {
		t.Fatal("a refused group call reached the app")
	}

	result := call(t, server, localCaller(), "thread_group", `{"group_id":"g1","rename":"Later"}`)
	if app.gcalls[0].GroupID != "g1" || app.gcalls[0].Rename != "Later" {
		t.Fatalf("group call = %+v", app.gcalls[0])
	}
	if result["action"] != "renamed" || result["group"] != "Release" {
		t.Fatalf("result = %v", result)
	}

	app.groupRes = GroupReport{GroupID: "g1", Group: "Release", Action: "deleted", Ungrouped: 4}
	deleted := call(t, server, localCaller(), "thread_group", `{"group":"Release","project_id":"p1","delete":true}`)
	if note := deleted["note"].(string); !strings.Contains(note, "ungrouped, not deleted") {
		t.Errorf("note = %q", note)
	}
	if deleted["ungrouped"] != float64(4) {
		t.Errorf("ungrouped = %v", deleted["ungrouped"])
	}
}

// TestGroupOnAnotherComputerRunsThere.
func TestGroupOnAnotherComputerRunsThere(t *testing.T) {
	p := newPair(t)
	p.remote.groupRes = GroupReport{GroupID: "g9", Group: "Release", Action: "pinned", Pin: PinFront}

	result := call(t, p.server, localCaller(), "thread_group", `{"group_id":"g9","pin":"front","computer_id":"studio"}`)
	if len(p.remote.gcalls) != 1 || p.remote.gcalls[0].Pin != PinFront {
		t.Fatalf("the destination received %+v", p.remote.gcalls)
	}
	if result["computer_id"] != "studio" || result["computer"] != "Studio" {
		t.Fatalf("result = %v", result)
	}
}

// TestLocalDestinationIsEmptyWhileComputersArePaired: in the paired shape
// every resolved row carries the computer that owns it, this one included.
// A send, ask or cancel against a thread on the caller's own computer must
// still reach the App with no destination, because a computer is not paired
// with itself and asking for a peer by its own id names nothing.
func TestLocalDestinationIsEmptyWhileComputersArePaired(t *testing.T) {
	p := newPair(t)
	p.local.addThread(Thread{ID: "caller-thread", Title: "Caller", Provider: "claude"})
	p.local.addThread(Thread{ID: localThreadID, Title: "Worker"})
	p.local.ack = RequestAck{Token: "tok-1", ThreadID: localThreadID, State: StateRunning, Outcome: OutcomeBackgrounded}
	p.local.cancelled = CancelReport{Token: "tok-1", ThreadID: localThreadID, State: RequestCancelled, Effect: "turn_interrupted"}

	target, err := p.session().resolve(context.Background(), localThreadID, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !target.Local || target.ComputerID != "laptop" {
		t.Fatalf("target = %+v, want the local thread stamped with this computer", target)
	}
	if destination := target.Destination(); destination != "" {
		t.Fatalf("destination = %q, want none for a thread on this computer", destination)
	}

	call(t, p.server, localCaller(), "thread_send", `{"thread_id":"`+localThreadID+`","message":"go"}`)
	if len(p.local.sends) != 1 || p.local.sends[0].ComputerID != "" {
		t.Fatalf("send = %+v, want no destination", p.local.sends)
	}
	call(t, p.server, localCaller(), "thread_ask", `{"thread_id":"`+localThreadID+`","question":"status?"}`)
	if len(p.local.asks) != 1 || p.local.asks[0].ComputerID != "" {
		t.Fatalf("ask = %+v, want no destination", p.local.asks)
	}
	call(t, p.server, localCaller(), "thread_cancel", `{"thread_id":"`+localThreadID+`"}`)
	if len(p.local.cancels) != 1 || p.local.cancels[0].ComputerID != "" {
		t.Fatalf("cancel = %+v, want no destination", p.local.cancels)
	}

	// A fork of a local thread runs here too, however the row names this
	// computer.
	call(t, p.server, localCaller(), "thread_spawn", `{"prompt":"continue","from_thread":"`+localThreadID+`"}`)
	if len(p.local.spawns) != 1 || p.local.spawns[0].ComputerID != "" {
		t.Fatalf("spawn = %+v, want no destination", p.local.spawns)
	}

	// The destination of a thread on the paired computer is unchanged.
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Remote"})
	call(t, p.server, localCaller(), "thread_send", `{"thread_id":"`+remoteThreadID+`","message":"go"}`)
	if len(p.local.sends) != 2 || p.local.sends[1].ComputerID != "studio" {
		t.Fatalf("remote send = %+v, want the paired computer", p.local.sends)
	}
}
