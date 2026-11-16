package app

import (
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
)

// soloWakeBody renders the message one settled request's answer is delivered
// as when nothing else is delivered with it.
func soloWakeBody(t *testing.T, a *App, row store.ThreadRequest, late bool) string {
	t.Helper()
	body, err := wakeBodyOf(a, row, late)
	if err != nil {
		t.Fatalf("render the wake of %s: %v", row.Token, err)
	}
	return body
}

// wakeBodyOf is soloWakeBody reporting a request that owes no wake.
func wakeBodyOf(a *App, row store.ThreadRequest, late bool) (string, error) {
	message, err := soloThreadWakeMessage(row, late)
	if err != nil {
		return "", err
	}
	return a.threadWakeFor(message).String(), nil
}

// soloWakeOrigin is who a message carrying only this request's answer is
// attributed to. It does not need the request settled.
func soloWakeOrigin(a *App, row store.ThreadRequest) *usermessage.OriginThread {
	wake := a.threadWakeFor(threadWakeMessage{kind: threadtools.WakeReply, rows: []store.ThreadRequest{row}})
	return a.threadWakeNote(row, wake).OriginThread()
}
