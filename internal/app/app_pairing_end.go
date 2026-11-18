package app

import (
	"log"

	"agent-overflow/internal/store"
)

// The end of a pairing: the one transition, over the request ledger and the
// command ledger, for a paired computer this one will hear nothing more
// from. The poll's verdict on a pairing that ended, a command watch's, and
// the person forgetting the computer all reach it here
// (docs/specs/agent-thread-tools.md, "Reaching another computer").

// lockComputerPairing takes one paired computer's fence. Admission to it (a
// thread request's row, a command's watch) checks the pairing and writes
// its record inside the fence, and the end of the pairing runs inside it,
// so no request enters a pairing being ended. The fence is held for writes
// only, never across a call to that computer.
func (a *App) lockComputerPairing(computerID string) func() {
	return a.remoteStartLocks().Lock("computer:" + computerID)
}

// endPairing records the end of the pairing with computerID, which answered
// a call by saying it no longer accepts this computer, or which this
// computer no longer holds, and tells the callers their requests ended.
func (a *App) endPairing(computerID, reason string) error {
	unlock := a.lockComputerPairing(computerID)
	wakes, err := a.endPairingFenced(computerID, store.PairingEnd{
		RequestAnswer: []byte(reason),
		CommandError:  "The pairing with this computer ended, so this command's outcome is unknown. " + reason,
	})
	unlock()
	a.deliverThreadWakes(wakes)
	return err
}

// endPairingFenced is endPairing for a caller that holds the computer's
// fence. It returns the wakes the settlements owe, which the caller
// delivers once it has released the fence: a wake takes its thread's locks
// and can start a session.
func (a *App) endPairingFenced(computerID string, end store.PairingEnd) ([]threadWakeRef, error) {
	open, err := a.store.OpenThreadRequestTokensOn(computerID)
	if err != nil {
		return nil, err
	}
	// Settled under each token's settle lock, as every settlement is, and
	// the wake decided under it (collectionWakeOwed). Locked in token
	// order; no holder of one settle lock waits for another.
	unlocks := make([]func(), 0, len(open))
	for _, token := range open {
		unlocks = append(unlocks, a.threadRequestSettleLock(token))
	}
	ended, err := a.store.EndPairing(computerID, open, end)
	var wakes []threadWakeRef
	if err == nil {
		for _, request := range ended.Settled {
			if a.collectionWakeOwed(request.Token, request.Notify) {
				wakes = append(wakes, threadWakeRef{token: request.Token})
			}
		}
	}
	for _, unlock := range unlocks {
		unlock()
	}
	if err != nil {
		return nil, err
	}
	for _, request := range ended.Settled {
		a.forgetRemoteRequestBlocked(request.Token)
	}
	for _, threadID := range ended.CommandThreads {
		a.emitBackgroundChanged(threadID)
	}
	if len(ended.Settled) > 0 || len(ended.CommandThreads) > 0 {
		log.Printf("thread tools: pairing with %s ended; settled %d open request(s) and %d thread(s) of commands",
			computerID, len(ended.Settled), len(ended.CommandThreads))
	}
	return wakes, nil
}
