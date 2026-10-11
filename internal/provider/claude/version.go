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

// supportsLiveSystemPrompt reports whether this process's CLI will APPLY
// (not merely ack) a `set_model.system_prompt`: new enough to carry the
// handler, and not running under a prompt snapshot that would mask the
// swap on the next request (see spawnedWithSnapshotOff). The fallback for
// either is the restart, whose respawn passes the opt-out once the
// installed version is known.
func (s *Session) supportsLiveSystemPrompt() bool {
	version := s.CLIVersion()
	if !claudeCLIVersionAtLeast(version, minLiveSystemPromptCLIVersion) {
		return false
	}
	if claudeCLIVersionAtLeast(version, minSystemPromptSnapshotCLIVersion) && !s.spawnedWithSnapshotOff {
		return false
	}
	return true
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

// minSystemPromptSnapshotCLIVersion is the oldest Claude Code build that
// accepts `--system-prompt-snapshot`. Added in 2.1.267 (its changelog entry);
// an older build answers the flag with `error: unknown option` and exits 1,
// so the flag is only passed when the binary about to run is known to be at
// least this new.
const minSystemPromptSnapshotCLIVersion = "2.1.267"

// SystemPromptSnapshotArgs is the argv that turns the CLI's system prompt
// snapshot off, or nil when installedVersion is unknown or too old.
//
// With the snapshot on (the CLI default), the first request's rendered
// system prompt is recorded in the transcript and reused on every resume,
// so neither a changed `--append-system-prompt-file` nor a changed
// `--system-prompt-file` reaches a resumed session. Off, the prompt is
// rendered fresh on every start; the default body is byte-identical across
// resumes of one build (spike-verified 2.1.284, headless and TUI), so the
// prompt cache is unaffected. An unknown version omits the flag: the cost
// is one session whose later prompt changes wait for a fresh conversation,
// against a failed spawn on the other side.
func SystemPromptSnapshotArgs(installedVersion string) []string {
	if !claudeCLIVersionAtLeast(installedVersion, minSystemPromptSnapshotCLIVersion) {
		return nil
	}
	return []string{"--system-prompt-snapshot", "off"}
}
