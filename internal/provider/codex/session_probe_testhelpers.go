package codex

import (
	"context"
	"encoding/json"
	"errors"

	"agent-overflow/internal/provider"
)

// NewProbeOnlyTestSession returns a *Session whose Probe method
// resolves exclusively from the supplied function, skipping the
// app-server wire call. The rest of the session is left at zero —
// callers MUST NOT call Send / Interrupt / Close on it. The only
// supported entry point is Probe.
//
// This helper exists so the app-layer `codexthread` reconciler can be
// tested without spinning up
// a real Codex app-server. Production code never constructs Session
// through this path; NewSession in session_start.go is the only
// supported production constructor.
//
// Scoped out of `_test.go` so sibling packages (notably the root
// `main` package's integration tests) can import it; the package's
// own tests use it too. If we grow more seams we may move this into
// a `codextest` subpackage — for now the single helper lives here.
func NewProbeOnlyTestSession(probeFn func(ctx context.Context) (ProbeResult, error)) *Session {
	return &Session{
		probeFn: probeFn,
	}
}

// NewProbeAndResumeTestSession extends NewProbeOnlyTestSession with a
// Resume override. The on-reopen reconcile flow calls Probe first, then
// (on notLoaded) Resume — wiring both as stubs lets the app-layer test
// assert the sequenced behaviour without any subprocess.
//
// Either function may be nil: callers typically pass both. A nil probe
// falls back to the wire path (which will fail because proc is nil), so
// only production-shaped tests should leave it unset.
func NewProbeAndResumeTestSession(
	probeFn func(ctx context.Context) (ProbeResult, error),
	resumeFn func(ctx context.Context) error,
) *Session {
	return &Session{
		probeFn:  probeFn,
		resumeFn: resumeFn,
	}
}

// BackgroundTerminalsTestWire stands in for the app-server side of the
// background-terminal RPCs in NewBackgroundTerminalsTestSession. Each
// function receives the provider thread the call named. A nil function
// fails the call. List answers with the running terminals' item ids; an
// error from any of them goes through the same not-loaded reading as a
// wire error (TestRPCError builds Codex's).
type BackgroundTerminalsTestWire struct {
	// RootThreadID is the session's root provider thread.
	RootThreadID string
	Clean        func(ctx context.Context, providerThreadID string) error
	Terminate    func(ctx context.Context, providerThreadID, processID string) (bool, error)
	List         func(ctx context.Context, providerThreadID string) ([]string, error)
	// Children maps each agent thread the session owns to its spawn launch.
	Children map[string]string
	// InterruptChild answers an agent's turn/interrupt. With it set, every
	// child is running.
	InterruptChild func(ctx context.Context, childThreadID, turnID string) error
	// OnEvent receives the events the session reports, such as an agent
	// thread's unload; nil drops them.
	OnEvent func(provider.ProviderEvent)
}

// TestRPCError is the JSON-RPC error a test wire answers with, such as
// Codex's -32600 "thread not found: <id>" for a thread it has not loaded.
func TestRPCError(code int, message string) error {
	return &RPCError{Code: code, Message: message}
}

// NewBackgroundTerminalsTestSession returns a *Session whose
// background-terminal methods run their real scope and argument checks and
// their real reading of the answer, then answer from wire instead of an
// app-server. The app-layer binding tests use it to assert which thread and
// process a stop reaches without a real subprocess. The rest of the
// session is zero, so callers MUST NOT call Send / Interrupt / Close on it.
func NewBackgroundTerminalsTestSession(wire BackgroundTerminalsTestWire) *Session {
	collab := sessionCollabState{
		childParentByThread:  map[string]string{},
		childRuntimeByThread: map[string]childRuntimeState{},
	}
	for childThreadID, launchID := range wire.Children {
		collab.childParentByThread[childThreadID] = launchID
		if wire.InterruptChild != nil {
			collab.childRuntimeByThread[childThreadID] = childRuntimeState{phase: childRuntimeRunning}
		}
	}
	onEvent := wire.OnEvent
	if onEvent == nil {
		onEvent = func(provider.ProviderEvent) {}
	}
	s := &Session{
		threadID:             "test-thread",
		collab:               collab,
		onEvent:              onEvent,
		interruptChildTurnFn: wire.InterruptChild,
		cleanBackgroundTerminalsFn: func(ctx context.Context, providerThreadID string) error {
			if wire.Clean == nil {
				return errors.New("codex test session: no clean wire")
			}
			return wire.Clean(ctx, providerThreadID)
		},
		terminateBackgroundTerminalFn: func(ctx context.Context, providerThreadID, processID string) (bool, error) {
			if wire.Terminate == nil {
				return false, errors.New("codex test session: no terminate wire")
			}
			return wire.Terminate(ctx, providerThreadID, processID)
		},
		listBackgroundTerminalsFn: func(ctx context.Context, providerThreadID, _ string) (json.RawMessage, error) {
			if wire.List == nil {
				return nil, errors.New("codex test session: no list wire")
			}
			itemIDs, err := wire.List(ctx, providerThreadID)
			if err != nil {
				return nil, err
			}
			rows := make([]BackgroundTerminal, 0, len(itemIDs))
			for _, itemID := range itemIDs {
				rows = append(rows, BackgroundTerminal{ItemID: itemID})
			}
			return json.Marshal(backgroundTerminalListResponse{Data: rows})
		},
	}
	s.setRootThreadID(wire.RootThreadID)
	return s
}

// NewInterruptSubagentTestSession returns a session with one pending child
// owned by launchID. Only the child turn/interrupt wire write is replaced, so
// the same launch ownership and repeated-stop checks run as in production.
func NewInterruptSubagentTestSession(
	launchID string,
	interruptFn func(ctx context.Context, childThreadID, turnID string) error,
) *Session {
	const childThreadID = "test-child-thread"
	session := &Session{
		threadID: "test-thread",
		collab: sessionCollabState{
			childParentByThread: map[string]string{childThreadID: launchID},
			childRuntimeByThread: map[string]childRuntimeState{
				childThreadID: {phase: childRuntimePending},
			},
		},
		onEvent:              func(provider.ProviderEvent) {},
		interruptChildTurnFn: interruptFn,
	}
	session.setRootThreadID("test-root-thread")
	return session
}
