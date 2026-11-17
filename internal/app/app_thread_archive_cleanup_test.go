package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/triage"
)

// An agent's archive whose caller goes away once the archive is written
// still stops the commands the thread started on another computer.
func TestOrganizeArchiveCutAfterTheArchiveStillCancelsRemoteCommands(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, receiver, ctx, input := remoteAdmissionFixture(t)
	receiver.remoteJobs.Close()
	manager, err := remotejobs.New(ctx, receiver.store, func(ctx context.Context, _ string, _ []string, _ io.Writer) (remotejobs.Outcome, error) {
		<-ctx.Done()
		return remotejobs.Outcome{ExitCode: -1}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	receiver.remoteJobs = manager
	t.Cleanup(manager.Close)
	started, err := source.AgentRemoteStart(ctx, input)
	if err != nil || started.State != "running" {
		t.Fatalf("start: %+v %v", started, err)
	}

	archiveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var once sync.Once
	source.testEmitHook = func(name string, data any) {
		event, ok := data.(triage.ThreadUpdateEvent)
		id := started.SourceThreadID
		if name == string(eventchan.ThreadUpdated) && ok && (event.ID == id || (event.Thread != nil && event.Thread.ID == id)) {
			// The archive is written: the caller goes away here.
			once.Do(cancel)
		}
	}
	archived := true
	if _, err := source.applyThreadOrganizePatch(archiveCtx, started.SourceThreadID, threadapp.OrganizePatch{Archived: &archived}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if archiveCtx.Err() == nil {
		t.Fatal("the archive never announced itself, so the cut never happened")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		job, err := receiver.store.GetRemoteJob(input.Request.ID)
		if err != nil {
			t.Fatal(err)
		}
		if job.State != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the archived thread's remote command was never cancelled")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An archive stops the commands the thread had started when it committed,
// and no other: one admitted once the archive has stopped the session,
// while the archive is still finishing, keeps running, and a repeated archive of the archived thread
// stops nothing. Both doors that archive run the same capture. The test
// holds the stop before it cancels anything, so the later command is
// always running when the stop decides what to cancel.
func TestArchiveStopsOnlyTheCommandsItCaptured(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	archived := true
	for name, archive := range map[string]func(*App, context.Context, string) error{
		"ArchiveThread": func(a *App, _ context.Context, id string) error { return archiveThread(a, id) },
		"thread_update": func(a *App, ctx context.Context, id string) error {
			_, err := a.applyThreadOrganizePatch(ctx, id, threadapp.OrganizePatch{Archived: &archived})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			source, receiver, ctx, input := remoteAdmissionFixture(t)
			receiver.remoteJobs.Close()
			manager, err := remotejobs.New(ctx, receiver.store, func(ctx context.Context, _ string, _ []string, _ io.Writer) (remotejobs.Outcome, error) {
				<-ctx.Done()
				return remotejobs.Outcome{ExitCode: -1}, ctx.Err()
			})
			if err != nil {
				t.Fatal(err)
			}
			receiver.remoteJobs = manager
			t.Cleanup(manager.Close)
			first, err := source.AgentRemoteStart(ctx, input)
			if err != nil || first.State != "running" {
				t.Fatalf("start: %+v %v", first, err)
			}
			later := input
			later.Request.ID = uuid.NewString()
			threadID := first.SourceThreadID

			// An open ask the thread owns: the stop settles it before it
			// cancels the commands, and the settle lock the test holds keeps
			// the stop there until a later command is running.
			// Its scratch thread is idle, so the stop finds nothing queued
			// and nothing running there.
			caller, err := source.store.GetThread(threadID)
			if err != nil {
				t.Fatalf("GetThread: %v", err)
			}
			scratch := testThread(uuid.NewString())
			scratch.ProjectID = caller.ProjectID
			if err := source.store.CreateThread(scratch); err != nil {
				t.Fatalf("CreateThread: %v", err)
			}
			ask := newThreadRequestToken()
			if err := source.store.InsertThreadRequest(store.ThreadRequest{
				Token: ask, CallerThreadID: threadID, Kind: store.ThreadRequestAsk, State: store.ThreadRequestAccepted,
				TargetThreadID: scratch.ID,
			}); err != nil {
				t.Fatalf("InsertThreadRequest: %v", err)
			}
			if _, _, err := source.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
				Token: ask, OwnerDeviceID: threadReceiptLocalOwner, SourceThreadID: threadID, Kind: store.ThreadRequestAsk,
				TargetThreadID: scratch.ID,
			}); err != nil {
				t.Fatalf("AcceptThreadRequestReceipt: %v", err)
			}
			releaseStop := source.threadRequestSettleLock(ask)
			var once sync.Once
			committed := make(chan struct{})
			source.testEmitHook = func(name string, data any) {
				event, ok := data.(triage.ThreadUpdateEvent)
				if name == string(eventchan.ThreadUpdated) && ok && (event.ID == threadID || (event.Thread != nil && event.Thread.ID == threadID)) {
					once.Do(func() { close(committed) })
				}
			}
			archived := make(chan error, 1)
			go func() { archived <- archive(source, ctx, threadID) }()
			select {
			case <-committed:
			case <-time.After(10 * time.Second):
				releaseStop()
				t.Fatal("the archive never committed")
			}
			// The archive has committed and captured its work. The thread
			// starts nothing until its session is stopped; this command is
			// admitted after that and before the held stop cancels.
			var started RemoteCommand
			for deadline := time.Now().Add(10 * time.Second); ; {
				started, err = source.AgentRemoteStart(ctx, later)
				if !errors.Is(err, errThreadResting) || time.Now().After(deadline) {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			releaseStop()
			if err != nil || started.State != "running" {
				t.Fatalf("starting a command after the commit: %+v, %v", started, err)
			}
			if err := <-archived; err != nil {
				t.Fatalf("archive: %v", err)
			}
			waitRemoteJobState(t, receiver, input.Request.ID, func(state string) bool { return state != "running" })

			if err := archive(source, ctx, threadID); err != nil {
				t.Fatalf("repeated archive: %v", err)
			}
			time.Sleep(50 * time.Millisecond)
			if job, err := receiver.store.GetRemoteJob(later.Request.ID); err != nil || job.State != "running" {
				t.Fatalf("command admitted after the archive committed = %+v, %v; want it still running", job, err)
			}
		})
	}
}

func waitRemoteJobState(t *testing.T, receiver *remoteAdmissionReceiver, id string, done func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		job, err := receiver.store.GetRemoteJob(id)
		if err != nil {
			t.Fatal(err)
		}
		if done(job.State) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote command %s stayed %s", id, job.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A remote command whose cancel the destination does not take is named in
// what a Stop or an archive reports, and the cancel stays owed: the watcher
// sends it again until the destination takes it, which stops the command.
func TestARemoteCancelTheDestinationRefusesIsReportedAndSentAgain(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	for name, rest := range map[string]func(*App, string) string{
		"stop": func(a *App, id string) string {
			var result ThreadAgentStop
			a.restThreadHere(context.Background(), id, userRestStop(), map[string]bool{}, &result)
			return strings.Join(result.Failures, "\n")
		},
		"archive": func(a *App, id string) string {
			result, err := a.ArchiveThread(id)
			if err != nil {
				return err.Error()
			}
			return result.CleanupError
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			source, receiver, ctx, input := remoteAdmissionFixture(t)
			receiver.remoteJobs.Close()
			manager, err := remotejobs.New(ctx, receiver.store, func(ctx context.Context, _ string, _ []string, _ io.Writer) (remotejobs.Outcome, error) {
				<-ctx.Done()
				return remotejobs.Outcome{ExitCode: -1}, ctx.Err()
			})
			if err != nil {
				t.Fatal(err)
			}
			receiver.remoteJobs = manager
			t.Cleanup(manager.Close)
			started, err := source.AgentRemoteStart(ctx, input)
			if err != nil || started.State != "running" {
				t.Fatalf("start: %+v %v", started, err)
			}
			refusal := errorsx.Public("remote_not_ready", "Remote commands are not ready on this computer.", nil)
			receiver.cancelRefusal.Store(&refusal)

			if reported := rest(source, started.SourceThreadID); !strings.Contains(reported, "cancel remote command") || !strings.Contains(reported, "sent again") {
				t.Fatalf("reported = %q, want the command whose cancel failed named", reported)
			}
			watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
			if err != nil || watch.CancelOwedAt == 0 || watch.Notification != "dismissed" {
				t.Fatalf("watch = %+v, %v; want its completion dismissed and its cancel owed", watch, err)
			}
			if job, err := receiver.store.GetRemoteJob(input.Request.ID); err != nil || job.State != "running" {
				t.Fatalf("job = %+v, %v; want it still running", job, err)
			}

			source.retryOwedRemoteCancels(time.Now().Add(time.Minute))
			if watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID); err != nil || watch.CancelOwedAt == 0 {
				t.Fatalf("a refused retry settled the owed cancel: %+v, %v", watch, err)
			}
			receiver.cancelRefusal.Store(nil)
			source.retryOwedRemoteCancels(time.Now().Add(2 * time.Minute))
			waitRemoteJobState(t, receiver, input.Request.ID, func(state string) bool { return state != "running" })
			if watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID); err != nil || watch.CancelOwedAt != 0 {
				t.Fatalf("watch after the cancel landed = %+v, %v; want nothing owed", watch, err)
			}
		})
	}
}

// A status reply already on its way when a Stop dismisses a command's
// completion does not bring the completion back, whether the Stop's cancel
// landed or is owed: the watch stays dismissed, the watcher neither polls it
// nor delivers it, and nothing reaches the stopped thread.
func TestALateStatusReplyDoesNotBringBackACompletionAStopDismissed(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	for name, owed := range map[string]bool{"cancelled": false, "cancel owed": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			source, receiver, ctx, input := remoteAdmissionFixture(t)
			receiver.remoteJobs.Close()
			manager, err := remotejobs.New(ctx, receiver.store, func(ctx context.Context, _ string, _ []string, _ io.Writer) (remotejobs.Outcome, error) {
				<-ctx.Done()
				return remotejobs.Outcome{ExitCode: -1}, ctx.Err()
			})
			if err != nil {
				t.Fatal(err)
			}
			receiver.remoteJobs = manager
			t.Cleanup(manager.Close)
			// The destination starts the command and its reply is lost, so the
			// watch is pending with no receipt, for the watcher to verify.
			receiver.lostReply.Store(true)
			if _, err := source.AgentRemoteStart(ctx, input); err == nil {
				t.Fatal("the start with a lost reply succeeded")
			}
			watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
			if err != nil || watch.Notification != "pending" || watch.Receipt.ID != "" {
				t.Fatalf("watch after the lost reply = %+v, %v; want it pending with no receipt", watch, err)
			}
			if owed {
				refusal := errorsx.Public("remote_not_ready", "Remote commands are not ready on this computer.", nil)
				receiver.cancelRefusal.Store(&refusal)
			}

			threadID := watch.ThreadID
			source.remoteStatusReadForTest = func(store.RemoteWatch) {
				var stop ThreadAgentStop
				source.restThreadHere(context.Background(), threadID, userRestStop(), map[string]bool{}, &stop)
			}
			source.checkRemoteWatch(watch)
			source.remoteStatusReadForTest = nil

			after, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
			if err != nil || after.Notification != "dismissed" {
				t.Fatalf("watch after the late reply = %+v, %v; want it still dismissed", after, err)
			}
			if owed != (after.CancelOwedAt != 0) {
				t.Fatalf("watch's owed cancel = %d, want owed %v", after.CancelOwedAt, owed)
			}
			if due, err := source.store.ListRemoteWatches("", time.Now().Add(time.Hour).UnixMilli(), 32); err != nil || len(due) != 0 {
				t.Fatalf("the watcher's due list = %+v, %v; want nothing", due, err)
			}
			if queued, err := source.store.ListFlushQueueItems(threadID); err != nil || len(queued) != 0 {
				t.Fatalf("the stopped thread's queue = %+v, %v; want it empty", queued, err)
			}
		})
	}
}

