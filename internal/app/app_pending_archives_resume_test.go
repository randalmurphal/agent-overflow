package app

import (
	"context"
	"io"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
)

// resumeFixture is a thread with a command running on a paired computer,
// an open ask and an armed loop: the work an archive stops.
type resumeFixture struct {
	source   *App
	receiver *remoteAdmissionReceiver
	ctx      context.Context
	input    AgentRemoteRequest
	threadID string
	ask      string
}

func newResumeFixture(t *testing.T) resumeFixture {
	t.Helper()
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
	f := resumeFixture{source: source, receiver: receiver, ctx: ctx, input: input, threadID: started.SourceThreadID}
	f.ask = f.openAsk(t)
	armLoop(t, source, f.threadID)
	return f
}

// openAsk records an ask the thread made that is still running.
func (f resumeFixture) openAsk(t *testing.T) string {
	t.Helper()
	token := newThreadRequestToken()
	if err := f.source.store.InsertThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: f.threadID, Kind: store.ThreadRequestAsk, State: store.ThreadRequestAccepted,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.source.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: threadReceiptLocalOwner, SourceThreadID: f.threadID, Kind: store.ThreadRequestAsk,
	}); err != nil {
		t.Fatal(err)
	}
	return token
}

func (f resumeFixture) askState(t *testing.T, token string) string {
	t.Helper()
	row, found, err := f.source.store.GetThreadRequest(token)
	if err != nil || !found {
		t.Fatalf("read ask %s: %v, %v", token, found, err)
	}
	return row.State
}

func (f resumeFixture) loops(t *testing.T) int {
	t.Helper()
	wakes, err := f.source.store.ListAgentWakes(f.threadID)
	if err != nil {
		t.Fatal(err)
	}
	return len(wakes)
}

// crashAfterTheFlip leaves the store as a crash right after the archive's
// flip leaves it: archived and unfinished, with nothing of the thread's
// work stopped.
func (f resumeFixture) crashAfterTheFlip(t *testing.T) {
	t.Helper()
	if _, changed, err := f.source.store.ArchiveThread(f.threadID); err != nil || !changed {
		t.Fatalf("flip: %v, %v", changed, err)
	}
	if left := unfinishedArchives(t, f.source); !slices.Equal(left, []string{f.threadID}) {
		t.Fatalf("unfinished after the flip = %v", left)
	}
}

// An archive a crash cut after its flip, before it stopped the thread's
// work, is finished by the resume as the archive would have: the open ask
// is cancelled, the loop ends, the command on the paired computer is
// cancelled through the owed cancel when that computer refuses it at
// first, and the archive ends.
func TestAResumedArchiveStopsTheThreadsWork(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	f.crashAfterTheFlip(t)
	refusal := errorsx.Public("remote_not_ready", "Remote commands are not ready on this computer.", nil)
	f.receiver.cancelRefusal.Store(&refusal)

	f.source.finishPendingArchives(f.ctx)

	if state := f.askState(t, f.ask); state != store.ThreadRequestCancelled {
		t.Errorf("the open ask after the resume = %q, want cancelled", state)
	}
	if n := f.loops(t); n != 0 {
		t.Errorf("%d loops still armed after the resume", n)
	}
	watch, err := f.source.store.GetRemoteWatch(f.input.ComputerID, f.input.Request.ID)
	if err != nil || watch.CancelOwedAt == 0 {
		t.Fatalf("watch after the resume = %+v, %v; want its cancel owed", watch, err)
	}
	if left := unfinishedArchives(t, f.source); len(left) != 0 {
		t.Errorf("unfinished after the resume = %v", left)
	}
	f.receiver.cancelRefusal.Store(nil)
	f.source.retryOwedRemoteCancels(time.Now().Add(time.Minute))
	waitRemoteJobState(t, f.receiver, f.input.Request.ID, func(state string) bool { return state != "running" })
	if watch, err := f.source.store.GetRemoteWatch(f.input.ComputerID, f.input.Request.ID); err != nil || watch.CancelOwedAt != 0 {
		t.Fatalf("watch after the owed cancel landed = %+v, %v; want nothing owed", watch, err)
	}
}

// A thread reopened after the boot read its unfinished archive and before
// the resume reached it keeps everything it has: the resume stops none of
// its work, the work it took on since included.
func TestAResumeLeavesAThreadReopenedBeforeIt(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	f.crashAfterTheFlip(t)
	if _, err := f.source.UnarchiveThread(f.threadID); err != nil {
		t.Fatal(err)
	}
	newAsk := f.openAsk(t)

	f.source.resumeArchive(f.threadID)

	for _, token := range []string{f.ask, newAsk} {
		if state := f.askState(t, token); state != store.ThreadRequestAccepted {
			t.Errorf("ask %s after the resume = %q, want it still open", token, state)
		}
	}
	if n := f.loops(t); n != 1 {
		t.Errorf("%d loops armed after the resume, want the thread's one", n)
	}
	if job, err := f.receiver.store.GetRemoteJob(f.input.Request.ID); err != nil || job.State != "running" {
		t.Errorf("command after the resume = %+v, %v; want it still running", job, err)
	}
	archivedIs(t, f.source, "after the resume", map[string]bool{f.threadID: false})
}
