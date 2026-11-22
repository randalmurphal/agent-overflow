package app

import (
	"log"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/store"
)

// The end of a pairing: the one transition, over the request ledger and the
// command ledger, for a paired computer this one will hear nothing more
// from (docs/specs/agent-thread-tools.md, "Reaching another computer").
// Work belongs to the computer, not to one pairing with it: pairing it again
// continues the work, and a record names its computer and nothing else
// about the pairing. The transition runs where the pairing ends and nothing
// replaces it: the person forgetting the computer (RemoveBackend), the
// other computer's verdict that this one is revoked while no pairing has
// been saved since (endRevokedPairing), and a sweep that finds no pairing
// saved for the computer at all (endUnpaired).

// lockComputerPairing takes one paired computer's fence. Admission to it (a
// thread request's row, a command's watch) checks the pairing and writes
// its record inside the fence, the end of a pairing runs inside it, and so
// do applying what that computer answered about a request and refusing a
// request whose call was never sent (startRemoteRequest), so nothing
// admitted after an end is touched by it and nothing admitted before it is
// missed. The fence is held for writes only, never across a call to that
// computer.
func (a *App) lockComputerPairing(computerID string) func() {
	return a.remoteStartLocks().Lock("computer:" + computerID)
}

// pairingSaved reports whether this computer holds a saved pairing with
// computerID. A profile that exists but cannot be read is an error, never
// "none".
func (a *App) pairingSaved(computerID string) (bool, error) {
	if a.backends == nil {
		return false, nil
	}
	_, paired, err := a.backends.Lookup(computerID)
	return paired, err
}

// Why the end of a pairing ended a request, and the error a command there is
// recorded with.
const (
	pairingEndedByComputer = "The other computer no longer accepts this pairing. Reconnect it in Remote access."
	pairingEndedHere       = "This computer is no longer paired with the other computer. Reconnect it in Remote access."
	pairingEndedCommand    = "The pairing with this computer ended, so this command's outcome is unknown. "
)

// pairingEnd is the record the end of a pairing for reason writes.
func pairingEnd(reason, computerName string) store.PairingEnd {
	return store.PairingEnd{RequestAnswer: []byte(reason), CommandError: pairingEndedCommand + reason, ComputerName: computerName}
}

// endUnpaired ends computerID's work when this computer holds no saved
// pairing with it, and tells the callers their requests ended. It reports
// whether it ended it. A profile that cannot be read ends nothing and is
// the error; the caller puts its work off by the error delay. The saved
// profile is what admission checks under the same fence, so a pairing
// saved since another ended, though not yet in use, keeps every record. A
// pairing the other computer's verdict ended (Manager.TakeVerdict) ends
// with that reason and the name it gave the computer.
func (a *App) endUnpaired(computerID string) (bool, error) {
	unlock := a.lockComputerPairing(computerID)
	paired, err := a.pairingSaved(computerID)
	if err != nil || paired {
		unlock()
		return false, err
	}
	end := pairingEnd(pairingEndedHere, "")
	name, revoked := "", false
	if a.backends != nil {
		name, revoked = a.backends.TakeVerdict(computerID)
	}
	if revoked {
		end = pairingEnd(pairingEndedByComputer, name)
	}
	wakes, err := a.endPairingFenced(computerID, end)
	done := a.ownerUpdates.cancelPeer(computerID)
	a.forgetPeerAgentRows(computerID)
	unlock()
	if done != nil {
		a.threadRequestsWG.Go(func() { <-done })
	}
	a.deliverThreadWakes(wakes)
	return err == nil, err
}

// unpairedAfterFailure is endUnpaired for a sweep whose call to computerID
// failed: it reports whether the failure was the pairing's absence, which
// ended that computer's work. Any other failure, an unreadable profile
// included, is the sweep's to retry.
func (a *App) unpairedAfterFailure(computerID string) bool {
	ended, err := a.endUnpaired(computerID)
	if err != nil {
		logThreadRequestSweep("end the work of "+computerID+", which holds no pairing", err)
	}
	return ended
}

// endRevokedPairing acts on the other computer's verdict that it revoked
// this one (attachedbackends.RemovedByComputer). The verdict ends the
// computer's work only when, under the fence, no pairing has been saved
// since (endUnpaired): a pairing made since continues the work. It runs off
// the caller, which announces the verdict from inside a call to that
// computer; one that fails, or that shutdown refuses, is ended by the first
// sweep that finds the computer unpaired.
func (a *App) endRevokedPairing(change attachedbackends.SetChange) {
	if a.lifeCtx().Err() != nil || !a.threadRequestsWG.Go(func() {
		_, err := a.endUnpaired(change.ID)
		logThreadRequestSweep("end the pairing "+change.ID+" revoked", err)
	}) {
		log.Printf("thread tools: the end of the pairing with %s is left to the sweeps: the app is stopping", change.ID)
	}
}

// endPairingFenced ends every request, late-reply poll, owed
// acknowledgement and command watch for computerID, for a caller that holds
// the computer's fence. It returns the wakes the settlements owe, which the
// caller delivers once it has released the fence: a wake takes its
// thread's locks and can start a session.
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
