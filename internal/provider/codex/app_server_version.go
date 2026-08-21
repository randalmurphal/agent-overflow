package codex

import (
	"encoding/json"
	"strings"

	"agent-overflow/internal/provider"
)

// The app-server's build version, learned from the `initialize` handshake.
//
// Why this exists at all: `initialize` carries no capability list and no
// protocol version an app-server client can branch on, so a method whose
// PARAMS changed shape between releases cannot be attempted speculatively —
// an older server rejects the unknown params with a JSON-RPC error rather
// than ignoring them (`account/usage/read` typed its params `Option<()>`
// through 0.147 and `Option<GetAccountTokenUsageParams>` from 0.148, so a
// `{threadId}` request to a 0.147 server is a hard invalid-params error every
// time). The only in-band statement of the running build is
// `InitializeResponse.userAgent`, e.g.
// `codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) …`
// (codex-rs/login/src/auth/default_client.rs get_codex_user_agent).
//
// This is deliberately NOT a new probe. AO already runs `codex --version`
// once per binary at detection time (provider.DetectProvider) to enforce the
// package-wide floor; this reads the version off a response the session was
// already waiting for. The two answers agree in practice but this one is
// strictly better for a per-call gate: it describes the process actually on
// the other end of this pipe, not whatever binary the setting named when the
// app booted.

// recordAppServerVersion stores the build version stated by an `initialize`
// response. A response that carries no parseable version leaves the field
// empty, which every gate reads as "too old" — fail closed, since the cost of
// guessing wrong is a JSON-RPC error on every attempt.
func (s *Session) recordAppServerVersion(initializeResponse json.RawMessage) {
	version := parseAppServerVersion(readNestedString(initializeResponse, "userAgent"))
	s.appServerVersion.Store(&version)
}

// AppServerVersion returns the codex build this session's process reported at
// `initialize`, or "" when it could not be determined.
func (s *Session) AppServerVersion() string {
	if v := s.appServerVersion.Load(); v != nil {
		return *v
	}
	return ""
}

// appServerAtLeast reports whether the connected app-server is at least the
// given version. An unknown version is never "at least" anything.
func (s *Session) appServerAtLeast(floor string) bool {
	return provider.CodexCLIVersionAtLeast(s.AppServerVersion(), floor)
}

// parseAppServerVersion pulls the build version out of a codex user-agent
// string. The version is the token between the first `/` and the following
// space (`codex_cli_rs/0.149.0 (…)`); everything after it — OS name, OS
// version, architecture, an optional client suffix — carries digits of its
// own, so the whole string is deliberately NOT handed to the loose
// first-semver-token parser.
//
// Anything that does not have that shape yields "", never a guess.
func parseAppServerVersion(userAgent string) string {
	_, rest, found := strings.Cut(strings.TrimSpace(userAgent), "/")
	if !found {
		return ""
	}
	token, _, _ := strings.Cut(rest, " ")
	return provider.ParseCodexCLIVersion(token)
}
