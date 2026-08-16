package provider

import "strings"

// ReservedEnvNames lists the environment variables Agent Overflow sets or
// clears deliberately when it spawns a process for the named provider, and
// which a user-defined environment therefore must not override. It is the
// source of truth for that set: the settings layer keeps its own copy (it must
// not import this package — see internal/settings/AGENTS.md) and a
// root-package test asserts the two agree, so adding a name here fails the
// test gate until the deny-list follows.
//
// Where each name is pinned:
//
//	PATH                — assembled by the app's sessionProcessEnv (the
//	                      bundled `agent-overflow` CLI directory is prepended)
//	                      and merged additively by BuildEnvironment.
//	CLAUDE_CONFIG_DIR   — cleared by claude.NewSession (session.go),
//	                      claude.ProbeAccount (probe.go), and the MCP status
//	                      fetcher (mcpstatus.go); set to a temporary home by
//	                      claude.Login and the inactive-account probes.
//	CLAUDE_SECURESTORAGE_CONFIG_DIR
//	                    — cleared everywhere CLAUDE_CONFIG_DIR is. Claude
//	                      ≥2.1.220 keys its secure-storage (Keychain service)
//	                      naming off this variable when present, overriding
//	                      CLAUDE_CONFIG_DIR — an inherited value would make a
//	                      temporary-home probe write its rotated single-use
//	                      token into the CANONICAL account's Keychain item.
//	CLAUDE_CODE_ENTRYPOINT
//	                    — pinned to "agent-overflow" by
//	                      withClaudeCodeEntrypoint (session.go).
//	CLAUDE_AUTOCOMPACT_PCT_OVERRIDE, CLAUDE_CODE_AUTO_COMPACT_WINDOW
//	                    — rendered into `--settings` by inlineSettingsForCLI
//	                      (options.go); Claude's flagSettings source outranks
//	                      the subprocess environment, so a value in the
//	                      environment would be silently ignored.
//	CODEX_HOME          — cleared by codex.NewSession (session.go),
//	                      ProbeAccount / ProbeIdentity, the model catalog
//	                      fetcher, and the MCP status fetcher; set to a
//	                      temporary home by codex.Login and the
//	                      inactive-account probes.
//
// CLAUDE_CODE_ENABLE_TODO_TOOLS is set WITHOUT being reserved, on purpose:
// claude.NewSession and claudetui's buildEnv default it to "true" (claude
// ≥2.1.233 removes the TodoWrite/Task* tools for modern models unless the
// session opts in, and AO's activity-rail todo list rides those events)
// but only when the merged environment does not already carry the name —
// so a user's custom environment can restore the vendor's stock tool
// surface by setting it to "false". Reserving it would turn a default
// into a mandate.
//
// ANTHROPIC_BASE_URL is the one variable Agent Overflow pins WITHOUT reserving
// it. claudetui's buildEnv (claudetui/launch.go) owns the child's copy because
// the interactive CLI must talk to the per-session loopback gateway — but
// redirecting the backend is the reason the custom environment exists, so the
// app hands a user-configured base URL to that gateway's upstream instead of
// letting the pin swallow it. Reserving the name would break the feature's
// primary use case on the headless provider too.
//
// The AO_ prefix (the `agent-overflow` CLI session contract, the harness
// control channel, the mock provider, claudetui's hook relay) is reserved by
// prefix rather than by name and is deliberately not enumerated here.
func ReservedEnvNames(providerName string) []string {
	shared := []string{"PATH"}
	switch strings.TrimSpace(providerName) {
	case string(Claude), string(ClaudeTUI):
		return append(shared,
			"CLAUDE_CONFIG_DIR",
			"CLAUDE_SECURESTORAGE_CONFIG_DIR",
			"CLAUDE_CODE_ENTRYPOINT",
			"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE",
			"CLAUDE_CODE_AUTO_COMPACT_WINDOW",
		)
	case string(Codex):
		return append(shared, "CODEX_HOME")
	default:
		return shared
	}
}
