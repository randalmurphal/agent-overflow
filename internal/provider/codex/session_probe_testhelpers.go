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
