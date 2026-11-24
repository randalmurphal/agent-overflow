package providertest

import "agent-overflow/internal/provider"

// CodexUserAgentFor is the `initialize` user agent a Codex app-server of
// version answers a handshake with when its originator is originator, in
// upstream's shape: `{originator}/{build} ({os}; {arch}) {terminal}`, then
// the client's `({name}; {version})` (rust-v0.154.0
// codex-rs/login/src/auth/default_client.rs get_codex_user_agent). The
// executable mock and the Go fakes both answer with it.
func CodexUserAgentFor(originator, version string) string {
	return originator + "/" + version + " (Linux 6.6.0; x86_64) xterm-256color (" + originator + "; 0.1.0)"
}

// CodexUserAgent is the user agent a fake Codex app-server that does not
// read the handshake answers with: the oldest version AO admits, with the
// originator a session's handshake sends. A client that sends another name
// reads it through its one-candidate fallback (codex.appServerBuild).
const CodexUserAgent = provider.CodexClientOrigin + "/" + provider.MinimumCodexCLIVersion +
	" (Linux 6.6.0; x86_64) xterm-256color (" + provider.CodexClientOrigin + "; 0.1.0)"

// CodexInitializeResult is the `result` object of that `initialize` answer.
const CodexInitializeResult = `{"userAgent":"` + CodexUserAgent + `"}`

// CodexInitializeResultQuoted is CodexInitializeResult escaped for a
// double-quoted shell string.
const CodexInitializeResultQuoted = `{\"userAgent\":\"` + CodexUserAgent + `\"}`
