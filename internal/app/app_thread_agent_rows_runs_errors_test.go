package app

// A message that starts work on a worker first ends the runs its rest
// already ended (endRunsBeforeWork). When that read or write fails, the
// operation fails before anything is dispatched, with the error, rather
// than starting the work inside a run that ended.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/threadtools"
)

// runBoundaryFaults are the store failures a message's run boundary can
// meet: the read of the runs its rest would end, the read of the worker's
// rest, and the write that ends the runs. A read fails because an index it
// names is gone.
type runBoundaryFault struct {
	name, statement string
	// want is the text the error carries; empty for the read of the runs,
	// whose text names the read each path makes (runBoundaryFault.expect).
	want string
}

var runBoundaryFaults = []runBoundaryFault{
	{"the read of the runs", `DROP INDEX idx_thread_requests_run_open`, ""},
	{"the read of the worker's rest", `DROP INDEX idx_remote_watches_thread`, "is at rest to end its runs"},
	{"the write that ends them", `CREATE TRIGGER refuse_run_end BEFORE UPDATE OF run_tool_calls ON thread_requests
		BEGIN SELECT RAISE(ABORT, 'injected run end failure'); END`, "injected run end failure"},
}

// expect is the text a path's error must carry for the fault, given the
// failed read of the runs that path makes first.
func (f runBoundaryFault) expect(runsRead string) string {
	if f.want == "" {
		return runsRead
	}
	return f.want
}

// holdRestPasses keeps the background rest pass from ending runs, so the
// message's own boundary is the one that would.
func holdRestPasses(t *testing.T, a *App) {
	t.Helper()
	r := &a.threadRequests
	r.mu.Lock()
	r.rest.checking = true
	r.mu.Unlock()
	t.Cleanup(func() {
		r.mu.Lock()
		r.rest.checking = false
		r.mu.Unlock()
	})
}

// recordDispatches replaces the provider dispatch with a recorder.
func recordDispatches(a *App) func() []string {
	var mu sync.Mutex
	var sent []string
	a.sendMessageFn = func(threadID, content string, _ []string) error {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, threadID+": "+content)
		return nil
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), sent...)
	}
}

// A person's message to a worker whose spawn's run its rest ended, which
// the rest pass has not recorded yet, fails with the store's error when
// that boundary cannot be read or written, and nothing is dispatched.
func TestASendFailsWhenItsRunBoundaryFails(t *testing.T) {
	t.Parallel()
	for _, fault := range runBoundaryFaults {
		t.Run(fault.name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			holdRestPasses(t, f.app)
			worker := f.plainThread(t)
			acceptRequest(t, f.app, "boundary-spawn", f.caller.ID, store.ThreadRequestSpawn, worker.ID)
			settleTaken(t, f.app, "boundary-spawn")
			if row := requestRow(t, f.app, "boundary-spawn"); !row.RunOpen() {
				t.Fatalf("the spawn %+v; the test needs its run open with the rest pass held back", row)
			}
			sent := recordDispatches(f.app)
			execOnStore(t, f.dbPath, fault.statement)

			err := f.app.SendMessage(context.Background(), worker.ID, "go on", nil)
			if want := fault.expect("read whether a run is open on"); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("SendMessage = %v; want the store's failure %q", err, want)
			}
			if got := sent(); len(got) != 0 {
				t.Fatalf("dispatched %q; want nothing", got)
			}
		})
	}
}

