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

func closeProviderSession(threadID string, sess session) error {
	providerSess := sess.providerSession()
	if providerSess == nil {
		return nil
	}
	if err := providerSess.Close(); err != nil {
		return fmt.Errorf("close %s session for thread %s: %w", sess.provider, threadID, err)
	}
	return nil
}
