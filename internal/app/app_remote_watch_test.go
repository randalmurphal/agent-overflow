package app

import (
	"agent-overflow/internal/testutil/mockexec"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"github.com/google/uuid"
)

func remoteWatchThread(t *testing.T, a *App, name string) store.Thread {
	t.Helper()
	project, err := a.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "source", Path: initGitRepo(t)})
	if err != nil {
		t.Fatal(err)
	}
	thread := testThread(uuid.NewString())
	thread.ProjectID = project.ID
	thread.ProjectPath = project.Path
	thread.WorkspacePath = project.Path
	thread.Provider = name
	if err := a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	return thread
}

func registeredRemoteWatch(t *testing.T, a *App, thread store.Thread) store.RemoteWatch {
	t.Helper()
	w := store.RemoteWatch{ComputerID: uuid.NewString(), RequestID: uuid.NewString(), ThreadID: thread.ID, Fingerprint: strings.Repeat("a", 64), Label: "build --cross-platform"}
	if _, err := a.store.RegisterRemoteWatch(w); err != nil {
		t.Fatal(err)
	}
	return w
}

func completedRemoteWatch(t *testing.T, a *App, thread store.Thread) store.RemoteWatch {
	t.Helper()
	w := registeredRemoteWatch(t, a, thread)
	w.Receipt = store.RemoteJob{ID: w.RequestID, SourceThreadID: thread.ID, State: "succeeded", Workspace: "/destination/worktree", FinishedAt: time.Now().UnixMilli(), Output: "build passed"}
	if err := a.store.ObserveRemoteWatch(w.ComputerID, w.RequestID, w.Receipt, "", 0); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestRemoteCompletionQueueHandoffDraftAndRestartRecovery(t *testing.T) {
	t.Parallel()
	a, rec := newAppForFlushQueueRPC(t)
	a.startSessionFn = func(string) error { return nil }
	thread := remoteWatchThread(t, a, string(provider.Claude))
	w := completedRemoteWatch(t, a, thread)
	if _, err := a.store.UpsertThreadDraft(store.ThreadDraft{ThreadID: thread.ID, Content: "my unsent draft", Attachments: "[]", TerminalChips: "[]", UpdatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := a.deliverRemoteCompletion(w, remoteCompletionOutput{Tail: w.Receipt.Output}); err != nil {
			t.Fatal(err)
		}
	}
	rows := durableQueueRows(t, a, thread.ID)
	if len(rows) != 1 || rows[0].SendID != remoteCompletionSendID(w) {
		t.Fatalf("duplicate or unstable queue: %+v", rows)
	}
	state := rec.lastQueueState(t)
	if len(state.Items) != 1 || state.Items[0].SendID != rows[0].SendID {
		t.Fatalf("completion was not ordinary visible queue state: %+v", state)
	}
	draft, _, err := a.store.GetThreadDraft(thread.ID)
	if err != nil || draft.Content != "my unsent draft" {
		t.Fatalf("draft overwritten: %+v %v", draft, err)
	}
	saved, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID)
	if err != nil || saved.Notification != "queued" {
		t.Fatalf("handoff=%+v %v", saved, err)
	}
	// Replace process-memory state and run the actual boot recovery: queued
	// completions follow ordinary messages into the draft, never auto-resend.
	a.triage = triage.NewRouter(a.store, rec.captureChannel)
	a.configureTriageQueueCallbacks()
	a.restoreDurableFlushQueueAtBoot()
	draft, _, err = a.store.GetThreadDraft(thread.ID)
	if err != nil || strings.Count(draft.Content, w.RequestID) != 1 || !strings.HasSuffix(draft.Content, "my unsent draft") {
		t.Fatalf("recovered draft=%+v %v", draft, err)
	}
	if len(durableQueueRows(t, a, thread.ID)) != 0 {
		t.Fatal("recovered completion remained queued")
	}
	due, err := a.store.ListRemoteWatches("", time.Now().Add(time.Hour).UnixMilli(), 256)
	if err != nil || len(due) != 0 {
		t.Fatalf("restart would re-notify recovered message: %+v %v", due, err)
	}
	// A stale in-memory observer is also unable to insert a second message after
	// recovery removed the normal queue row and its send identity: the durable
	// watch no longer owes a notification.
	_ = a.deliverRemoteCompletion(w, remoteCompletionOutput{Tail: w.Receipt.Output})
	if len(durableQueueRows(t, a, thread.ID)) != 0 {
		t.Fatal("stale observer re-enqueued recovered completion")
	}
}

func TestRemoteCompletionUsesBusyProviderQueueForBothProviders(t *testing.T) {
	t.Parallel()
	for _, name := range []string{string(provider.Claude), string(provider.Codex)} {
		t.Run(name, func(t *testing.T) {
			a, rec := newAppForFlushQueueRPC(t)
			thread := remoteWatchThread(t, a, name)
			capture := filepath.Join(t.TempDir(), "provider.ndjson")
			if name == string(provider.Claude) {
				installCapturingClaudeSession(t, a, thread, capture)
			} else {
				sess := installSteerTestSession(t, a, thread, "ok")
				a.sessionManager().put(thread.ID, session{Provider: name, Token: "remote-completion-test", Codex: sess})
			}
			if err := a.triage.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: thread.ID, TurnIndex: 0, Timestamp: time.Now()}); err != nil {
				t.Fatal(err)
			}
			w := completedRemoteWatch(t, a, thread)
			if err := a.deliverRemoteCompletion(w, remoteCompletionOutput{Tail: w.Receipt.Output}); err != nil {
				t.Fatal(err)
			}
			flushed := waitForAtLeastQueueFlushed(t, rec, 1)
			if len(flushed) != 1 || len(flushed[0].Items) != 1 || !strings.Contains(flushed[0].Items[0].Message, w.RequestID) {
				t.Fatalf("normal provider handoff missing: %+v", flushed)
			}
			if name == string(provider.Claude) {
				texts := waitForCapturedUserMessages(t, capture, 1)
				if len(texts) != 1 || !strings.Contains(texts[0], "build passed") {
					t.Fatalf("provider input=%q", texts)
				}
			}
			if err := a.deliverRemoteCompletion(w, remoteCompletionOutput{Tail: w.Receipt.Output}); err != nil {
				t.Fatal(err)
			}
			if count := len(waitForAtLeastQueueFlushed(t, rec, 1)); count != 1 {
				t.Fatalf("repeat notification dispatched %d times", count)
			}
			saved, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID)
			if err != nil || saved.Notification != "queued" {
				t.Fatalf("notification responsibility=%+v %v", saved, err)
			}
			// Keep the normal provider echo as the source of timeline confirmation.
			item := flushed[0].Items[0]
			meta, _ := json.Marshal(map[string]any{"provider_item_id": "remote-notice-echo", "client_id": item.UserItemID})
			if err := a.triage.Handle(provider.ProviderEvent{Kind: provider.EventUserText, ThreadID: thread.ID, TurnIndex: 0, ItemID: item.UserItemID, Meta: meta, Content: item.Message, Timestamp: time.Now()}); err != nil {
				t.Fatal(err)
			}
			row, found, err := a.store.GetThreadItem(thread.ID, item.UserItemID)
			if err != nil || !found || row.Kind != "user_text" || row.Role != "user" || row.Summary != item.Message {
				t.Fatalf("ordinary timeline confirmation=%+v found=%v err=%v", row, found, err)
			}
		})
	}
}

