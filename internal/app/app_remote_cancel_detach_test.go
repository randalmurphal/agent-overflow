package app

import (
	"context"
	"io"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/remotejobs"
)

// Stopping a remote command from the tray is its first step, so a caller
// already gone still has the cancel delivered and its answer recorded,
// rather than a stop that may or may not have reached the destination.
func TestTrayStopDeliversTheCancelWhenItsCallerIsGone(t *testing.T) {
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

	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := source.CancelThreadRemoteCommand(gone, started.SourceThreadID, input.ComputerID, input.Request.ID); err != nil {
		t.Fatalf("tray stop with its caller gone = %v, want the cancel delivered", err)
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
			t.Fatal("the destination never received the cancel")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
