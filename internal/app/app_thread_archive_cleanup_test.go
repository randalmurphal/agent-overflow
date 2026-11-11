package app

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/remotejobs"
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
// stops nothing. Both doors that archive run the same capture.
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
			var once sync.Once
			laterDone := make(chan error, 1)
			source.testEmitHook = func(name string, data any) {
				event, ok := data.(triage.ThreadUpdateEvent)
				if name == string(eventchan.ThreadUpdated) && ok && (event.ID == threadID || (event.Thread != nil && event.Thread.ID == threadID)) {
					// The archive has committed; a command admitted from here
					// on is not one it stops. The start waits for the locks
					// the archive still holds, so it runs beside it.
					once.Do(func() {
						go func() {
							started, err := source.AgentRemoteStart(ctx, later)
							if err == nil && started.State != "running" {
								err = fmt.Errorf("later command = %+v", started)
							}
							laterDone <- err
						}()
					})
				}
			}
			if err := archive(source, ctx, threadID); err != nil {
				t.Fatalf("archive: %v", err)
			}
			if err := <-laterDone; err != nil {
				t.Fatalf("starting a command after the commit: %v", err)
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