func TestRemoteWatchPairedCompletionStartsIdleAgentAndRespectsThreadOwnership(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	destination := newPairedBackend(t)
	source, rec := newAppForFlushQueueRPC(t)
	peers, err := attachedbackends.New(t.TempDir(), "source", "test")
	if err != nil {
		t.Fatal(err)
	}
	source.backends = peers
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	invite, _ := destination.mintLink(t, "full")
	peer, err := peers.Add(ctx, invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := destination.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err := peers.Await(ctx, peer.ID); err != nil {
		t.Fatal(err)
	}
	if err := source.SetAgentComputerEnabled(ctx, peer.ID, true); err != nil {
		t.Fatal(err)
	}
	project, err := destination.app.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "destination", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var executions atomic.Int32
	destination.app.remoteJobs, err = remotejobs.New(ctx, destination.app.store, func(_ context.Context, _ string, argv []string, out io.Writer) (remotejobs.Outcome, error) {
		if _, err := source.store.GetRemoteWatch(peer.ID, argv[1]); err != nil {
			t.Error("destination executed before source registered durable notification", err)
		}
		executions.Add(1)
		_, _ = io.WriteString(out, "remote integration passed")
		return remotejobs.Outcome{ExitCode: 0}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer destination.app.remoteJobs.Close()
	capture := filepath.Join(t.TempDir(), "idle.ndjson")
	if _, err := source.settings.Update(map[string]any{"claudeBinaryPath": writeStdinCapturingClaudeBinary(t, capture)}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"idle", "idle-codex", "archived", "deleted", "transferring", "finished-phase", "finished-run", "orphan-phase"} {
		t.Run(mode, func(t *testing.T) {
			name := string(provider.Claude)
			if mode == "idle-codex" {
				name = string(provider.Codex)
			}
			thread := remoteWatchThread(t, source, name)
			codexCapture := filepath.Join(t.TempDir(), "codex-idle.ndjson")
			if mode == "idle-codex" {
				binary := writeCodexSteerBinary(t, thread.ID+"-provider", "no-active-turn")
				script := []byte(mockScript(t, binary))
				script = []byte(strings.Replace(string(script), "while IFS= read -r line; do", "while IFS= read -r line; do\n    printf '%s\\n' \"$line\" >> "+shellQuote(codexCapture), 1))
				mockexec.Write(t, binary, string(script))
				if _, err := source.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
					t.Fatal(err)
				}
			}
			defer source.StopSession(thread.ID)
			id := uuid.NewString()
			caller := transport.WithCallerScope(ctx, transport.CallerScope{Kind: transport.ScopeKindInteractive, ThreadID: thread.ID, ProjectID: thread.ProjectID})
			request := AgentRemoteRequest{ComputerID: peer.ID, Workspace: gitapp.WorkspaceRef{ProjectID: project.ID}, Request: remotejobs.Request{ID: id, Argv: []string{"test-helper", id}, TimeoutSeconds: 60}}
			if _, err := source.AgentRemoteStart(caller, request); err != nil {
				t.Fatal(err)
			}
			// Wait on the fixture's job before reading it over the paired
			// transport. Millisecond RPC polling spends an auth ticket each
			// time and can exhaust the shared peer's rate limit across cases.
			deadline := time.Now().Add(5 * time.Second)
			for {
				receipt, err := destination.app.store.GetRemoteJob(id)
				if err != nil {
					t.Fatal(err)
				}
				if receipt.State != "running" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("job did not finish")
				}
				time.Sleep(time.Millisecond)
			}
			receipt, err := source.AgentRemoteStatus(caller, peer.ID, id)
			if err != nil || receipt.State != "succeeded" || receipt.Output != "remote integration passed" {
				t.Fatalf("paired completion receipt=%+v err=%v", receipt, err)
			}
			switch mode {
			case "archived":
				thread.Archived = true
				if err := source.store.UpdateThread(thread); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if err := source.store.DeleteThread(thread.ID); err != nil {
					t.Fatal(err)
				}
			case "finished-phase", "finished-run", "orphan-phase":
				thread.Mode = threadmode.ModeWorkflow
				if err := source.store.UpdateThread(thread); err != nil {
					t.Fatal(err)
				}
				if mode == "finished-phase" || mode == "finished-run" {
					snapshot, _ := json.Marshal(engine.Snapshot{Workflow: def.Workflow{ID: "remote-wf", Phases: []def.Phase{{ID: "build", Driver: def.DriverAgent, Grants: []string{"remote-commands"}}}}})
					item := store.WorkItem{ID: uuid.NewString(), ProjectID: thread.ProjectID, Goal: "build", WorkflowID: "remote-wf", WorkflowScope: "project", Snapshot: snapshot, State: string(engine.StateRunning), Source: "manual", CreatedAt: time.Now().UnixMilli()}
					if mode == "finished-run" {
						item.State = string(engine.StateDone)
					}
					if err := source.store.CreateWorkItem(item); err != nil {
						t.Fatal(err)
					}
					phase := store.WorkItemPhase{ItemID: item.ID, PhaseID: "build", Attempt: 1, ThreadID: thread.ID, Status: "completed", StartedAt: 1, EndedAt: 2}
					if mode == "finished-run" {
						phase.Status = "running"
						phase.EndedAt = 0
					}
					if err := source.store.CreateWorkItemPhase(phase); err != nil {
						t.Fatal(err)
					}
				}
			case "transferring":
				_, err := source.store.CreateThreadTransfer(store.ThreadTransfer{ID: uuid.NewString(), ThreadID: thread.ID, PeerBackendID: uuid.NewString(), Kind: "move", Direction: "outgoing", ActivationHash: strings.Repeat("b", 64), PrivateState: json.RawMessage(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
			}
			watch, err := source.store.GetRemoteWatch(peer.ID, id)
			if err != nil {
				t.Fatal(err)
			}
			wantFlushed := len(emittedQueueFlushed(rec)) + 1
			source.checkRemoteWatch(watch)
			if mode == "idle" || mode == "idle-codex" {
				if mode == "idle" {
					texts := waitForCapturedUserMessages(t, capture, 1)
					if len(texts) != 1 || !strings.Contains(texts[0], id) {
						t.Fatalf("idle startup input=%q", texts)
					}
				} else {
					waitForCaptured(t, codexCapture, `"method":"turn/start"`, "idle Codex completion did not start a turn")
					found := 0
					for _, frame := range capturedFrames(t, codexCapture) {
						if strings.Contains(frame, `"method":"turn/start"`) {
							found++
							if !strings.Contains(frame, id) {
								t.Fatalf("Codex start omitted completion: %s", frame)
							}
						}
					}
					if found != 1 {
						t.Fatalf("Codex turn starts=%d", found)
					}
				}
				waitForAtLeastQueueFlushed(t, rec, wantFlushed)
				source.checkRemoteWatch(watch)
				count := 0
				for _, event := range emittedQueueFlushed(rec) {
					if event.ThreadID == thread.ID {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("duplicate observer delivered %d messages", count)
				}
			} else {
				saved, err := source.store.GetRemoteWatch(peer.ID, id)
				if err != nil {
					t.Fatal(err)
				}
				if saved.Notification == "queued" || len(durableQueueRows(t, source, thread.ID)) != 0 {
					t.Fatalf("%s thread received notification: %+v", mode, saved)
				}
				if _, live := source.sessionManager().get(thread.ID); live {
					t.Fatalf("%s thread restarted", mode)
				}
			}
		})
	}
	if executions.Load() != 8 {
		t.Fatalf("destination executions=%d", executions.Load())
	}
}

func TestRemoteOnlyWorkRemainsVisibleUntilLifecycleCancelsIt(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	a.startSessionFn = func(string) error { return nil }
	thread := remoteWatchThread(t, a, string(provider.Codex))
	watch := registeredRemoteWatch(t, a, thread)
	watch.Receipt = store.RemoteJob{ID: watch.RequestID, SourceThreadID: thread.ID, State: "running", Workspace: thread.WorkspacePath, StartedAt: time.Now().UnixMilli()}
	if err := a.store.ObserveRemoteWatch(watch.ComputerID, watch.RequestID, watch.Receipt, "", 0); err != nil {
		t.Fatal(err)
	}
	if _, live := a.sessionManager().get(thread.ID); live {
		t.Fatal("fixture unexpectedly has provider session")
	}
	inventory, err := a.ListRunningBackgroundWork()
	if err != nil || len(inventory.Rows) != 1 {
		t.Fatalf("remote-only inventory=%+v err=%v", inventory, err)
	}
	row := inventory.Rows[0]
	if row.ThreadID != thread.ID || row.Kind != BackgroundWorkRemoteCommand || row.Provider != thread.Provider || !strings.Contains(row.StopID, watch.RequestID) {
		t.Fatalf("remote-only attribution=%+v", row)
	}
	if err := a.checkTransferIdle(thread); err == nil || !strings.Contains(err.Error(), "remote commands") {
		t.Fatalf("copy ignored remote job: %v", err)
	}
	// Archiving cancels the job and drops the watch; the computer is not
	// reachable here, so the cancel is logged and the destination's owner
	// grace ends the job.
	if err := a.ArchiveThread(thread.ID); err != nil {
		t.Fatal(err)
	}
	inventory, err = a.ListRunningBackgroundWork()
	if err != nil || len(inventory.Rows) != 0 {
		t.Fatalf("archived conversation kept its remote command: %+v %v", inventory, err)
	}
	if saved, err := a.store.GetRemoteWatch(watch.ComputerID, watch.RequestID); err != nil || saved.Notification != "dismissed" {
		t.Fatalf("archive left the watch pending: %+v %v", saved, err)
	}
	// A delete stops the conversation's job and removes its row.
	deleted := remoteWatchThread(t, a, string(provider.Codex))
	deletedWatch := registeredRemoteWatch(t, a, deleted)
	if err := a.store.ObserveRemoteWatch(deletedWatch.ComputerID, deletedWatch.RequestID, store.RemoteJob{ID: deletedWatch.RequestID, SourceThreadID: deleted.ID, State: "running"}, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteThread(deleted.ID); err != nil {
		t.Fatalf("deletion refused remote job: %v", err)
	}
	if _, err := a.store.GetThread(deleted.ID); err == nil {
		t.Fatalf("thread %s survived deletion", deleted.ID)
	}
	if saved, err := a.store.GetRemoteWatch(deletedWatch.ComputerID, deletedWatch.RequestID); err != nil || saved.Notification != "dismissed" {
		t.Fatalf("delete left the watch pending: %+v %v", saved, err)
	}
	// A finished job whose completion has not reached the conversation still
	// blocks a copy until the ordinary queue owns the message.
	thread = remoteWatchThread(t, a, string(provider.Codex))
	watch = registeredRemoteWatch(t, a, thread)
	watch.Receipt = store.RemoteJob{ID: watch.RequestID, SourceThreadID: thread.ID, State: "succeeded", Workspace: thread.WorkspacePath, FinishedAt: time.Now().UnixMilli()}
	if err := a.store.ObserveRemoteWatch(watch.ComputerID, watch.RequestID, watch.Receipt, "", 0); err != nil {
		t.Fatal(err)
	}
	inventory, err = a.ListRunningBackgroundWork()
	if err != nil || len(inventory.Rows) != 0 {
		t.Fatalf("finished job remained running: %+v %v", inventory, err)
	}
	if err := a.checkTransferIdle(thread); err == nil {
		t.Fatal("transfer orphaned pending completion")
	}
	if err := a.deliverRemoteCompletion(watch, remoteCompletionOutput{Tail: watch.Receipt.Output}); err != nil {
		t.Fatal(err)
	}
	if pending, err := a.store.HasPendingRemoteWatches(thread.ID); err != nil || pending {
		t.Fatalf("ordinary queue did not assume ownership: %v %v", pending, err)
	}
	if err := a.checkTransferIdle(thread); err == nil || !strings.Contains(err.Error(), "queued work") {
		t.Fatalf("normal queue transfer guard not retained: %v", err)
	}
}

// A tray row is background work once no tool call waits on its job: the
// transcript row is the running one until then. The record a client names
// jobs and computers by carries the label, the command and the computer's
// name, never only ids.
func TestRemoteTrayRowIsBackgroundedOnlyOnceTheWaitEnds(t *testing.T) {
	t.Parallel()
	a, rec := newAppForFlushQueueRPC(t)
	thread := remoteWatchThread(t, a, string(provider.Codex))
	watch := registeredRemoteWatch(t, a, thread)
	receipt := store.RemoteJob{ID: watch.RequestID, SourceThreadID: thread.ID, State: "running", StartedAt: time.Now().UnixMilli()}
	if err := a.store.ObserveRemoteWatch(watch.ComputerID, watch.RequestID, receipt, "", 0); err != nil {
		t.Fatal(err)
	}
	_, endWait := a.beginRemoteWait(context.Background(), thread.ID, watch.ComputerID, watch.RequestID)
	items, err := a.remoteTrayItems(thread.ID, 0)
	if err != nil || len(items) != 1 || items[0].IsBackground {
		t.Fatalf("waiting job projected as backgrounded: %+v %v", items, err)
	}
	endWait()
	items, err = a.remoteTrayItems(thread.ID, 0)
	if err != nil || len(items) != 1 || !items[0].IsBackground {
		t.Fatalf("job with no wait not backgrounded: %+v %v", items, err)
	}
	_ = rec
	records, err := a.ListThreadRemoteCommands(thread.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("records: %+v %v", records, err)
	}
	record := records[0]
	if record.ComputerID != watch.ComputerID || record.RequestID != watch.RequestID || record.ThreadID != thread.ID || record.Label != watch.Label || record.Receipt.State != "running" || record.Notification != "pending" {
		t.Fatalf("record: %+v", record)
	}
}

// Archiving a thread whose start failed takes its finished job's queued
// completion back to the job's watch: the next start delivers nothing, the
// watcher leaves it alone while the thread is archived, and once the thread
// is unarchived the watcher queues it again, once.
func TestArchiveReturnsAHeldRemoteCompletionToItsWatch(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	var failing atomic.Bool
	failing.Store(true)
	a.startSessionFn = func(string) error {
		if failing.Load() {
			return errors.New("the provider binary is missing")
		}
		return nil
	}
	thread := remoteWatchThread(t, a, string(provider.Claude))
	w := completedRemoteWatch(t, a, thread)
	notification := func() string {
		t.Helper()
		saved, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		return saved.Notification
	}
	queued := func() []string {
		t.Helper()
		durable, memory := queuedSendIDs(t, a, thread.ID)
		if !slices.Equal(durable, memory) {
			t.Fatalf("durable queue %q and in-memory queue %q differ", durable, memory)
		}
		return durable
	}
	// One pass of the watcher: what is due, checked.
	watch := func() {
		t.Helper()
		due, err := a.store.ListRemoteWatches("", time.Now().Add(time.Hour).UnixMilli(), 32)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range due {
			a.checkRemoteWatch(row)
		}
		a.waitAgentNoticeWork()
	}

	watch()
	if got := queued(); !slices.Equal(got, []string{remoteCompletionSendID(w)}) || notification() != "queued" {
		t.Fatalf("held behind the failed start: queue %q, watch %q", got, notification())
	}
	if err := a.ArchiveThread(thread.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	if got := queued(); len(got) != 0 || notification() != "pending" {
		t.Fatalf("after the archive: queue %q, watch %q, want an empty queue and a pending watch", got, notification())
	}

	// Archived, it is neither due nor delivered by a check that read it
	// before the archive.
	watch()
	held, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	a.checkRemoteWatch(held)
	a.waitAgentNoticeWork()
	if got := queued(); len(got) != 0 || notification() != "pending" {
		t.Fatalf("while archived: queue %q, watch %q, want an empty queue and a pending watch", got, notification())
	}

	failing.Store(false)
	if _, err := a.UnarchiveThread(thread.ID); err != nil {
		t.Fatalf("UnarchiveThread: %v", err)
	}
	watch()
	watch()
	if got := queued(); !slices.Equal(got, []string{remoteCompletionSendID(w)}) || notification() != "queued" {
		t.Fatalf("after the unarchive: queue %q, watch %q, want the completion queued once", got, notification())
	}
}

// pairedRemoteSource pairs a source App with a destination whose remote jobs
// run through run, and returns the source, the destination, the
// destination's computer id on the source and a project there to run in.
func pairedRemoteSource(t *testing.T, run remotejobs.Run) (source *App, destination *pairedBackend, computerID, projectID string) {
	t.Helper()
	destination = newPairedBackend(t)
	source, _ = newAppForFlushQueueRPC(t)
	peers, err := attachedbackends.New(t.TempDir(), "source", "test")
	if err != nil {
		t.Fatal(err)
	}
	source.backends = peers
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	invite, _ := destination.mintLink(t, "full")
	peer, err := peers.Add(ctx, invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := destination.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err := peers.Await(ctx, peer.ID); err != nil {
		t.Fatal(err)
	}
	if err := source.SetAgentComputerEnabled(ctx, peer.ID, true); err != nil {
		t.Fatal(err)
	}
	project, err := destination.app.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "destination", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := remotejobs.New(context.Background(), destination.app.store, run)
	if err != nil {
		t.Fatal(err)
	}
	destination.app.remoteJobs = jobs
	t.Cleanup(jobs.Close)
	return source, destination, peer.ID, project.ID
}

// A job that finished on the destination after the watcher last checked it
// still reads as running here when its conversation is archived. The
// archive's cancel is answered with the finished receipt, so the completion
// is kept and arrives once after the unarchive; the job the archive stopped
// is dropped.
func TestArchiveKeepsAJobThatFinishedBeforeTheNextCheck(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, destination, computer, project := pairedRemoteSource(t, func(ctx context.Context, _ string, argv []string, out io.Writer) (remotejobs.Outcome, error) {
		if argv[0] == "hold" {
			<-ctx.Done()
			return remotejobs.Outcome{ExitCode: -1}, ctx.Err()
		}
		_, _ = io.WriteString(out, "nightly build passed")
		return remotejobs.Outcome{ExitCode: 0}, nil
	})
	source.startSessionFn = func(string) error { return nil }
	thread := remoteWatchThread(t, source, string(provider.Claude))
	caller := transport.WithCallerScope(context.Background(), transport.CallerScope{Kind: transport.ScopeKindInteractive, ThreadID: thread.ID, ProjectID: thread.ProjectID})
	start := func(program string) string {
		t.Helper()
		id := uuid.NewString()
		request := AgentRemoteRequest{ComputerID: computer, Workspace: gitapp.WorkspaceRef{ProjectID: project}, Request: remotejobs.Request{ID: id, Argv: []string{program, id}, TimeoutSeconds: 60}}
		if _, err := source.AgentRemoteStart(caller, request); err != nil {
			t.Fatal(err)
		}
		return id
	}
	held := start("hold")
	done := start("build")
	saved := func(id string) store.RemoteWatch {
		t.Helper()
		w, err := source.store.GetRemoteWatch(computer, id)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	queued := func() []string {
		t.Helper()
		durable, memory := queuedSendIDs(t, source, thread.ID)
		if !slices.Equal(durable, memory) {
			t.Fatalf("durable queue %q and in-memory queue %q differ", durable, memory)
		}
		return durable
	}
	watch := func() {
		t.Helper()
		due, err := source.store.ListRemoteWatches("", time.Now().Add(time.Hour).UnixMilli(), 32)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range due {
			source.checkRemoteWatch(row)
		}
		source.waitAgentNoticeWork()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		receipt, err := destination.app.store.GetRemoteJob(done)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.State == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the build did not finish on the destination: %q", receipt.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if state := saved(done).Receipt.State; state != "running" {
		t.Fatalf("the source already saw the finish (%q); the case needs a finish it has not checked", state)
	}

	if err := source.ArchiveThread(thread.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	if w := saved(done); w.Notification != "pending" || w.Receipt.State != "succeeded" {
		t.Fatalf("the finished job after the archive: watch %q, receipt %q, want pending and succeeded", w.Notification, w.Receipt.State)
	}
	if w := saved(held); w.Notification != "dismissed" {
		t.Fatalf("the running job after the archive: watch %q, want dismissed", w.Notification)
	}
	watch()
	if got := queued(); len(got) != 0 || saved(done).Notification != "pending" {
		t.Fatalf("while archived: queue %q, watch %q, want an empty queue and a pending watch", got, saved(done).Notification)
	}

	if _, err := source.UnarchiveThread(thread.ID); err != nil {
		t.Fatalf("UnarchiveThread: %v", err)
	}
	watch()
	watch()
	want := []string{remoteCompletionSendID(store.RemoteWatch{ComputerID: computer, RequestID: done})}
	if got := queued(); !slices.Equal(got, want) || saved(done).Notification != "queued" {
		t.Fatalf("after the unarchive: queue %q, watch %q, want the completion queued once", got, saved(done).Notification)
	}
}

// A completion archive kept cannot be delivered from a computer the person
// forgets. Forgetting names how many it discards, and goes once confirmed;
// a live thread's pending job still refuses it as before. Copying the
// archived thread names it as the cause.
func TestForgettingAComputerDiscardsTheCompletionsHeldForArchivedThreads(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, _, computer, _ := pairedRemoteSource(t, func(context.Context, string, []string, io.Writer) (remotejobs.Outcome, error) {
		return remotejobs.Outcome{}, nil
	})
	source.startSessionFn = func(string) error { return nil }
	watch := func(thread store.Thread, state string) store.RemoteWatch {
		t.Helper()
		w := store.RemoteWatch{ComputerID: computer, RequestID: uuid.NewString(), ThreadID: thread.ID, Fingerprint: strings.Repeat("a", 64), Label: "nightly build"}
		if _, err := source.store.RegisterRemoteWatch(w); err != nil {
			t.Fatal(err)
		}
		w.Receipt = store.RemoteJob{ID: w.RequestID, SourceThreadID: thread.ID, State: state, Output: "build passed"}
		if err := source.store.ObserveRemoteWatch(w.ComputerID, w.RequestID, w.Receipt, "", 0); err != nil {
			t.Fatal(err)
		}
		return w
	}
	notification := func(w store.RemoteWatch) string {
		t.Helper()
		saved, err := source.store.GetRemoteWatch(w.ComputerID, w.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		return saved.Notification
	}
	forgotten := func() bool {
		t.Helper()
		rows, err := source.ListBackends()
		if err != nil {
			t.Fatal(err)
		}
		return !slices.ContainsFunc(rows, func(row AttachedBackend) bool { return row.ID == computer })
	}

	archived := remoteWatchThread(t, source, string(provider.Claude))
	archived.Title = "Release notes"
	if err := source.store.UpdateThread(archived); err != nil {
		t.Fatal(err)
	}
	held := watch(archived, "succeeded")
	if err := source.ArchiveThread(archived.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	if notification(held) != "pending" {
		t.Fatalf("archive dropped the finished job: watch %q", notification(held))
	}
	archived, err := source.store.GetThread(archived.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.checkTransferIdle(archived); err == nil || err.Error() != `"Release notes" is archived and holds a finished remote job; unarchive it to receive the result first.` {
		t.Fatalf("copy refusal for the archived thread: %v", err)
	}

	live := remoteWatchThread(t, source, string(provider.Claude))
	running := watch(live, "running")
	if err := source.checkTransferIdle(live); err == nil || !strings.HasPrefix(err.Error(), "Let remote commands finish") {
		t.Fatalf("copy refusal for the live thread: %v", err)
	}
	err = source.RemoveBackend(computer, true)
	if err == nil || !strings.HasPrefix(err.Error(), "This computer has remote commands awaiting completion or confirmation.") || forgotten() {
		t.Fatalf("a live thread's running job did not refuse the forget: %v", err)
	}
	if err := source.store.DismissRemoteWatch(running.ComputerID, running.RequestID); err != nil {
		t.Fatal(err)
	}

	err = source.RemoveBackend(computer, false)
	code, message, public := errorsx.PublicDetails(err)
	if !public || code != "remote_completions_held" || message != "1 finished job held for an archived conversation will be discarded. Confirm to forget the computer anyway." {
		t.Fatalf("the unconfirmed forget: %q %q %v", code, message, err)
	}
	if forgotten() || notification(held) != "pending" {
		t.Fatalf("the refused forget acted: forgotten %v, watch %q", forgotten(), notification(held))
	}
	if err := source.RemoveBackend(computer, true); err != nil {
		t.Fatalf("the confirmed forget: %v", err)
	}
	if !forgotten() || notification(held) != "dismissed" {
		t.Fatalf("after the confirmed forget: forgotten %v, watch %q", forgotten(), notification(held))
	}
	if _, err := source.UnarchiveThread(archived.ID); err != nil {
		t.Fatal(err)
	}
	due, err := source.store.ListRemoteWatches("", time.Now().Add(time.Hour).UnixMilli(), 32)
	if err != nil || len(due) != 0 {
		t.Fatalf("a discarded completion is still due: %d, %v", len(due), err)
	}
}
