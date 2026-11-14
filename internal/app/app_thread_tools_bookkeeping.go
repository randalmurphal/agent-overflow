package app

import (
	"log"

	"agent-overflow/internal/store"
)

// Triage reports a delivery fact once: the turn a request's message was
// delivered into (bindDeliveredRequests) and the echo of an answer, a
// reminder or a completion (confirmOwnedDeliveries). The report is the only
// evidence there will be, so a store write that records it and fails keeps
// the fact here, as resolved, until a write lands: every read of a
// receipt's turn tries its binding again and reads the kept one while the
// write fails (deliveredReceiptLocked), and a later echo, a turn
// end or a give-back tries an echo's confirmation again. Nothing is sent to
// the provider again. Each entry goes once its write lands or its receipt
// settles, so the maps hold only facts whose write is failing.

// receiptBinding is the turn a delivery named for a receipt, as
// bindRequestDelivery or rebindRequestDelivery would write it. rebindFrom
// is the turn a rebind moves it from, or -1 for a first binding.
type receiptBinding struct {
	threadID   string
	itemID     string
	turnIndex  int
	ownTurn    bool
	rebindFrom int
}

// ownedEcho is the provider's echo of an answer, a reminder or a completion
// whose confirmation has not been written.
type ownedEcho struct {
	threadID string
	itemID   string
	at       int64
}

// writeReceiptBindingLocked writes b for token and reports whether the
// store moved the receipt. A write that fails keeps b for the next try; one
// that lands, moving the receipt or finding it already bound or settled,
// forgets it. Caller holds the token's settle lock.
func (a *App) writeReceiptBindingLocked(token string, b receiptBinding) (bool, error) {
	if b.rebindFrom >= 0 {
		// A rebind of a binding that never landed binds where it moved to.
		if kept, ok := a.keptReceiptBinding(token); ok && kept.rebindFrom < 0 && kept.turnIndex == b.rebindFrom {
			b.rebindFrom = -1
		}
	}
	var moved bool
	var err error
	if b.rebindFrom >= 0 {
		moved, err = a.store.RebindThreadReceiptTurn(token, b.itemID, b.threadID, b.rebindFrom,
			threadRequestTurnKey(b.threadID, b.rebindFrom), threadRequestTurnKey(b.threadID, b.turnIndex), b.ownTurn)
	} else {
		moved, err = a.store.BindThreadReceiptTurn(token, b.itemID, threadRequestTurnKey(b.threadID, b.turnIndex), b.ownTurn)
	}
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	if err != nil {
		if a.threadRequests.bindings == nil {
			a.threadRequests.bindings = make(map[string]receiptBinding)
		}
		a.threadRequests.bindings[token] = b
		return false, err
	}
	delete(a.threadRequests.bindings, token)
	return moved, nil
}

// keptReceiptBinding is the binding a failed write left for token.
func (a *App) keptReceiptBinding(token string) (receiptBinding, bool) {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	b, ok := a.threadRequests.bindings[token]
	return b, ok
}

// deliveredReceiptLocked reads token's receipt as its deliveries bound it:
// a binding or rebind a failed write kept is written again first, and while
// that write keeps failing the kept binding is still the receipt's turn.
// Every reader that picks a receipt's turn under the settle lock reads it
// here, so none settles, stops or gives back a request against a turn its
// delivery moved it from. Caller holds the token's settle lock.
func (a *App) deliveredReceiptLocked(token string) (store.ThreadRequestReceipt, bool, error) {
	b, kept := a.keptReceiptBinding(token)
	if kept {
		if _, err := a.writeReceiptBindingLocked(token, b); err != nil {
			log.Printf("thread tools: bind receipt %s to turn %d again: %v", token, b.turnIndex, err)
		} else {
			kept = false
		}
	}
	receipt, found, err := a.store.GetThreadRequestReceipt(token)
	if err != nil || !found || !kept {
		return receipt, found, err
	}
	// As the write would leave it (BindThreadReceiptTurn,
	// RebindThreadReceiptTurn). A binding is kept only for an open receipt
	// it could still move: a write that finds one bound elsewhere or
	// settled lands, changing nothing, and a settlement forgets it.
	receipt.State = store.ThreadReceiptRunning
	receipt.MessageItemID = b.itemID
	receipt.TurnID = threadRequestTurnKey(b.threadID, b.turnIndex)
	receipt.OwnTurn = b.ownTurn
	return receipt, true, nil
}

// deliveredReceipt is deliveredReceiptLocked for a reader that holds no
// settle lock.
func (a *App) deliveredReceipt(token string) (store.ThreadRequestReceipt, bool, error) {
	unlock := a.threadRequestSettleLock(token)
	defer unlock()
	return a.deliveredReceiptLocked(token)
}

// confirmOwnedEcho writes the confirmation of an echoed answer, reminder or
// completion, keeping the echo for the next try when the write fails.
func (a *App) confirmOwnedEcho(sendID string, echo ownedEcho) error {
	var err error
	if token, late, ok := threadWakeFromSendID(sendID); ok {
		_, err = a.store.ConfirmThreadWake(token, late, echo.itemID, echo.at)
	} else if computerID, requestID, ok := remoteCompletionFromSendID(sendID); ok {
		_, err = a.store.ConfirmRemoteCompletion(computerID, requestID, echo.at)
	} else {
		return nil
	}
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	if err != nil {
		if a.threadRequests.echoes == nil {
			a.threadRequests.echoes = make(map[string]ownedEcho)
		}
		a.threadRequests.echoes[sendID] = echo
		return err
	}
	delete(a.threadRequests.echoes, sendID)
	return nil
}

// echoed reports whether the provider echoed sendID and its confirmation
// has not been written: it is consumed, and nothing gives it back.
func (a *App) echoed(sendID string) bool {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	_, ok := a.threadRequests.echoes[sendID]
	return ok
}

// hasUnwrittenEchoes reports whether a thread has an echo whose
// confirmation is still to be written.
func (a *App) hasUnwrittenEchoes(threadID string) bool {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	for _, echo := range a.threadRequests.echoes {
		if echo.threadID == threadID {
			return true
		}
	}
	return false
}

// retryOwnedEchoes writes again every confirmation of threadID's echoes a
// failed write left.
func (a *App) retryOwnedEchoes(threadID string) {
	a.threadRequests.mu.Lock()
	var retry map[string]ownedEcho
	for sendID, echo := range a.threadRequests.echoes {
		if echo.threadID == threadID {
			if retry == nil {
				retry = make(map[string]ownedEcho)
			}
			retry[sendID] = echo
		}
	}
	a.threadRequests.mu.Unlock()
	for sendID, echo := range retry {
		if err := a.confirmOwnedEcho(sendID, echo); err != nil {
			log.Printf("thread tools: confirm %s in %s again: %v", sendID, threadID, err)
		}
	}
}
