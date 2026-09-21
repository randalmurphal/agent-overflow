package transport

import "net/http"

// testSessionAuthority supplies explicit test decisions through the complete
// production interface. Tests can replace individual decisions during a race.
type testSessionAuthority struct {
	resolve func(*http.Request) (string, bool)
	live    func(string) bool
	scopes  func(string) ([]string, string)
	admits  func(string, string) bool
	check   func(string) SessionStatus
}

func (s *testSessionAuthority) Resolve(r *http.Request) (string, bool) {
	if s.resolve == nil {
		return "", true
	}
	return s.resolve(r)
}
func (s *testSessionAuthority) Check(id string) SessionStatus {
	if s.check != nil {
		return s.check(id)
	}
	if s.live != nil && !s.live(id) {
		return SessionStatus{Refusal: "revoked_session"}
	}
	if s.scopes != nil {
		scopes, refusal := s.scopes(id)
		return SessionStatus{Scopes: scopes, Refusal: refusal}
	}
	var scopes []string
	for _, scope := range Scopes {
		scopes = append(scopes, string(scope))
	}
	return SessionStatus{Scopes: scopes}
}
func (s *testSessionAuthority) AdmitsPeer(id, peer string) bool {
	return s.admits == nil || s.admits(id, peer)
}
func sessionAuthorityForTest(cfg *Config) *testSessionAuthority {
	if cfg.Sessions == nil {
		cfg.Sessions = &testSessionAuthority{}
	}
	return cfg.Sessions.(*testSessionAuthority)
}
