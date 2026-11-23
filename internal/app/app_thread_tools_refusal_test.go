package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// A request that cannot be started is answered by the call that made it, on
// this computer and on a paired one, and by no message afterwards.

// databaseFailure is a real SQLite error out of the fixture's store: a
// second insert of one token, which its primary key refuses.
func (f *requestFixture) databaseFailure(t *testing.T) error {
	t.Helper()
	row := store.ThreadRequest{
		Token: "tok-twice", CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSend, State: store.ThreadRequestUnconfirmed,
	}
	if err := f.app.store.InsertThreadRequest(row); err != nil {
		t.Fatalf("InsertThreadRequest: %v", err)
	}
	err := f.app.store.InsertThreadRequest(row)
	if !store.IsDatabaseFailure(err) {
		t.Fatalf("a duplicate insert = %v, want a database failure", err)
	}
	return err
}

// The error carries the cause, the request is recorded as delivered with the
// same text, and no message repeats the refusal later.
func TestARequestThatCannotStartIsAnsweredByItsOwnCall(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "noted")
	f.holdCallerTurn(t)
	adapter := f.adapter()
	cause := errors.New("create thread: worktree: git worktree add failed: fatal: 'rb-S1' is already used by worktree at '/work/rb-S1'")

	var token string
	err := f.startUnconfirmedRequest(t, store.ThreadRequest{
		Kind: store.ThreadRequestSpawn, Notify: true,
	}, func(minted string) (string, error) {
		token = minted
		return "", adapter.failRequest(minted, cause)
	})

	code, message, ok := errorsx.PublicDetails(err)
	if !ok || code != threadtools.CodeInvalidRequest {
		t.Fatalf("refusal = %v, want a public one", err)
	}
	if !strings.Contains(message, "'rb-S1' is already used by worktree at '/work/rb-S1'. Nothing ran under it") {
		t.Fatalf("refusal = %q, want the cause", message)
	}
	if !errors.Is(err, cause) {
		t.Errorf("the refusal dropped its cause from the chain: %v", err)
	}
	row := f.request(t, token)
	if row.State != store.ThreadRequestRefused || string(row.Answer) != message {
		t.Fatalf("request = %q %q, want it refused with the same text the call returned", row.State, row.Answer)
	}
	if row.DeliveredAt == 0 || row.DeliveredHow != store.ThreadWakeInline {
		t.Fatalf("delivery = %q at %d, want the call's own error recorded as the delivery", row.DeliveredHow, row.DeliveredAt)
	}
	// The retry pass is what used to send the refusal as a message.
	f.app.retryUndeliveredThreadWakes(time.Now().Add(time.Hour))
	f.nothingOwed(t)
	if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
		t.Fatalf("a refusal the call returned was also queued as a message: %+v", rows)
	}
}

// A cause that already carries reviewed prose is returned as it is, and any
// other is said in full, except the database failing: its text stays in the
// log, out of the call and out of the record thread_status reads back.
func TestARefusalWithholdsOnlyADatabaseFailure(t *testing.T) {
	t.Parallel()
	reviewed := errorsx.Public(threadtools.CodeNotFound, "There is no project p-1 on this computer.", nil)
	if got := threadRequestRefusal(reviewed, ""); got != reviewed {
		t.Fatalf("refusal = %v, want the reviewed error itself", got)
	}
	said := threadRequestRefusal(errors.New("send message: start session: exec: \"claude\": executable file not found in $PATH."), "")
	if _, message, _ := errorsx.PublicDetails(said); !strings.Contains(message, `start session: exec: "claude": executable file not found in $PATH. Nothing ran under it`) {
		t.Fatalf("refusal = %q, want the cause in full", message)
	}

	f := newRequestFixture(t)
	f.holdCallerTurn(t)
	adapter := f.adapter()
	cause := fmt.Errorf("accept the request: %w", f.databaseFailure(t))
	var token string
	err := f.startUnconfirmedRequest(t, store.ThreadRequest{
		Kind: store.ThreadRequestSpawn, Notify: true,
	}, func(minted string) (string, error) {
		token = minted
		return "", adapter.failRequest(minted, cause)
	})
	_, message, ok := errorsx.PublicDetails(err)
	if !ok || !strings.Contains(message, "the database on the computer that would run it failed") {
		t.Fatalf("refusal = %v, want the fixed sentence", err)
	}
	if strings.Contains(message, "tok-twice") || strings.Contains(strings.ToLower(message), "constraint") {
		t.Fatalf("refusal = %q shows the database's own text", message)
	}
	if !errors.Is(err, cause) {
		t.Errorf("the refusal dropped its cause from the chain: %v", err)
	}
	row := f.request(t, token)
	if row.State != store.ThreadRequestRefused || row.DeliveredHow != store.ThreadWakeInline {
		t.Fatalf("request = %q delivered %q, want refused and delivered by the call", row.State, row.DeliveredHow)
	}
	if string(row.Answer) != message {
		t.Errorf("recorded answer = %q, want the refusal the call returned", row.Answer)
	}
	f.nothingOwed(t)
}

