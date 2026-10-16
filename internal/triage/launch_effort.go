package triage

import (
	"encoding/json"
	"strings"
)

// launchInheritedEffortKey is the effort a Claude Agent launch runs at
// when its input names none, stamped by the parser at launch
// (internal/provider/claude/subagent_effort.go). It records what was true
// when the launch happened.
const launchInheritedEffortKey = "inherited_effort"

// keepLaunchInheritedEffort keeps a launch row's first `inherited_effort`
// when its tool start is delivered again: a later process, or a mirror
// re-projection, would stamp the session's effort at that later moment.
func keepLaunchInheritedEffort(existing, merged string) string {
	if !strings.Contains(existing, `"`+launchInheritedEffortKey+`"`) {
		return merged
	}
	var prior map[string]json.RawMessage
	if json.Unmarshal([]byte(existing), &prior) != nil {
		return merged
	}
	value, ok := prior[launchInheritedEffortKey]
	if !ok {
		return merged
	}
	var next map[string]json.RawMessage
	if json.Unmarshal([]byte(merged), &next) != nil || next == nil {
		return merged
	}
	if string(next[launchInheritedEffortKey]) == string(value) {
		return merged
	}
	next[launchInheritedEffortKey] = value
	encoded, err := json.Marshal(next)
	if err != nil {
		return merged
	}
	return string(encoded)
}

// launchRunEffort is the effort an original Agent launch ran at, copied
// onto its §E6 resume carrier with the rest of the agent's identity: its
// `effort` input, which the CLI ignores for a fork, else the effort it
// inherited at launch. Empty when neither is known.
func launchRunEffort(launchMeta string, input launchIdentity) string {
	if effort := strings.TrimSpace(input.Effort); effort != "" && strings.TrimSpace(input.SubagentType) != "fork" {
		return effort
	}
	var stamp map[string]json.RawMessage
	if json.Unmarshal([]byte(launchMeta), &stamp) != nil {
		return ""
	}
	var inherited string
	if raw, ok := stamp[launchInheritedEffortKey]; ok && json.Unmarshal(raw, &inherited) == nil {
		return strings.TrimSpace(inherited)
	}
	return ""
}
