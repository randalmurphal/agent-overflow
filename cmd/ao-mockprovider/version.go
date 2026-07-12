package main

// versionString is printed for `--version` in BOTH impersonation modes.
// `claude --version` and `codex --version` arrive with identical argv
// (just the flag — no "app-server"), so one line must satisfy both
// probes:
//
//   - Claude: internal/provider/detect.go detectClaudeVersion accepts
//     any trimmed non-empty output (no minimum-version gate).
//   - Codex: internal/provider/codex_version.go parseCodexCLIVersion
//     takes the FIRST semver-looking token and gates it against
//     minimumCodexCLIVersion (0.143.0). Leading "99.0.0" parses as
//     99.0.0, far above the gate, and the trailing text is ignored.
//
// TestVersionSatisfiesBothProviders pins this against the real
// provider.DetectProvider for both names.
const versionString = "99.0.0 (ao-mockprovider; impersonates Claude Code and codex-cli 99.0.0)"

// mockVersionNumber is the bare semver used inside wire frames (e.g.
// system/init's claude_code_version).
const mockVersionNumber = "99.0.0"