// A fork whose thread could not keep the spawn's group, its source being
// outside every project, is refused before its request or thread exists,
// so a notify spawn leaves nothing owed. A spawn refused after its thread
// existed is TestASpawnWhoseThreadExistsNamesItInTheRefusal.
func TestARefusedSpawnWithNotifyQueuesNoMessage(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.holdCallerTurn(t)
	source := testThread("refused-spawn-source")
	source.ProjectID = ""
	source.Provider = string(provider.Claude)
	source.SessionRef = "refused-spawn"
	fixture := newMidTurnForkFixture(t, testProviderHome(t, f.app), "refused-spawn", midTurnSourceJSONL)
	source.WorkspacePath = fixture.workspace
	if err := f.app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertForkTestItems(t, f.app.store, source.ID)

	_, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		FromThread: source.ID, Prompt: "carry on", Group: "Release", Notify: true,
	})
	if code := publicCode(t, err); code != threadtools.CodeInvalidRequest || !strings.Contains(err.Error(), "takes no group or placement") {
		t.Fatalf("spawn into an impossible group: code = %q, err = %v", code, err)
	}
	rows, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 10, 0)
	if err != nil || len(rows) != 0 {
		t.Fatalf("requests = %+v err %v, want none: the refusal comes before the request exists", rows, err)
	}
	threads, err := f.app.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	for _, thread := range threads {
		if thread.ForkedFromThreadID == source.ID {
			t.Fatalf("the refused fork left thread %s behind", thread.ID)
		}
	}
	f.nothingOwed(t)
}

// vanishingClaudeAfterTheSpawnIsChecked gives app a Claude binary that a
// spawn's checks find and that is removed once the spawn's thread exists,
// before its prompt is sent: the one way a spawn's thread can exist with
// its provider unable to start.
func vanishingClaudeAfterTheSpawnIsChecked(t *testing.T, app *App) {
	t.Helper()
	binary := createMockBinary(t, "2.1.284 (Claude Code)")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
	app.spawnNamingForTest = func(string) {
		if err := os.Remove(binary); err != nil {
			t.Errorf("remove the claude binary: %v", err)
		}
	}
}

// A spawn whose thread was created and whose prompt then could not be sent
// names that thread in its refusal, whatever the cause, so the caller
// continues it instead of spawning a second one.
func TestASpawnWhoseThreadExistsNamesItInTheRefusal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		cause error
		want  string
	}{
		{"a cause said in full", errors.New("start session: exec: no such file."), "That request could not be started: start session: exec: no such file. Thread t-1 was created for it and its prompt did not run."},
		{"reviewed prose", errorsx.Public(threadtools.CodeNotFound, "That mode needs approval.", nil), "That mode needs approval. Thread t-1 was created for it and its prompt did not run."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, message, ok := errorsx.PublicDetails(threadRequestRefusal(tc.cause, "t-1"))
			if !ok || !strings.HasPrefix(message, tc.want) {
				t.Fatalf("refusal = %q, want it to open %q", message, tc.want)
			}
			if strings.Contains(message, "make the request again") || !strings.Contains(message, "thread_send instead of spawning again") {
				t.Fatalf("refusal = %q, want it to point at the thread that exists", message)
			}
		})
	}

	f := newRequestFixture(t)
	f.holdCallerTurn(t)
	// The provider is there when the spawn is checked and gone once its
	// thread exists, so the prompt cannot start.
	vanishingClaudeAfterTheSpawnIsChecked(t, f.app)
	_, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "review the branch", Notify: true})
	_, message, ok := errorsx.PublicDetails(err)
	if !ok {
		t.Fatalf("spawn = %v, want a public refusal", err)
	}
	rows, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 10, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("requests = %+v err %v, want the one refused spawn", rows, err)
	}
	row := rows[0]
	if row.State != store.ThreadRequestRefused || row.DeliveredHow != store.ThreadWakeInline || string(row.Answer) != message {
		t.Fatalf("request = %q delivered %q answer %q, want refused with the call's text", row.State, row.DeliveredHow, row.Answer)
	}
	if row.TargetThreadID == "" {
		t.Fatal("the refused spawn records no thread")
	}
	if _, err := f.app.store.GetThread(row.TargetThreadID); err != nil {
		t.Fatalf("the spawned thread: %v", err)
	}
	if !strings.Contains(message, "Thread "+row.TargetThreadID+" was created for it and its prompt did not run.") {
		t.Fatalf("refusal = %q, want it to name thread %s", message, row.TargetThreadID)
	}
	f.nothingOwed(t)
}

