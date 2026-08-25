package main

import (
	"agent-overflow/internal/keyedlock"
)

// The App's two keyed-lock registries. The registry itself lives in
// internal/keyedlock; what stays here is which registries the App owns and the
// lock-order rules between them.

func (a *App) threadLocks() *keyedlock.Registry {
	a.threadActionLocksOnce.Do(func() {
		if a.threadActionLocks == nil {
			a.threadActionLocks = keyedlock.New()
		}
	})
	return a.threadActionLocks
}

// configApplyLocks serializes liveApplySessionConfig per thread: the apply
// is a read-modify-write over session.launchOpts (snapshot → plan → send →
// commit), and two concurrent reconciles would both plan against the same
// snapshot and both send the same change. A separate registry rather than
// the thread action lock because reconcile callers arrive both with and
// without that lock held (applyRuntimeMode and the deferred watcher hold
// it; the model-selection bindings do not).
//
// Lock-order rules: thread action lock → config-apply lock → a.mu, never
// any other order. Session-START paths must never acquire a config-apply
// lock — the serialized section blocks on waitForStartingSession, so a
// start that waited on this lock would deadlock against a holder waiting
// on the start.
func (a *App) configApplyLocks() *keyedlock.Registry {
	a.sessionConfigApplyLocksOnce.Do(func() {
		if a.sessionConfigApplyLocks == nil {
			a.sessionConfigApplyLocks = keyedlock.New()
		}
	})
	return a.sessionConfigApplyLocks
}
