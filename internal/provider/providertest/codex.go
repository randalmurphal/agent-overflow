package providertest

import "agent-overflow/internal/provider"

// FormatCodexUserAgent is the one formatter of a Codex app-server's
// `initialize` user agent, in upstream's shape: `{originator}/{build}
// ({os} {os version}; {arch}) {terminal}`, then the client's
// ` ({name}; {version})` (rust-v0.154.0
// codex-rs/login/src/auth/default_client.rs get_codex_user_agent,
// codex-rs/app-server/src/request_processors/initialize_processor.rs).
// originator is CODEX_INTERNAL_ORIGINATOR_OVERRIDE when the process has one,
// otherwise clientName; clientName and clientVersion are the clientInfo the
// handshake sent. The executable mock and every Go fixture answer with it.
func FormatCodexUserAgent(build, originator, clientName, clientVersion string) string {
	return originator + "/" + build + " (Linux 6.6.0; x86_64) xterm-256color (" + clientName + "; " + clientVersion + ")"
}

// CodexInitializeResultFor is the `initialize` result a stock Codex at the
// minimum, launched with no originator override, answers the AO client
// named clientName with.
func CodexInitializeResultFor(clientName string) string {
	return `{"userAgent":"` + CodexUserAgentFor(clientName) + `"}`
}

// CodexInitializeResultQuotedFor is CodexInitializeResultFor escaped for a
// double-quoted shell string.
func CodexInitializeResultQuotedFor(clientName string) string {
	return `{\"userAgent\":\"` + CodexUserAgentFor(clientName) + `\"}`
}

// CodexUserAgentFor is the user agent of that reply.
func CodexUserAgentFor(clientName string) string {
	return FormatCodexUserAgent(provider.MinimumCodexCLIVersion, clientName, clientName, provider.CodexClientVersion)
}

// CodexUserAgent, CodexInitializeResult and CodexInitializeResultQuoted
// answer a session's handshake (clientInfo.name provider.CodexClientOrigin):
// a fake app-server that does not read the handshake and serves sessions
// uses them. A fake that serves another client answers with that client's
// (CodexInitializeResultFor).
var (
	CodexUserAgent              = CodexUserAgentFor(provider.CodexClientOrigin)
	CodexInitializeResult       = CodexInitializeResultFor(provider.CodexClientOrigin)
	CodexInitializeResultQuoted = CodexInitializeResultQuotedFor(provider.CodexClientOrigin)
)

// CodexUserAgentShellCase is a bash statement for a fake app-server that
// serves several AO clients: it sets variable to the user agent a stock
// Codex of build answers the client named on the initialize request in
// $line with, one branch per clientNames, each built by the formatter. build
// is spliced into a single-quoted string, so a fake that picks its build at
// run time passes `'"$version"'`. A client it does not list makes the fake
// exit 97, so a fixture that answers the wrong client fails instead of
// being admitted unread.
func CodexUserAgentShellCase(variable, build string, clientNames ...string) string {
	statement := "case \"$line\" in\n"
	for _, name := range clientNames {
		statement += "  *'\"name\":\"" + name + "\"'*) " + variable + "='" +
			FormatCodexUserAgent(build, name, name, provider.CodexClientVersion) + "' ;;\n"
	}
	return statement + "  *) exit 97 ;;\nesac\n"
}

// CodexClientNames is every clientInfo.name an AO Codex client sends.
var CodexClientNames = []string{
	provider.CodexClientOrigin,
	provider.CodexProbeClientName,
	provider.CodexLoginClientName,
	provider.CodexUsageClientName,
	provider.CodexSkillsClientName,
	provider.CodexForkClientName,
	provider.CodexMCPAuthClientName,
	provider.CodexMCPStatusClientName,
}

// CodexInitializeInOrder opens a fake app-server that reads its requests in
// order: it reads the initialize request (id 1) and answers it as a stock
// Codex at the minimum answers the AO client that sent it.
func CodexInitializeInOrder() string {
	return "IFS= read -r line || true\n" + CodexUserAgentShellCase("ua", provider.MinimumCodexCLIVersion, CodexClientNames...) +
		"printf '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"userAgent\":\"%s\"}}\\n' \"$ua\"\n"
}
