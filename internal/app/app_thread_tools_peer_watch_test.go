package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// TestThreadToolsForwardedWatchEndsWithItsCaller: a thread_status wait
// forwarded to another computer stops there as soon as the calling
// computer gives up on it, instead of reading that computer's live state
// until its slice runs out.
func TestThreadToolsForwardedWatchEndsWithItsCaller(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	var faults *peerFaults
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		wire, served := serveFaultyApp(t, app)
		faults = served
		return wire
	})
	faults.watchEnded = make(chan struct{}, 4)
	// The spawned thread's turn stays open, so a wait on it parks there.
	installMockClaudeTurns(t, pair.dest, [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"working"}]}}`,
	}})
	ack := pair.spawnThere(t, "keep working", nil)
	target := pair.receipt(t, ack.Token).TargetThreadID
	waitUntil(t, 30*time.Second, func() bool {
		report, err := pair.dest.threadToolsAdapter().RequestStates(t.Context(), pair.callerIdentity(),
			threadtools.StatusCall{ThreadIDs: []string{target}})
		return err == nil && len(report.Threads) == 1 && !threadtools.Rests(report.Threads[0])
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		_, err := pair.source.threadToolsServer().Call(ctx, pair.callerIdentity(), "thread_status",
			json.RawMessage(`{"thread_ids":["`+target+`"],"wait_seconds":50}`))
		returned <- err
	}()
	waitUntil(t, 30*time.Second, func() bool { return pair.dest.requestWaitActive(threadWatchKey(target)) })

	cancel()
	<-returned
	select {
	case <-faults.watchEnded:
	case <-time.After(20 * time.Second):
		t.Fatal("the forwarded wait was still running on the destination after its caller left")
	}
	if pair.dest.requestWaitActive(threadWatchKey(target)) {
		t.Fatal("the destination still holds a wait whose caller left")
	}
}

// olderDestination serves the thread peer methods as a computer on v0.0.19
// does for a thread_status wait: there is no ThreadToolWatch, and
// ThreadToolQuery resolves every id before it waits and refuses the whole
// call with the first id that does not resolve
// (v0.0.19:internal/threadtools/requests.go statusThreads). Every other
// answer is this build's.
type olderDestination struct {
	*App
	// beforeQuery, when set, runs as each thread_status query arrives.
	beforeQuery func()
}

func (o *olderDestination) ThreadToolQuery(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	if call.Tool != "thread_status" {
		return o.App.ThreadToolQuery(ctx, call)
	}
	if o.beforeQuery != nil {
		o.beforeQuery()
	}
	var args struct {
		ThreadIDs []string `json:"thread_ids"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return ThreadPeerReply{}, err
	}
	for _, id := range args.ThreadIDs {
		resolution, err := o.threadToolsAdapter().ResolveThreadRef(ctx, id)
		if err != nil {
			return ThreadPeerReply{}, err
		}
		if len(resolution.Matches) != 1 {
			return ThreadPeerReply{}, o.publicThreadToolError(call.Tool, call.Source.ThreadID, errorsx.Public(threadtools.CodeNotFound,
				fmt.Sprintf("No thread or assistant matches %q on this computer.", id), nil))
		}
	}
	return o.App.ThreadToolQuery(ctx, call)
}

// serveOlderDestination serves app as a computer on v0.0.19.
func serveOlderDestination(t *testing.T, app *App) (*pairedBackend, *olderDestination) {
	t.Helper()
	older := &olderDestination{App: app}
	wire := servePairedApp(t, app, func(cfg *transport.Config) {
		dispatcher := transport.NewDispatcher()
		if _, err := dispatcher.Register(older, transport.RegisterOptions{
			Package: "main", TypeName: "App", AllowList: transport.NewMethodAllowList(),
			Skip: map[string]bool{"ThreadToolWatch": true},
		}); err != nil {
			t.Fatalf("register the older destination: %v", err)
		}
		cfg.Dispatcher = dispatcher
	})
	return wire, older
}

