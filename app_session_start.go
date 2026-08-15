package main

import (
	"context"
	"fmt"
)

type sessionStart struct {
	done chan struct{}
	err  error
}

// runSessionStart single-flights session starts per thread: the first caller
// leads and runs start(), everybody else joins its result.
//
// Two properties are load-bearing and were both learned the hard way:
//
//   - The leader's teardown is DEFERRED. A panic inside start() would
//     otherwise leave the in-flight entry registered and its `done` channel
//     open forever, so every later start of that thread — and every joiner
//     already parked — blocks for the life of the process.
//   - A joiner's wait is cancellable. It has performed no side effects, so
//     abandoning it costs nothing; the leader is unaffected and its start
//     runs to completion either way. (The leader's own wait is the provider
//     IO inside start(), which deliberately stays uncancellable — a
//     cancelled spawn/send is indistinguishable from a delivered one.)
func (a *App) runSessionStart(ctx context.Context, threadID string, start func() error) error {
	startState, leader := a.sessionManager().beginStart(threadID)
	if !leader {
		select {
		case <-startState.done:
			return startState.err
		case <-ctx.Done():
			return fmt.Errorf("waiting for in-flight session start of thread %s: %w", threadID, ctx.Err())
		}
	}

	completed := false
	defer func() {
		if !completed && startState.err == nil {
			// The leader is unwinding through a panic. Joiners are released
			// with a real cause instead of a nil error plus a thread that
			// mysteriously has no session.
			startState.err = fmt.Errorf("session start for thread %s panicked", threadID)
		}
		a.sessionManager().finishStart(threadID, startState)
	}()
	startState.err = start()
	completed = true
	return startState.err
}

func (a *App) closeProviderSession(threadID string, sess session) error {
	providerSess := sess.providerSession()
	if providerSess == nil {
		return nil
	}
	// Capture the pgid before Close — the process exits during Close and
	// we want the group id regardless.
	pgid := providerSess.PID()
	if err := providerSess.Close(); err != nil {
		return fmt.Errorf("close %s session for thread %s: %w", sess.provider, threadID, err)
	}
	// Clean close → the subprocess is down, so stop the orphan reaper from
	// tracking it. On a Close error we deliberately keep the watch: an
	// abandoned-but-still-alive subprocess must still be reaped if the app
	// later dies.
	a.releaseSessionProcess(pgid)
	return nil
}