// A finished command whose cancel became owed after the watcher listed it is
// not delivered as a completion until the cancel settles.
func TestACompletionWhoseCancelIsOwedIsNotDelivered(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, _, ctx, input := remoteAdmissionFixture(t)
	if _, err := source.AgentRemoteStart(ctx, input); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		w, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
		if err != nil {
			t.Fatal(err)
		}
		if w.Receipt.State == "running" {
			if _, err := source.AgentRemoteStatus(ctx, input.ComputerID, input.Request.ID); err != nil {
				t.Fatal(err)
			}
		}
		return w.Receipt.ID != "" && w.Receipt.State != "running"
	})
	listed, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
	if err != nil || listed.Notification != "pending" {
		t.Fatalf("the finished command's watch = %+v, %v; want it pending", listed, err)
	}
	if err := source.store.OweRemoteCancel(input.ComputerID, input.Request.ID, time.Now().UnixMilli(), time.Now().Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, deliver, err := source.prepareRemoteCompletion(listed, remoteCompletionOutput{Unavailable: true}); err != nil || deliver {
		t.Fatalf("prepare = %v, %v; want the completion held while its cancel is owed", deliver, err)
	}
	if err := source.store.SettleRemoteCancel(input.ComputerID, input.Request.ID); err != nil {
		t.Fatal(err)
	}
	if _, deliver, err := source.prepareRemoteCompletion(listed, remoteCompletionOutput{Unavailable: true}); err != nil || !deliver {
		t.Fatalf("prepare once the cancel settled = %v, %v; want the completion delivered", deliver, err)
	}
}

