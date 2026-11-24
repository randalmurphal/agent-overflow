package main

import "agent-overflow/internal/providerupdate/updatefake"

// versionString is printed for `--version` in BOTH impersonation modes
// until an `update` records a newer version (update.go); versionLine prints
// either in the same format.
// `claude --version` and `codex --version` arrive with identical argv
// (just the flag — no "app-server"), so one line must satisfy both
// probes:
//
//   - Claude: internal/provider/detect.go detectClaudeVersion accepts
//     any trimmed non-empty output (no minimum-version gate).
//   - Codex: internal/provider/codex_version.go ParseCodexCLIVersion
//     takes the FIRST word that is a Codex build and gates it against
//     MinimumCodexCLIVersion. Leading "99.0.0" parses as
//     99.0.0, far above the gate, and the trailing text is ignored.
//
// TestVersionSatisfiesBothProviders pins this against the real
// provider.DetectProvider for both names.
var versionString = versionLine(mockVersionNumber)

func versionLine(v string) string {
	return v + " (ao-mockprovider; impersonates Claude Code and codex-cli " + v + ")"
}

// mockVersionNumber is the bare semver used inside wire frames (e.g.
// system/init's claude_code_version), and the installed version until an
// update records another.
const mockVersionNumber = updatefake.Baseline
