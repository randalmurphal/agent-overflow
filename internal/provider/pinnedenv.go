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
//	                      claude.StartLogin and the inactive-account probes.
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
//	CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH, CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS,
//	CLAUDE_CODE_TOOL_MEMORY_LIMIT
//	                    — rendered into `--settings` by inlineSettingsForCLI
//	                      from the Claude subagent-limit / tool-memory
//	                      settings; same flagSettings precedence, so a
//	                      value in the environment would be silently
//	                      ignored. CLAUDE_CODE_TOOL_MEMORY_LIMIT takes
//	                      effect only when the CLI runs on Linux (the WSL
//	                      backend included) — it is implemented as a
//	                      memory cgroup write.
//	CLAUDE_CODE_DISABLE_1M_CONTEXT
//	                    — not set by Agent Overflow at all, and reserved for
//	                      the opposite reason: when it IS set the CLI drops
//	                      the `[1m]` model suffix AO appends to request the
//	                      1M-token tier, so the thread's context-window axis
//	                      would report and bill against a window the session
//	                      never got. A silently-lied-about window is worse
//	                      than a refused save.
//	CLAUDE_CODE_RESUME_INTERRUPTED_TURN
//	                    — not set by Agent Overflow either, and reserved
//	                      because it changes what a RESUME keeps. With it
//	                      set, the CLI's deserialization additionally drops
//	                      the user rows carrying a shutdown's synthetic
//	                      tool_results (`shutdownUnwindResultsDoNotResolve`,
//	                      2.1.236+). AO's resume-filter mirror
//	                      (claude/sessionleaf_resumefilters.go) does not
//	                      model that pass, so under this variable it would
//	                      bless a `--resume-session-at` cursor the CLI is
//	                      about to discard — the pre-init "No message found
//	                      with message.uuid of:" brick that leaves a thread
//	                      unresumable (incident 2026-08-03). Refusing the
//	                      save is the honest answer until the mirror grows
//	                      the pass.
//	CLAUDE_CODE_HARBOR_KITE
//	                    — exported "1" by claude.NewSession when the
//	                      cross-session messaging setting is on
//	                      (options.go withClaudeCrossSessionEnv). It is
//	                      the CLI's one environment override for the
//	                      GrowthBook gate that binds the peer inbox, and
//	                      it goes in the real subprocess environment
//	                      rather than the `--settings` env block because
//	                      the bind runs during setup — the block's
//	                      re-application is not proven to precede it, and
//	                      the plain variable IS proven to work
//	                      (spike 2026-08-21, 2.1.237). Reserved because it
//	                      is the whole ON switch: a user value would make a
//	                      thread discoverable and addressable by every
//	                      other Claude session on the host while the
//	                      setting says the feature is off.
//	CLAUDE_CODE_SESSION_NAME
//	                    — not set by Agent Overflow, which passes `--name`
//	                      instead (both feed the same sanitizer and the
//	                      same peer registry). Reserved because the name is
//	                      DERIVED PER THREAD and kept in step with the
//	                      thread title through `/rename`: one value in the
//	                      custom environment would give every thread the
//	                      same peer-visible name, and the CLI's
//	                      collision-yield would then rename them out from
//	                      under the app.
//	CLAUDE_CODE_USER_DIALOG_TIMEOUT_MS
//	                    — not set by Agent Overflow either. It bounds how
//	                      long a HELD peer message waits before being
//	                      dropped with an expired receipt. AO always sends
//	                      an explicit `crossSessionInbound` (accept or
//	                      refuse, never hold or unset) precisely so nothing
//	                      is ever held, and reserving the name keeps a
//	                      custom value from quietly re-timing a drop that
//	                      produces no output at all.
//	CODEX_HOME          — cleared by codex.NewSession (session.go),
//	                      ProbeAccount / ProbeIdentity, the model catalog
//	                      fetcher, and the MCP status fetcher; set to a
//	                      temporary home by codex.StartLogin and the
//	                      inactive-account probes.
//
// CLAUDE_CODE_ENABLE_TODO_TOOLS is set WITHOUT being reserved, on purpose:
// claude.NewSession and claudetui's buildEnv default it to "true" (claude
// ≥2.1.233 removes the TodoWrite/Task* tools for modern models unless the
// session opts in, and AO's activity-rail todo list rides those events)
// but only when the merged environment does not already carry the name —
// so a user's custom environment can restore the vendor's stock tool
// surface by setting it to "false". Reserving it would turn a default
// into a mandate. CLAUDE_CODE_TODO_REMINDER_MODE follows the same rule:
// both spawn paths export "off" when the Settings nudge toggle asks for
// it (Config.DisableTodoReminders), and a user value in the custom
// environment outranks the setting.
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
			"CLAUDE_CODE_DISABLE_1M_CONTEXT",
			"CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH",
			"CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS",
			"CLAUDE_CODE_TOOL_MEMORY_LIMIT",
			"CLAUDE_CODE_RESUME_INTERRUPTED_TURN",
			"CLAUDE_CODE_HARBOR_KITE",
			"CLAUDE_CODE_SESSION_NAME",
			"CLAUDE_CODE_USER_DIALOG_TIMEOUT_MS",
		)
	case string(Codex):
		return append(shared, "CODEX_HOME")
	default:
		return shared
	}
}