// The spawner's own send through the thread tools fails the same way,
// before its request is written.
func TestAToolsSendFailsWhenItsRunBoundaryFails(t *testing.T) {
	t.Parallel()
	for _, fault := range runBoundaryFaults {
		t.Run(fault.name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			holdRestPasses(t, f.app)
			worker := f.plainThread(t)
			acceptRequest(t, f.app, "tools-boundary-spawn", f.caller.ID, store.ThreadRequestSpawn, worker.ID)
			settleTaken(t, f.app, "tools-boundary-spawn")
			before, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 1000, 0)
			if err != nil {
				t.Fatal(err)
			}
			sent := recordDispatches(f.app)
			execOnStore(t, f.dbPath, fault.statement)

			_, err = f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: worker.ID, Message: "one more", Notify: true})
			if want := fault.expect("has a run open on"); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Send = %v; want the store's failure %q", err, want)
			}
			if got := sent(); len(got) != 0 {
				t.Fatalf("dispatched %q; want nothing", got)
			}
			after, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 1000, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Fatalf("the caller holds %d requests after the failed send, %d before; want none written", len(after), len(before))
			}
		})
	}
}

// An edit-and-resend in such a worker fails the same way, before the
// revert: the thread's messages are as they were.
func TestAnEditAndResendFailsWhenItsRunBoundaryFails(t *testing.T) {
	t.Parallel()
	for _, fault := range runBoundaryFaults {
		t.Run(fault.name, func(t *testing.T) {
			t.Parallel()
			dbPath := storetest.ClonePath(t)
			app, _ := newResendTestAppAt(t, dbPath)
			holdRestPasses(t, app)
			worker, _ := seedResendThread(t, app, "t-resend-boundary")
			spawner := noticeTestThread(t, app, "resend-boundary-spawner")
			acceptRequest(t, app, "resend-boundary-spawn", spawner.ID, store.ThreadRequestSpawn, worker.ID)
			settleTaken(t, app, "resend-boundary-spawn")
			before, err := app.store.ListItems(worker.ID)
			if err != nil {
				t.Fatal(err)
			}
			sent := recordDispatches(app)
			execOnStore(t, dbPath, fault.statement)

			err = revertAndResendForTest(app, context.Background(), worker.ID, "user:1", RevertAndResendOptions{Content: "rewritten prompt"})
			if want := fault.expect("read whether a run is open on"); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("revert and resend = %v; want the store's failure %q", err, want)
			}
			if got := sent(); len(got) != 0 {
				t.Fatalf("dispatched %q; want nothing", got)
			}
			after, err := app.store.ListItems(worker.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Fatalf("the thread holds %d items after the failed resend, %d before; want it untouched", len(after), len(before))
			}
		})
	}
}

// A send of the spawner's to its worker on a paired computer reads the
// spawn that links the worker to start its run there. When that read
// fails, the send fails with the error before any row is written or
// anything reaches that computer.
func TestARemoteSendFailsWhenItsRunStartCannotBeRead(t *testing.T) {
	t.Parallel()
	pair := newReachPair(t)
	spawn, child := remoteRestedWorker(t, pair)
	before, err := pair.source.store.ListThreadRequestsByCaller(pair.caller.ID, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The spawn's row no longer scans: its run's start is not a number.
	execOnStore(t, pair.dbPath, `UPDATE thread_requests SET run_base_tokens = 'unreadable' WHERE token = ?`, spawn.Token)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err = pair.adapter().Send(ctx, pair.callerIdentity(), threadtools.SendCall{
		ThreadID: child, ComputerID: pair.computer, Message: "and the release notes", Notify: true,
	})
	if err == nil || !strings.Contains(err.Error(), "read the spawn") {
		t.Fatalf("remote send = %v; want the failed read of its spawn", err)
	}
	execOnStore(t, pair.dbPath, `UPDATE thread_requests SET run_base_tokens = 0 WHERE token = ?`, spawn.Token)
	after, err := pair.source.store.ListThreadRequestsByCaller(pair.caller.ID, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("the caller holds %d requests after the failed send, %d before; want none written", len(after), len(before))
	}
	receipts, err := pair.dest.store.ListThreadRequestReceiptsForThread(child)
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range receipts {
		if receipt.Kind == store.ThreadRequestSend {
			t.Fatalf("the destination holds send receipt %+v; want nothing sent", receipt)
		}
	}
}
