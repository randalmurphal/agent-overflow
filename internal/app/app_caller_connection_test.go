package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/transport"
)

// newTestConnection opens a client connection with principal for a test:
// the context a call over it carries and its state. Its cleanups run when
// the test ends, as a closed socket's do.
func newTestConnection(t testing.TB, principal transport.ConnPrincipal) (context.Context, *transport.ConnState) {
	t.Helper()
	ctx, state := transport.WithConnState(context.Background(), principal)
	t.Cleanup(state.RunCleanups)
	return ctx, state
}

// testConnections holds the connection each test's calls arrive on when it
// does not name one.
var testConnections sync.Map // testing.TB -> context.Context

// connCtx is the context of the one connection a test's calls arrive on
// when the test does not name one, opened by newTestConnection on first
// use: every call a test makes through it comes from the same client.
func connCtx(t testing.TB) context.Context {
	t.Helper()
	if ctx, ok := testConnections.Load(t); ok {
		return ctx.(context.Context)
	}
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	if prior, loaded := testConnections.LoadOrStore(t, ctx); loaded {
		return prior.(context.Context)
	}
	t.Cleanup(func() { testConnections.Delete(t) })
	return ctx
}

// testConnection is ctx's connection, for a test that calls below the
// bound method.
func testConnection(t testing.TB, ctx context.Context) *transport.ConnState {
	t.Helper()
	conn, err := transport.CallerConnection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// connectionRefusal calls one method that arms a resource for its
// connection, and reports whether anything it arms was created; created is
// nil where the App holds nothing it could create.
type connectionRefusal struct {
	call    func(*App, context.Context) error
	created func(*App) bool
}

func ignoreResult[T any](_ T, err error) error { return err }

// callerConnectionRefusals is every method that opens with
// transport.CallerConnection (TestArmingMethodsAreTiedToTheirConnection
// keeps the lists equal).
var callerConnectionRefusals = map[string]connectionRefusal{
	"SubscribePRUpdates": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.SubscribePRUpdates(ctx, gitops.PRReference{}))
		},
		created: func(a *App) bool { return a.prUpdates.pumps != nil || a.prUpdates.handles != nil },
	},
	"SubscribePRList": {
		call:    func(a *App, ctx context.Context) error { return ignoreResult(a.SubscribePRList(ctx)) },
		created: func(a *App) bool { return a.prUpdates.list != nil || a.prUpdates.listHandles != nil },
	},
	"SubscribePipeline": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.SubscribePipeline(ctx, gitops.RepoReference{}, ""))
		},
		created: func(a *App) bool { return a.prUpdates.pipelines != nil || a.prUpdates.pipelineHandles != nil },
	},
	"GitStatusSubscribe": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.GitStatusSubscribe(ctx, WorkspaceRef{}))
		},
		created: func(a *App) bool { return a.gitApp != nil },
	},
	"BrowserCompanionPaneAttach": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.BrowserCompanionPaneAttach(ctx, "thread"))
		},
		// The App has no browser manager to mount in: a step past the
		// refusal fails with "browser manager unavailable".
	},
	"BeginDictation": {
		call:    func(a *App, ctx context.Context) error { return ignoreResult(a.BeginDictation(ctx, "audio/webm")) },
		created: func(a *App) bool { return a.dictation != nil },
	},
	"BeginLiveDictation": {
		call:    func(a *App, ctx context.Context) error { return ignoreResult(a.BeginLiveDictation(ctx)) },
		created: func(a *App) bool { return a.liveDictation != nil },
	},
	"ProviderTerminalAttach": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.ProviderTerminalAttach(ctx, "thread"))
		},
		created: func(a *App) bool { return a.providerTerminals.byCaller != nil },
	},
	"WatchBackgroundTaskOutput": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.WatchBackgroundTaskOutput(ctx, "thread", "launch"))
		},
		created: func(a *App) bool { return a.taskOutput.bindings != nil || a.taskOutput.tailer.Load() != nil },
	},
	"SubscribeTrackerBoard": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.SubscribeTrackerBoard(ctx, "project", "connection", "1", nil))
		},
		// The App has no tracker service: a step past the refusal fails
		// with "tracker connections are not available before the app has
		// started".
	},
	"SubscribeTrackerTicket": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.SubscribeTrackerTicket(ctx, "connection", "10101"))
		},
		// As SubscribeTrackerBoard's.
	},
	"GetNativeNetworkConfig": {
		call: func(a *App, ctx context.Context) error { return ignoreResult(a.GetNativeNetworkConfig(ctx)) },
		created: func(a *App) bool {
			return a.nativeNetwork.seen || a.nativeNetwork.owner != nil
		},
	},
	"OpenComputerPairing": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.OpenComputerPairing(ctx, "lan", "full"))
		},
		created: computerPairingCreated,
	},
	"OpenOwnComputerPairing": {
		call:    func(a *App, ctx context.Context) error { return ignoreResult(a.OpenOwnComputerPairing(ctx, "lan")) },
		created: computerPairingCreated,
	},
	"OpenPRDiff": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.OpenPRDiff(ctx, WorkspaceRef{}, gitops.PRReference{}, gitops.PRRevision{}, false, true))
		},
		created: reviewDiffCreated,
	},
	"OpenPRCommitDiff": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.OpenPRCommitDiff(ctx, WorkspaceRef{}, gitops.PRReference{}, gitops.PRRevision{}, "", "", false, true))
		},
		created: reviewDiffCreated,
	},
	"OpenCommitFileDiff": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.OpenCommitFileDiff(ctx, WorkspaceRef{}, []string{"file"}))
		},
		created: reviewDiffCreated,
	},
	"OpenWorkspaceDiff": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.OpenWorkspaceDiff(ctx, WorkspaceRef{}, false))
		},
		created: reviewDiffCreated,
	},
	"OpenBranchBaseDiff": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.OpenBranchBaseDiff(ctx, WorkspaceRef{}, "main", false))
		},
		created: reviewDiffCreated,
	},
	"OpenCommitDiff": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.OpenCommitDiff(ctx, WorkspaceRef{}, "HEAD", false))
		},
		created: reviewDiffCreated,
	},
	"OpenEditDiff": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.OpenEditDiff(ctx, "thread", "payload"))
		},
		created: reviewDiffCreated,
	},
	"OpenTurnEditsDiff": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.OpenTurnEditsDiff(ctx, "thread", 0))
		},
		created: reviewDiffCreated,
	},
	"ThreadOwnerChanges": {
		call: func(a *App, ctx context.Context) error {
			return ignoreResult(a.ThreadOwnerChanges(ctx, OwnerCursor{}))
		},
		created: func(a *App) bool { return a.ownerActivity.devices != nil },
	},
}

