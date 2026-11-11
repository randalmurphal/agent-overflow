package app

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

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

// An archive repeated after an earlier one wrote the row but did not clean
// up finishes the cleanup.
func TestRepeatedOrganizeArchiveFinishesTheCleanup(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	spawn := f.runningRequest(t, "how big is the index?")
	if row := f.request(t, spawn.Token); !row.Notify {
		t.Fatal("the request under test is not owed a message")
	}
	// What an archive cut after its write left behind.
	if _, _, err := f.app.threadApplication().Archive(f.caller.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	archived := true
	if _, err := f.app.applyThreadOrganizePatch(t.Context(), f.caller.ID, threadapp.OrganizePatch{Archived: &archived}); err != nil {
		t.Fatalf("applyThreadOrganizePatch: %v", err)
	}
	if row := f.request(t, spawn.Token); row.Notify {
		t.Error("a repeated archive left the archived thread owed a message")
	}
}