// TestThreadToolsWatchReachesAnOlderComputerThroughItsReadCall: a computer
// on v0.0.19 serves no ThreadToolWatch, and a wait on its thread is
// answered through ThreadToolQuery as that release answers it.
func TestThreadToolsWatchReachesAnOlderComputerThroughItsReadCall(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		wire, _ := serveOlderDestination(t, app)
		return wire
	})
	remote := reachThread(t, pair.dest, "dddddddd-0000-4000-8000-000000000001", "On the older computer")

	result := pair.call(t, "thread_status", `{"thread_ids":["`+remote.ID+`"],"wait_seconds":10}`)
	if woke, _ := result["woke_on"].(string); woke != remote.ID {
		t.Fatalf("the wait on the older computer's thread = %+v, want it to end on the resting thread", result)
	}
}

// TestAThreadGoneFromAnOlderComputerLeavesItsSiblingsTheirStates: of two
// threads on a computer on v0.0.19, the first is deleted after the calling
// computer resolved them and before that computer serves the wait, which it
// then refuses whole. The deleted thread reads gone and ends the wait, its
// idle sibling reads as it stands, and nothing says the wait ran out.
func TestAThreadGoneFromAnOlderComputerLeavesItsSiblingsTheirStates(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	var older *olderDestination
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		wire, served := serveOlderDestination(t, app)
		older = served
		return wire
	})
	deleted := reachThread(t, pair.dest, "dddddddd-0000-4000-8000-000000000001", "Deleted")
	idle := reachThread(t, pair.dest, "dddddddd-0000-4000-8000-000000000002", "Idle")
	var once sync.Once
	older.beforeQuery = func() {
		once.Do(func() {
			if err := pair.dest.store.DeleteThread(deleted.ID); err != nil {
				t.Errorf("delete the thread: %v", err)
			}
		})
	}

	result := pair.call(t, "thread_status", `{"thread_ids":["`+deleted.ID+`","`+idle.ID+`"],"wait_seconds":50}`)
	if result["woke_on"] != deleted.ID || result["timed_out"] != nil || result["errors"] != nil {
		t.Fatalf("result = %v, want the deleted thread as the wake, no timeout and no computer under errors", result)
	}
	threads, _ := result["threads"].([]any)
	if len(threads) != 2 {
		t.Fatalf("threads = %v, want both", result["threads"])
	}
	gone, _ := threads[0].(map[string]any)
	if gone["thread_id"] != deleted.ID || gone["state"] != threadtools.StateUnavailable || gone["error_code"] != threadtools.CodeNotFound ||
		gone["computer_id"] != pair.computer {
		t.Fatalf("the deleted thread's row = %v, want it gone from the older computer", gone)
	}
	kept, _ := threads[1].(map[string]any)
	if kept["thread_id"] != idle.ID || kept["state"] != threadtools.StateIdle || kept["error"] != nil || kept["computer_id"] != pair.computer {
		t.Fatalf("the sibling's row = %v, want it idle as it stands", kept)
	}
}

// TestThreadToolWatchRunsOnlyAThreadWait. ThreadToolWatch is caller-bound
// because it runs one branch of one read tool; anything else it carried
// would run on a context its caller's disconnect cuts.
func TestThreadToolWatchRunsOnlyAThreadWait(t *testing.T) {
	t.Parallel()
	app, granted, _ := autonomyApp(t)
	before := threadIDsInStore(t, app)
	source := threadtools.Caller{ThreadID: uuid.NewString()}
	for _, call := range []ThreadPeerCall{
		{Tool: "thread_spawn", Args: []byte(`{"prompt":"go"}`), Source: source},
		{Tool: "thread_search", Args: []byte(`{}`), Source: source},
		{Tool: "thread_options", Args: []byte(`{}`), Source: source},
		{Tool: "thread_status", Args: []byte(`{}`), Source: source},
	} {
		_, err := app.ThreadToolWatch(callFrom(granted, false), call)
		if code, _, ok := errorsx.PublicDetails(err); !ok || code != threadtools.CodeInvalidRequest {
			t.Errorf("ThreadToolWatch(%s %s) = %v, want %s", call.Tool, call.Args, err, threadtools.CodeInvalidRequest)
		}
	}
	if after := threadIDsInStore(t, app); len(after) != len(before) {
		t.Fatalf("threads before %v, after %v", before, after)
	}
}