func computerPairingCreated(a *App) bool {
	return a.computerPairing.book != nil || a.computerPairing.windowID != ""
}

func reviewDiffCreated(a *App) bool {
	return a.reviewDiffs.diffs != nil || a.reviewDiffFiles.len() != 0
}

// Every method that arms a resource for its connection refuses a call that
// came over none before it does anything else. Each runs on an App with no
// dependencies, so any step taken before the refusal (a forge fetch, a
// pump, a file watch, a dial, a mount, a slot) panics or fails another way,
// and what it would have created is checked to be absent.
func TestArmingMethodsRefuseACallWithNoConnection(t *testing.T) {
	t.Parallel()
	names := make([]string, 0, len(callerConnectionRefusals))
	for name := range callerConnectionRefusals {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		refusal := callerConnectionRefusals[name]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := &App{}
			err := func() (err error) {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("ran past the refusal and panicked: %v", r)
					}
				}()
				return refusal.call(a, context.Background())
			}()
			if !errors.Is(err, transport.ErrNoConnection) {
				t.Fatalf("%s with no connection: err = %v, want transport.ErrNoConnection", name, err)
			}
			if refusal.created != nil && refusal.created(a) {
				t.Fatalf("%s with no connection created what it arms", name)
			}
		})
	}
}
