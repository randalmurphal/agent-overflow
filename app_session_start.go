package main

import (
	"fmt"
	"log"

	"agent-overflow/internal/provider"
)

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
	if err := a.reconcileClosedProviderProfile(sess); err != nil {
		return fmt.Errorf("share %s account state after closing thread %s: %w", sess.provider, threadID, err)
	}
	return nil
}

func (a *App) reconcileClosedProviderProfile(sess session) error {
	if sess.provider != string(provider.Codex) ||
		sess.credentialAccountID == "" ||
		a.providerCredentials == nil {
		return nil
	}
	return a.providerCredentials.ReconcileProfile(sess.provider, sess.credentialAccountID)
}

func (a *App) logReconcileExitedProviderProfile(threadID string, sess session) {
	if err := a.reconcileClosedProviderProfile(sess); err != nil {
		log.Printf("app: share %s account state after thread %s exited: %v", sess.provider, threadID, err)
	}
}