// An owed cancel the destination does not answer holds up no other job's
// completion: the watcher sends owed cancels apart from its checks.
func TestAnOwedRemoteCancelThatHangsHoldsUpNoCompletion(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, receiver, ctx, input := remoteAdmissionFixture(t)
	receiver.remoteJobs.Close()
	manager, err := remotejobs.New(ctx, receiver.store, func(context.Context, string, []string, io.Writer) (remotejobs.Outcome, error) {
		time.Sleep(300 * time.Millisecond)
		return remotejobs.Outcome{ExitCode: 0}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	receiver.remoteJobs = manager
	t.Cleanup(manager.Close)
	owed := input
	if _, err := source.AgentRemoteStart(ctx, owed); err != nil {
		t.Fatalf("start the job whose cancel is owed: %v", err)
	}
	if err := source.store.OweRemoteCancel(owed.ComputerID, owed.Request.ID, time.Now().UnixMilli(), 0); err != nil {
		t.Fatal(err)
	}
	other := input
	other.Request.ID = uuid.NewString()
	if _, err := source.AgentRemoteStart(ctx, other); err != nil {
		t.Fatalf("start the other job: %v", err)
	}

	hold := make(chan struct{})
	receiver.cancelHold.Store(&hold)
	read := make(chan struct{}, 1)
	source.remoteCompletionReadForTest = func(w store.RemoteWatch) {
		if w.RequestID == other.Request.ID {
			select {
			case read <- struct{}{}:
			default:
			}
		}
	}
	life, stop := context.WithCancel(context.Background())
	source.appCtx = life
	source.startRemoteWatches()
	t.Cleanup(func() {
		close(hold)
		stop()
		source.remoteWatchWG.Wait()
	})
	select {
	case <-read:
	case <-time.After(15 * time.Second):
		t.Fatal("the other job's completion was not read while the owed cancel hung")
	}
}
