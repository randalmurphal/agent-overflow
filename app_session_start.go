package main

import "fmt"

type sessionStart struct {
	done chan struct{}
	err  error
}

func (a *App) runSessionStart(threadID string, start func() error) error {
	startState, leader := a.sessionManager().beginStart(threadID)
	if !leader {
		<-startState.done
		return startState.err
	}

	startState.err = start()
	a.sessionManager().finishStart(threadID, startState)
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
