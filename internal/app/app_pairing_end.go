package app

import (
	"log"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/store"
)

// The end of a pairing: the one transition, over the request ledger and the
// command ledger, for a pairing with a paired computer that will answer
// nothing again (docs/specs/agent-thread-tools.md, "Reaching another
// computer"). Every record of either ledger names the pairing that
// admitted it, the session id of this computer's profile for that computer
// then (store.ThreadRequest.Pairing), and the transition ends what the
// pairings other than the one held now admitted, and nothing else. It runs
// where the pairing's profile ends, whoever learned of it: the profile's
// removal (backendSetChanged), the person forgetting the computer, and any
// call or sweep that finds a record of a pairing no longer held.

// lockComputerPairing takes one paired computer's fence. Admission to it (a
// thread request's row, a command's watch) reads the pairing and writes its
// record inside the fence, the end of a pairing runs inside it, and so does
// applying what that computer answered about a request, so no record enters
// a pairing being ended and no answer reopens what its end closed. The fence
// is held for writes only, never across a call to that computer.
func (a *App) lockComputerPairing(computerID string) func() {
	return a.remoteStartLocks().Lock("computer:" + computerID)
}

// currentPairing names the pairing this computer holds with computerID now
// (attachedbackends.Manager.Grant), empty when it holds none.
func (a *App) currentPairing(computerID string) (string, error) {
	if a.backends == nil {
		return "", nil
	}
	return a.backends.Grant(computerID)
}

// pairingLive reports whether a record admitted under the pairing recorded
// belongs to the pairing held now (current). A record from before pairings
// were recorded belongs to whichever pairing is held.
func pairingLive(recorded, current string) bool {
	return current != "" && (recorded == "" || recorded == current)
}

// Why the end of a pairing ended a request, and the error a command there is
// recorded with.
const (
	pairingEndedByComputer = "The other computer no longer accepts this pairing. Reconnect it in Remote access."
	pairingEndedHere       = "This computer no longer holds the pairing this was made under. Reconnect the computer in Remote access if it is no longer paired."
	pairingEndedCommand    = "The pairing with this computer ended, so this command's outcome is unknown. "
)

// pairingEndReason is why a removed profile's pairing ended.
func pairingEndReason(reason attachedbackends.RemovalReason) string {
	if reason == attachedbackends.RemovedByComputer {
		return pairingEndedByComputer
	}
	return pairingEndedHere
}

// endPairing ends every pairing with computerID other than the one this
// computer holds now, and tells the callers their requests ended. A record
// the pairing held now admitted is left alone.
func (a *App) endPairing(computerID, reason string) error {
	unlock := a.lockComputerPairing(computerID)
	live, err := a.currentPairing(computerID)
	var wakes []threadWakeRef
	if err == nil {
		wakes, err = a.endPairingFenced(computerID, live, store.PairingEnd{
			RequestAnswer: []byte(reason),
			CommandError:  pairingEndedCommand + reason,
		})
	}
	unlock()
	a.deliverThreadWakes(wakes)
	return err
}

// endPairingDetached is endPairing on the request work, for a caller that
// can hold the locks it takes: the profile's removal is announced from
// inside a call to that computer, and from inside the forget that holds the
// fence. One that fails, or that shutdown refuses, is ended by the first
// sweep that finds a record of that pairing.
func (a *App) endPairingDetached(computerID, reason string) {
	if a.lifeCtx().Err() != nil || !a.threadRequestsWG.Go(func() {
		logThreadRequestSweep("end the pairing with "+computerID, a.endPairing(computerID, reason))
	}) {
		log.Printf("thread tools: the end of the pairing with %s is left to the sweeps: the app is stopping", computerID)
	}
}

// endEndedPairings reads the pairing held with computerID now and, when any
// of the records' pairings is not it, ends what those pairings admitted. It
// returns the pairing held now. A sweep calls it before it asks that
// computer anything, so what an ended pairing admitted is ended rather than
// asked about under another one.
func (a *App) endEndedPairings(computerID string, recorded []string) (string, error) {
	current, err := a.currentPairing(computerID)
	if err != nil {
		return "", err
	}
	for _, pairing := range recorded {
		if !pairingLive(pairing, current) {
			return current, a.endPairing(computerID, pairingEndedHere)
		}
	}
	return current, nil
}

// pairingEndedSince ends the pairing a failed call to computerID was made
// under (before) when this computer no longer holds it: the call's own
// refusal retired the profile, or another pairing replaced it meanwhile. It
// reports whether it did; a pairing still held failed for some other reason.
func (a *App) pairingEndedSince(computerID, before string) bool {
	current, err := a.currentPairing(computerID)
	if err != nil || current == before {
		return false
	}
	if err := a.endPairing(computerID, pairingEndedHere); err != nil {
		logThreadRequestSweep("end the pairing with "+computerID, err)
		return false
	}
	return true
}

// endPairingFenced ends what the pairings with computerID other than live
// admitted, for a caller that holds the computer's fence. It returns the
// wakes the settlements owe, which the caller delivers once it has released
// the fence: a wake takes its thread's locks and can start a session.
func (a *App) endPairingFenced(computerID, live string, end store.PairingEnd) ([]threadWakeRef, error) {
	open, err := a.store.OpenThreadRequestTokensOn(computerID, live)
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
	ended, err := a.store.EndPairing(computerID, live, open, end)
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
