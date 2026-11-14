package app

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/buildvariant/remotetest"
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
// and no other: one admitted after the commit, while the archive is still
// finishing, keeps running, and a repeated archive of the archived thread
// stops nothing. Both doors that archive run the same capture. The test
// holds the stop before it cancels anything, so the later command is
// always running when the stop decides what to cancel.
func TestArchiveStopsOnlyTheCommandsItCaptured(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	archived := true
	for name, archive := range map[string]func(*App, context.Context, string) error{
		"ArchiveThread": func(a *App, _ context.Context, id string) error { return a.ArchiveThread(id) },
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
			ask := newThreadRequestToken()
			if err := source.store.InsertThreadRequest(store.ThreadRequest{
				Token: ask, CallerThreadID: threadID, Kind: store.ThreadRequestAsk, State: store.ThreadRequestAccepted,
			}); err != nil {
				t.Fatalf("InsertThreadRequest: %v", err)
			}
			if _, _, err := source.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
				Token: ask, OwnerDeviceID: threadReceiptLocalOwner, SourceThreadID: threadID, Kind: store.ThreadRequestAsk,
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
			// The archive has committed and captured its work; this command
			// is admitted after that and before the held stop cancels.
			started, err := source.AgentRemoteStart(ctx, later)
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
