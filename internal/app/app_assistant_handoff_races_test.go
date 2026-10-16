package app

import (
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/attachment"
	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"

	"github.com/google/uuid"
)

// A job an assistant's thread A launched on a paired computer, after two
// handoffs (A to B to C): C reads and cancels it and the cancel reaches the
// destination, A and B are refused, and the completion lands on C once.
func TestAJobOverAPairedDestinationFollowsTwoHandOffs(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	backend := newPairedBackend(t)
	source := identityApp(t)
	manager, err := attachedbackends.New(t.TempDir(), "workhorse", "linux")
	if err != nil {
		t.Fatal(err)
	}
	source.backends = manager
	invite, _ := backend.mintLink(t, "full")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	attached, err := manager.Add(ctx, invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Await(ctx, attached.ID); err != nil {
		t.Fatal(err)
	}
	project, err := backend.app.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "GPU repo", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	var canceled atomic.Bool
	backend.app.remoteJobs, err = remotejobs.New(context.Background(), backend.app.store, func(ctx context.Context, cwd string, argv []string, out io.Writer) (remotejobs.Outcome, error) {
		close(started)
		<-ctx.Done()
		canceled.Store(true)
		return remotejobs.Outcome{ExitCode: -1}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backend.app.remoteJobs.Close)

	source.startSessionFn = func(string) error { return nil }
	a := remoteWatchThread(t, source, "claude")
	a.Model = "claude-opus-4-7"
	if err := source.store.UpdateThread(a); err != nil {
		t.Fatal(err)
	}
	testAssistant(t, source, a.ID)
	if err := source.SetAgentComputerEnabled(context.Background(), attached.ID, true); err != nil {
		t.Fatal(err)
	}
	scope := func(thread string) context.Context {
		return transport.WithCallerScope(ctx, transport.CallerScope{Kind: transport.ScopeKindInteractive, ThreadID: thread, ProjectID: a.ProjectID})
	}
	input := AgentRemoteRequest{ComputerID: attached.ID, Workspace: gitapp.WorkspaceRef{ProjectID: project.ID}, Request: remotejobs.Request{ID: uuid.NewString(), Argv: []string{"train", "--gpu"}, TimeoutSeconds: 120}}
	job, err := source.AgentRemoteStart(scope(a.ID), input)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	b, err := source.nextAssistantThread(ctx, a.ID, false)
	if err != nil {
		t.Fatalf("first handoff: %v", err)
	}
	c, err := source.nextAssistantThread(ctx, b.ID, false)
	if err != nil {
		t.Fatalf("second handoff: %v", err)
	}
	source.waitAgentNoticeWork()
	w, err := source.store.GetRemoteWatch(attached.ID, job.ID)
	if err != nil || w.ThreadID != c.ID || w.LaunchedIn != a.ID {
		t.Fatalf("watch = thread %s launched %s %v; want %s / %s", w.ThreadID, w.LaunchedIn, err, c.ID, a.ID)
	}
	for name, id := range map[string]string{"A (launcher)": a.ID, "B (past)": b.ID} {
		if _, err := source.AgentRemoteStatus(scope(id), attached.ID, job.ID); err == nil {
			t.Errorf("%s read the moved job's status", name)
		}
		if _, err := source.AgentRemoteCancel(scope(id), attached.ID, job.ID); err == nil {
			t.Errorf("%s canceled the moved job", name)
		}
	}
	if canceled.Load() {
		t.Fatal("a refused cancel reached the destination")
	}
	if got, err := source.AgentRemoteStatus(scope(c.ID), attached.ID, job.ID); err != nil || got.State != "running" {
		t.Fatalf("C's status = %+v %v", got, err)
	}
	got, err := source.AgentRemoteCancel(scope(c.ID), attached.ID, job.ID)
	if err != nil {
		t.Fatalf("C's cancel = %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for !canceled.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !canceled.Load() {
		t.Fatalf("C's cancel did not reach the destination (receipt %s)", got.State)
	}
	// The watcher delivers the finished job to C once, never to A or B.
	for i := 0; i < 50; i++ {
		cur, _ := source.store.GetRemoteWatch(attached.ID, job.ID)
		if cur.Receipt.State != "" && cur.Receipt.State != "running" {
			break
		}
		if s, err := source.AgentRemoteStatus(scope(c.ID), attached.ID, job.ID); err == nil && s.State != "running" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for range 2 {
		cur, _ := source.store.GetRemoteWatch(attached.ID, job.ID)
		source.checkRemoteWatch(cur)
		source.waitAgentNoticeWork()
	}
	final, _ := source.store.GetRemoteWatch(attached.ID, job.ID)
	count := func(id string) int {
		n := 0
		for _, row := range durableQueueRows(t, source, id) {
			if row.SendID == remoteCompletionSendID(final) {
				n++
			}
		}
		return n
	}
	if count(c.ID) != 1 || count(a.ID) != 0 || count(b.ID) != 0 || strings.Contains(final.Error, "different conversation") {
		t.Fatalf("completion rows A %d B %d C %d, error %q", count(a.ID), count(b.ID), count(c.ID), final.Error)
	}
}

// A second New Thread, Same Assistant while the first still runs is
// refused, from the old thread or from the successor the first already
// committed, so the assistant gets one new thread; the next one after it
// ends runs.
func TestASecondHandOffWhileOneRunsIsRefused(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaude(t, "ok", "ok", "ok")
	f.assistantCaller(t)
	before := threadCount(t, f.app)
	var fromOld, fromSuccessor error
	var tried atomic.Bool
	f.app.handOffCommittedForTest = func(old, successor string) {
		if tried.CompareAndSwap(false, true) {
			_, fromOld = f.app.StartNextAssistantThread(t.Context(), old)
			_, fromSuccessor = f.app.StartNextAssistantThread(t.Context(), successor)
		}
	}
	successor, err := f.app.StartNextAssistantThread(t.Context(), f.caller.ID)
	if err != nil {
		t.Fatalf("handoff: %v", err)
	}
	f.app.waitAgentNoticeWork()
	if code, _, _ := errorsx.PublicDetails(fromSuccessor); code != transport.ErrCodeAssistantHandOffRunning {
		t.Fatalf("second handoff from the successor = %v, want %s", fromSuccessor, transport.ErrCodeAssistantHandOffRunning)
	}
	if fromOld == nil {
		t.Fatal("second handoff from the old thread succeeded")
	}
	if made := threadCount(t, f.app) - before; made != 1 {
		t.Fatalf("the handoffs made %d threads, want the one successor", made)
	}
	f.app.handOffCommittedForTest = nil
	if _, err := f.app.StartNextAssistantThread(t.Context(), successor.ID); err != nil {
		t.Fatalf("handoff after the first ended: %v", err)
	}
}

// A session-loss handoff and New Thread, Same Assistant share the one
// running handoff an assistant has.
func TestAHandOffIsRefusedWhileASessionLossHandOffRuns(t *testing.T) {
	t.Parallel()
	f, threadID, _ := lostSessionAssistant(t)
	before := threadCount(t, f.app)
	var manual error
	release := holdSessionLossHandOff(t, f.app, func() {
		_, manual = f.app.StartNextAssistantThread(t.Context(), threadID)
	})
	sendFindingTheSessionGone(t, f.app, threadID, "first message")
	release()
	awaitCurrentThread(t, f.app, threadID)
	f.app.waitAgentNoticeWork()
	if code, _, _ := errorsx.PublicDetails(manual); code != transport.ErrCodeAssistantHandOffRunning {
		t.Fatalf("handoff during the session-loss one = %v, want %s", manual, transport.ErrCodeAssistantHandOffRunning)
	}
	if made := threadCount(t, f.app) - before; made != 1 {
		t.Fatalf("the handoffs made %d threads, want the one successor", made)
	}
}

// threadCount counts every thread, archived ones included.
func threadCount(t *testing.T, a *App) int {
	t.Helper()
	open, err := a.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	archived, err := a.store.ListArchivedThreads()
	if err != nil {
		t.Fatal(err)
	}
	return len(open) + len(archived)
}

// A draft move that fails (an attachment the store cannot copy) leaves the
// draft in the previous thread and moves nothing, and the successor says
// where the text is, once.
func TestAFailedDraftMoveLeavesTheDraftInPlace(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaude(t, "ok", "ok")
	f.assistantCaller(t)
	if _, err := f.app.store.UpsertThreadDraft(store.ThreadDraft{ThreadID: f.caller.ID, Content: "half-typed note", Attachments: `["no-such-attachment"]`, TerminalChips: "[]", UpdatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	successor, err := f.app.StartNextAssistantThread(t.Context(), f.caller.ID)
	if err != nil {
		t.Fatalf("handoff: %v", err)
	}
	f.app.waitAgentNoticeWork()
	assertDraftMoveFailed(t, f.app, f.caller.ID, successor.ID, "half-typed note")
}

// A draft move whose clear of the previous thread's draft fails writes
// nothing to the successor and deletes the attachment copies it made: the
// text is in one composer, the previous one, and the successor says so once.
func TestADraftMoveWhoseClearFailsMovesNothing(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaude(t, "ok", "ok")
	f.assistantCaller(t)
	attStore, err := attachment.NewStore(attachment.Config{RootDir: filepath.Join(t.TempDir(), "attachments")}, f.app.store)
	if err != nil {
		t.Fatal(err)
	}
	f.app.attachments = attStore
	att := uploadTestAttachment(t, f.app, f.caller.ID, "note.txt", "text/plain", []byte("hello"))
	if err := f.app.SaveDraft(t.Context(), f.caller.ID, "half-typed note", []string{att.ID}, nil, nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER refuse_draft_clear BEFORE DELETE ON thread_drafts WHEN OLD.thread_id = '` + f.caller.ID + `' BEGIN SELECT RAISE(ABORT, 'clear refused'); END`); err != nil {
		t.Fatal(err)
	}
	successor, err := f.app.StartNextAssistantThread(t.Context(), f.caller.ID)
	if err != nil {
		t.Fatalf("handoff: %v", err)
	}
	f.app.waitAgentNoticeWork()
	assertDraftMoveFailed(t, f.app, f.caller.ID, successor.ID, "half-typed note")
	old, _, err := f.app.store.GetThreadDraft(f.caller.ID)
	if err != nil || old.Attachments != `["`+att.ID+`"]` {
		t.Fatalf("previous draft attachments = %s %v, want the original", old.Attachments, err)
	}
	if copies, err := f.app.store.ListAttachments(successor.ID); err != nil || len(copies) != 0 {
		t.Fatalf("successor attachments = %+v %v, want the copies deleted", copies, err)
	}
}

func assertDraftMoveFailed(t *testing.T, a *App, oldID, successorID, text string) {
	t.Helper()
	old, next := draftContent(t, a, oldID), draftContent(t, a, successorID)
	items, err := a.store.ListItems(successorID)
	if err != nil {
		t.Fatal(err)
	}
	notices := 0
	for _, it := range items {
		if strings.Contains(it.Summary, "could not be moved") {
			notices++
		}
	}
	if old != text || strings.Contains(next, text) || notices != 1 {
		t.Fatalf("failed move: previous draft %q, successor draft %q, %d notices; want the text in the previous one only and one notice", old, next, notices)
	}
}

// A send of the saved draft racing the handoff: the text ends in one place,
// sent or in a composer. A send that arrives after the handoff committed is
// refused, naming where the text went.
func TestASendRacingTheDraftMoveLandsOnce(t *testing.T) {
	t.Parallel()
	for run := range 10 {
		f := newAssistantFixture(t)
		f.mockClaude(t, "ok", "ok", "ok")
		f.assistantCaller(t)
		if err := f.app.SaveDraft(t.Context(), f.caller.ID, "typed note", nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		var sendErr error
		done := make(chan struct{})
		late := run >= 5
		go func() {
			defer close(done)
			if late {
				for {
					if cur, found, _ := f.app.store.AssistantCurrentThreadOf(f.caller.ID); found && cur != f.caller.ID {
						break
					}
					time.Sleep(50 * time.Microsecond)
				}
			}
			_, sendErr = f.app.SendMessageWithOptions(t.Context(), f.caller.ID, "typed note", SendMessageOptions{SendID: uuid.NewString(), ConsumeDraft: &DraftSnapshot{Content: "typed note"}})
		}()
		if run%2 == 1 {
			time.Sleep(time.Duration(run) * 200 * time.Microsecond)
		}
		successor, err := f.app.StartNextAssistantThread(t.Context(), f.caller.ID)
		<-done
		if err != nil {
			t.Fatalf("run %d: handoff: %v", run, err)
		}
		f.app.waitAgentNoticeWork()
		sent := 0
		for _, id := range []string{f.caller.ID, successor.ID} {
			items, err := f.app.store.ListItems(id)
			if err != nil {
				t.Fatal(err)
			}
			for _, it := range items {
				if it.Role == "user" && strings.Contains(it.Summary, "typed note") {
					sent++
				}
			}
		}
		total := sent + strings.Count(draftContent(t, f.app, f.caller.ID), "typed note") + strings.Count(draftContent(t, f.app, successor.ID), "typed note")
		if total != 1 {
			t.Errorf("run %d: send err %v; the text is in %d places", run, sendErr, total)
		}
		code, message, _ := errorsx.PublicDetails(sendErr)
		if (late || sendErr != nil) && (code != transport.ErrCodeAssistantDraftMoved || message != "Your unsent text moved to "+successor.Title+"; send it from there.") {
			t.Errorf("run %d (late %v): send = %v, want the draft-moved refusal", run, late, sendErr)
		}
	}
}

// Sends racing a failed handoff's return of the carried messages: every
// carried message ends in a composer once, wherever the assistant is.
func TestSendsDuringAFailedReturnAreEachKeptOnce(t *testing.T) {
	t.Parallel()
	for run := range 20 {
		f, threadID, _ := lostSessionAssistant(t)
		agentID := assistantIDOf(t, f.app, threadID)
		if !f.app.assistantHandOffs.begin(agentID, threadID) {
			t.Fatal("begin")
		}
		sendFindingTheSessionGone(t, f.app, threadID, "first message")
		sendFindingTheSessionGone(t, f.app, threadID, "second message")
		items := f.app.assistantHandOffs.finish(agentID)
		done := make(chan struct{})
		go func() {
			f.app.returnSessionLossCarries(threadID, items)
			close(done)
		}()
		_, err := f.app.SendMessageWithOptions(t.Context(), threadID, "third message", SendMessageOptions{SendID: uuid.NewString(), ConsumeDraft: &DraftSnapshot{Content: "third message"}})
		<-done
		awaitCurrentThread(t, f.app, threadID)
		f.app.waitAgentNoticeWork()
		threads, err2 := f.app.store.ListThreads()
		if err2 != nil {
			t.Fatal(err2)
		}
		total := map[string]int{}
		for _, th := range threads {
			d := draftContent(t, f.app, th.ID)
			for _, text := range []string{"first message", "second message"} {
				total[text] += strings.Count(d, text)
			}
		}
		if total["first message"] != 1 || total["second message"] != 1 {
			code, _, _ := errorsx.PublicDetails(err)
			t.Errorf("run %d: third %q; totals %v", run, code, total)
		}
	}
}

// A queued send from the past thread of the draft the handoff moved is
// refused the same way, and nothing is queued there.
func TestAQueuedSendOfAMovedDraftIsRefused(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaude(t, "ok", "ok")
	f.assistantCaller(t)
	if err := f.app.SaveDraft(t.Context(), f.caller.ID, "typed note", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	successor, err := f.app.StartNextAssistantThread(t.Context(), f.caller.ID)
	if err != nil {
		t.Fatalf("handoff: %v", err)
	}
	f.app.waitAgentNoticeWork()
	_, err = f.app.RegisterQueueItem(t.Context(), f.caller.ID, "typed note", SendMessageOptions{SendID: uuid.NewString(), ConsumeDraft: &DraftSnapshot{Content: "typed note"}})
	if code, _, _ := errorsx.PublicDetails(err); code != transport.ErrCodeAssistantDraftMoved {
		t.Fatalf("queued send = %v, want %s", err, transport.ErrCodeAssistantDraftMoved)
	}
	if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
		t.Fatalf("past thread's queue = %+v, want nothing", rows)
	}
	if got := draftContent(t, f.app, successor.ID); got != "typed note" {
		t.Fatalf("successor's composer = %q", got)
	}
	// A send that consumes nothing goes through to the past thread.
	if _, err := f.app.RegisterQueueItem(t.Context(), f.caller.ID, "a later note", SendMessageOptions{SendID: uuid.NewString(), ConsumeDraft: &DraftSnapshot{}}); err != nil {
		t.Fatalf("queued send with nothing to consume = %v", err)
	}
}
