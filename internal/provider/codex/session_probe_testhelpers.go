package codex

import "context"

// NewProbeOnlyTestSession returns a *Session whose Probe method
// resolves exclusively from the supplied function, skipping the
// app-server wire call. The rest of the session is left at zero —
// callers MUST NOT call Send / Interrupt / Close on it. The only
// supported entry point is Probe.
//
// This helper exists so the app-layer reconciler
// (App.ReconcileCodexOnReopen) can be unit-tested without spinning up
// a real Codex app-server. Production code never constructs Session
// through this path; NewSession in session.go is the only
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
