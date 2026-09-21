package app

import (
	"net/http"
	"time"

	"agent-overflow/internal/transport"
)

// SessionAuthority binds all session admission operations to the same owner.
func SessionAuthority(a *App) transport.SessionAuthority { return sessionAuthority{app: a} }

type sessionAuthority struct{ app *App }

func (s sessionAuthority) Resolve(r *http.Request) (string, bool) { return SessionForRequest(s.app, r) }
func (s sessionAuthority) AdmitsPeer(id, peer string) bool        { return SessionAdmitsPeer(s.app, id, peer) }
func (s sessionAuthority) Check(id string) transport.SessionStatus {
	state := s.app.identityState()
	if state == nil {
		return transport.SessionStatus{Refusal: "temporarily_unavailable"}
	}
	row, reason := state.sessions.Live(id)
	if reason.Refused() {
		return transport.SessionStatus{Refusal: reason.Code()}
	}
	result := transport.SessionStatus{Scopes: row.Scopes}
	if !row.ProcessBound() {
		result.Deadline = time.UnixMilli(row.ExpiresAt)
	}
	return result
}
