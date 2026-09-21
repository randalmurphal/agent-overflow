package transport

import (
	"net/http"
	"time"
)

// SessionAuthority is the complete session admission boundary. A nil authority
// serves launch-credential loopback clients only and cannot mint session tickets.
type SessionAuthority interface {
	Resolve(*http.Request) (sessionID string, ok bool)
	Check(sessionID string) SessionStatus
	AdmitsPeer(sessionID, remoteAddr string) bool
}

// SessionStatus is a current admission decision, never a connection's cached
// authorization. A zero Deadline denotes a process-bound local session.
type SessionStatus struct {
	Scopes   []string
	Deadline time.Time
	Refusal  string
}

func (s *Server) resolveSession(r *http.Request) (string, bool) {
	if s.cfg.Sessions == nil {
		return "", SessionCredential(r) == ""
	}
	return s.cfg.Sessions.Resolve(r)
}

func (s *Server) checkSession(id string) SessionStatus {
	if id == "" || s.cfg.Sessions == nil {
		return SessionStatus{Refusal: "unknown_session"}
	}
	return s.cfg.Sessions.Check(id)
}
