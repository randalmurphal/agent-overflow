package claude

import (
	"strconv"
	"strings"

	"agent-overflow/internal/provider"
)

// version.go: the CLI-version gate behind the live thinking axis and the
// prompt snapshot flag, and the parser that reads a Claude version string.
//
// WHY THE THINKING AXIS HAS A FLOOR AT ALL. `set_max_thinking_tokens` appears
// in no changelog entry, so its floor is set by DIRECT INSPECTION of the
// shipped bundles rather than by release notes: 2.1.214, 2.1.219 and 2.1.237
// all carry the stdin-transport handler, and 2.1.214 is simply the oldest
// build available to inspect, not a boundary anyone observed.
//
// AN UNKNOWN VERSION IS TOO OLD, here and on `supportsSlashCommand`. The
// version arrives on `system/init`, so "unknown" means the session has not
// reached init yet; the fallback is the deferred restart that would converge
// the change anyway, so there is nothing to buy by being optimistic. Below
// the floor an unknown control subtype is answered with an ERROR response,
// thread error state for a change the user cannot connect to it.
//
// Prefer a `system/init.capabilities` token to a version gate whenever one
// exists for the behaviour (AGENTS.md §Capabilities); this one has none.

// minLiveThinkingCLIVersion is the oldest Claude Code build AO will send
// `set_max_thinking_tokens` to. See the file comment for how it was set.
const minLiveThinkingCLIVersion = "2.1.214"

// supportsLiveThinking reports whether this process's CLI carries the
// `set_max_thinking_tokens` handler.
func (s *Session) supportsLiveThinking() bool {
	return claudeCLIVersionAtLeast(s.CLIVersion(), minLiveThinkingCLIVersion)
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