// A failed attempt of a request-minting call may have started the request
// unless the destination answered it with a refusal, which it gives only for
// a request it did not start. That is what decides whether it is sent again
// and whether the request can be refused (callThreadPeerRequest); a call the
// carrier never sent is TestThreadToolsAnUnreachableComputerIsRefusedAtOnce's.
func TestWhetherAFailedPeerAttemptMayHaveStartedTheRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		ran  bool
	}{
		{"the destination refuses it", &rpcclient.Error{Code: threadtools.CodeInvalidRequest, Message: "That request could not be started: not a git repository."}, false},
		{"the destination's transport refuses the pairing", &rpcclient.Error{Code: transport.ErrCodeScopeRequired, Message: "scope missing"}, false},
		{"the destination does not have the method", &rpcclient.Error{Code: transport.ErrCodeMethodNotFound, Message: "method not registered"}, false},
		{"the destination cannot read the arguments", &rpcclient.Error{Code: transport.ErrCodeBadParams, Message: "App.ThreadToolsSpawn: expected 3 params, got 4"}, false},
		{"the destination fails in a way this computer cannot read", &rpcclient.Error{Code: transport.ErrCodeMethodError, Message: "method failed (id: trace)"}, true},
		{"the reply is lost", errors.New("the answer was lost on the way back"), true},
		{"the wait for the reply ends", context.DeadlineExceeded, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ran := threadErrorDetails("thread_spawn", tc.err); ran != tc.ran {
				t.Fatalf("may have started = %v, want %v", ran, tc.ran)
			}
		})
	}
}

// A spawn the other computer refuses is answered by the call that made it:
// the cause is in the error, the request is refused and delivered, and no
// poll afterwards turns it into a message.
func TestARequestAnotherComputerRefusesIsAnsweredByItsOwnCall(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	installMockClaudeReplies(t, pair.source, "read the wake")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// The destination's project is a plain directory, so git refuses the
	// worktree there.
	_, err := pair.adapter().Spawn(ctx, pair.callerIdentity(), threadtools.SpawnCall{
		Prompt: "review the branch", ComputerID: pair.computer, ProjectID: pair.project,
		WorktreeBranch: "agent/review", WorktreeBaseLocal: true, Notify: true,
	})
	_, message, ok := errorsx.PublicDetails(err)
	const cause = "not a git repository"
	if !ok || !strings.Contains(message, cause) {
		t.Fatalf("spawn = %v, want the destination's refusal with its cause", err)
	}
	rows, err := pair.source.store.ListThreadRequestsByCaller(pair.caller.ID, 10, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("requests = %+v err %v, want the one refused spawn", rows, err)
	}
	if rows[0].State != store.ThreadRequestRefused || rows[0].DeliveredHow != store.ThreadWakeInline {
		t.Fatalf("request = %q delivered %q, want refused and delivered by the call", rows[0].State, rows[0].DeliveredHow)
	}
	if string(rows[0].Answer) != message {
		t.Errorf("recorded answer = %q, want what the call returned, which names the computer that refused", rows[0].Answer)
	}

	pair.poll(t)
	pair.source.retryUndeliveredThreadWakes(time.Now().Add(time.Hour))
	pair.source.threadRequestsWG.Wait()
	if row := pair.request(t, rows[0].Token); row.State != store.ThreadRequestRefused || row.DeliveredHow != store.ThreadWakeInline {
		t.Fatalf("after a poll the request = %q delivered %q", row.State, row.DeliveredHow)
	}
	if queued := durableQueueRows(t, pair.source, pair.caller.ID); len(queued) != 0 {
		t.Fatalf("a refusal the call returned was also queued as a message: %+v", queued)
	}
}

// The user's case of a send that could not start: the target's queue is
// full. The call says so, and the request is refused and delivered by it.
func TestASendIntoAFullQueueSaysTheQueueIsFull(t *testing.T) {
	t.Parallel()
	q := newQueuedRequests(t)
	f := q.f
	f.holdCallerTurn(t)
	holdFlushQueue(f.app)
	for n := range maxQueueLength() {
		if _, err := f.app.RegisterQueueItem(context.Background(), q.target.ID, "filler "+strconv.Itoa(n), SendMessageOptions{}); err != nil {
			t.Fatalf("fill the queue at %d: %v", n, err)
		}
	}

	_, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: q.target.ID, Message: "one more", Notify: true,
	})
	_, message, ok := errorsx.PublicDetails(err)
	if !ok || !strings.Contains(message, "queue full (max "+strconv.Itoa(maxQueueLength())+" items per thread). Nothing ran under it") {
		t.Fatalf("send = %v, want the queue's refusal", err)
	}
	rows, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 10, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("requests = %+v err %v, want the one refused send", rows, err)
	}
	if rows[0].State != store.ThreadRequestRefused || rows[0].DeliveredHow != store.ThreadWakeInline || string(rows[0].Answer) != message {
		t.Fatalf("request = %q delivered %q answer %q", rows[0].State, rows[0].DeliveredHow, rows[0].Answer)
	}
	f.nothingOwed(t)
}

// startUnconfirmedRequest writes a request of the caller unconfirmed under a
// fresh token, as a call does before its destination accepts it, and runs
// accept with the token: the refusal accept returns is the call's error.
func (f *requestFixture) startUnconfirmedRequest(t *testing.T, row store.ThreadRequest, accept func(token string) (string, error)) error {
	t.Helper()
	row.Token = newThreadRequestToken()
	row.CallerThreadID = f.caller.ID
	row.State = store.ThreadRequestUnconfirmed
	if err := f.app.store.InsertThreadRequest(row); err != nil {
		t.Fatalf("InsertThreadRequest: %v", err)
	}
	_, err := accept(row.Token)
	return err
}
