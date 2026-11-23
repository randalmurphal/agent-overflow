package providertest

import "agent-overflow/internal/provider"

// CodexUserAgent is the `initialize` user agent a fake Codex app-server
// answers with: the oldest version AO admits.
const CodexUserAgent = "codex_cli_rs/" + provider.MinimumCodexCLIVersion + " (fake; providertest)"

// CodexInitializeResult is the `result` object of that `initialize` answer.
const CodexInitializeResult = `{"userAgent":"` + CodexUserAgent + `"}`

// CodexInitializeResultQuoted is CodexInitializeResult escaped for a
// double-quoted shell string.
const CodexInitializeResultQuoted = `{\"userAgent\":\"` + CodexUserAgent + `\"}`
