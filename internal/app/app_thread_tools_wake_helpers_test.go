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
	kind, text, err := threadWakeContent(row, late)
	if err != nil {
		t.Fatalf("threadWakeContent(%s): %v", row.Token, err)
	}
	return a.threadWake(threadWakeMessage{late: late, kind: kind, text: text, rows: []store.ThreadRequest{row}}).String()
}

// soloWakeOrigin is who a message carrying only this request's answer is
// attributed to. It does not need the request settled.
func soloWakeOrigin(a *App, row store.ThreadRequest) *usermessage.OriginThread {
	wake := a.threadWake(threadWakeMessage{kind: threadtools.WakeReply, rows: []store.ThreadRequest{row}})
	_, origin := a.threadWakeAttribution(row, wake)
	return origin
}
