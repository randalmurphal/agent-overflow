package claude

import (
	"strconv"
	"strings"

	"agent-overflow/internal/provider"
)

// version.go — the CLI-version gates behind the live-update axes, and the
// parser that reads a Claude version string.
//
// WHY THERE ARE FLOORS AT ALL, once, for both axes below.
//
// Neither `set_max_thinking_tokens` nor `set_model.system_prompt` appears in
// any changelog entry (the latter's field is `@internal`), so both floors are
// set by DIRECT INSPECTION of the shipped bundles rather than by release
// notes: 2.1.214, 2.1.219 and 2.1.237 all carry the stdin-transport handler,
// and 2.1.214 is simply the oldest build available to inspect — not a boundary
// anyone observed.
//
// AN UNKNOWN VERSION IS TOO OLD, on both axes and on `supportsSlashCommand`.
// The version arrives on `system/init`, so "unknown" means the session has not
// reached init yet; the fallback in every case is the deferred restart that
// would converge the change anyway, so there is nothing to buy by being
// optimistic. The two axes' failure modes below the floor differ in severity
// but not in remedy: an unknown control subtype is answered with an ERROR
// response (thread error state for a change the user cannot connect to it),
// while `set_model.system_prompt`'s own doc states that older builds and other
// transports ACK SUCCESS WITHOUT APPLYING IT — AO would believe the session
// runs the new prompt while it runs the old one, with no wire signal either
// way. That second one is precisely the failure a version floor exists to
// prevent.
//
// Prefer a `system/init.capabilities` token to a version gate whenever one
// exists for the behaviour (AGENTS.md §Capabilities); neither of these has one.

// minLiveThinkingCLIVersion is the oldest Claude Code build AO will send
// `set_max_thinking_tokens` to. See the file comment for how it was set.
const minLiveThinkingCLIVersion = "2.1.214"

// supportsLiveThinking reports whether this process's CLI carries the
// `set_max_thinking_tokens` handler.
func (s *Session) supportsLiveThinking() bool {
	return claudeCLIVersionAtLeast(s.CLIVersion(), minLiveThinkingCLIVersion)
}

// minLiveSystemPromptCLIVersion is the oldest Claude Code build AO will send
// `set_model.system_prompt` to. See the file comment for how it was set and
// for why an older build's silent success is the dangerous case.
const minLiveSystemPromptCLIVersion = "2.1.214"

// supportsLiveSystemPrompt reports whether this process's CLI is new enough
// to APPLY (not merely ack) a `set_model.system_prompt`.
func (s *Session) supportsLiveSystemPrompt() bool {
	return claudeCLIVersionAtLeast(s.CLIVersion(), minLiveSystemPromptCLIVersion)
}

// claudeCLIVersionAtLeast compares two dotted Claude Code versions. An
// unparseable or empty `have` answers false — the caller's fallback is
// always the safe one.
//
// The ORDERING is provider.SemverAtLeast, shared with Codex's own version
// gate; only the parsing tolerance below is Claude's.
func claudeCLIVersionAtLeast(have, want string) bool {
	haveParts, ok := parseClaudeCLIVersion(have)
	if !ok {
		return false
	}
	wantParts, ok := parseClaudeCLIVersion(want)
	if !ok {
		return false
	}
	return provider.SemverAtLeast(haveParts, wantParts)
}

// parseClaudeCLIVersion reads the leading `major.minor.patch` of a version
// string. `claude --version` prints "2.1.237 (Claude Code)" while
// `system/init.claude_code_version` carries the bare number, so the trailing
// remainder is ignored rather than rejected.
func parseClaudeCLIVersion(version string) ([3]int, bool) {
	var parts [3]int
	fields := strings.SplitN(strings.TrimSpace(version), ".", 4)
	if len(fields) < 3 {
		return parts, false
	}
	for i := 0; i < 3; i++ {
		field := fields[i]
		if i == 2 {
			// "237 (Claude Code)" / "237-beta" — keep the leading digits.
			end := 0
			for end < len(field) && field[end] >= '0' && field[end] <= '9' {
				end++
			}
			field = field[:end]
		}
		n, err := strconv.Atoi(field)
		if err != nil || n < 0 {
			return parts, false
		}
		parts[i] = n
	}
	return parts, true
}
