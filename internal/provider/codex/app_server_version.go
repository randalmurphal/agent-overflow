package codex

import (
	"strings"

	"agent-overflow/internal/provider"
)

// The app-server's build version, learned from the `initialize` handshake.
//
// `initialize` carries no capability list and no protocol version a client
// can branch on; the only in-band statement of the running build is
// `InitializeResponse.userAgent`, e.g.
// `codex_cli_rs/0.154.0 (Ubuntu 24.04; x86_64) …`
// (codex-rs/login/src/auth/default_client.rs get_codex_user_agent). Dial and
// the one-shot client admit the process by it (provider.CheckCodexVersion),
// and the binary watch compares it with the binary on disk. It describes the
// process actually on the other end of the pipe, not whatever binary the
// setting named when the app booted.

// AppServerVersion returns the codex build this session's process reported at
// `initialize`, or "" when it could not be determined.
func (s *Session) AppServerVersion() string {
	if v := s.appServerVersion.Load(); v != nil {
		return *v
	}
	return ""
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
