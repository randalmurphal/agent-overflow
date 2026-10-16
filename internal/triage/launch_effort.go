package triage

import (
	"encoding/json"
	"strings"
)

// launchStampKeys are the facts the Claude parser stamps on an Agent launch
// (and `inherited_effort` on a §E6 resume carrier) about the moment it
// happened (internal/provider/claude/subagent_effort.go): the effort the
// launcher ran at and the model the launch's alias resolved to. A later
// delivery of the same row would stamp a later moment, so a row's first
// state wins.
var launchStampKeys = []string{"inherited_effort", "requested_model"}

// keepLaunchStamps settles the launch stamps of a row whose tool start is
// delivered again (a later process, a mirror re-projection): the existing
// row's state wins in both directions, so a row stored without a stamp never
// gains one from a replay.
func keepLaunchStamps(existing, merged string) string {
	return settleLaunchStamps(existing, merged, false)
}

// keepFirstLaunchStamps is keepLaunchStamps for a meta-only update, which
// is how a resume carrier receives its stamp: a row without one may gain it,
// and a row with one keeps it.
func keepFirstLaunchStamps(existing, merged string) string {
	return settleLaunchStamps(existing, merged, true)
}

func settleLaunchStamps(existing, merged string, mayAdd bool) string {
	if !mentionsLaunchStamp(existing) && (mayAdd || !mentionsLaunchStamp(merged)) {
		return merged
	}
	var prior, next map[string]json.RawMessage
	if json.Unmarshal([]byte(existing), &prior) != nil {
		return merged
	}
	if json.Unmarshal([]byte(merged), &next) != nil || next == nil {
		return merged
	}
	changed := false
	for _, key := range launchStampKeys {
		value, had := prior[key]
		current, has := next[key]
		switch {
		case had && string(current) != string(value):
			next[key] = value
			changed = true
		case !had && has && !mayAdd:
			delete(next, key)
			changed = true
		}
	}
	if !changed {
		return merged
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return merged
	}
	return string(encoded)
}

func mentionsLaunchStamp(meta string) bool {
	for _, key := range launchStampKeys {
		if strings.Contains(meta, `"`+key+`"`) {
			return true
		}
	}
	return false
}

// launchRequestedModel is the model a launch's `model` alias resolved to at
// launch (`requested_model`), "" when the parser could not resolve it.
func launchRequestedModel(launchMeta string) string {
	var stamps struct {
		RequestedModel string `json:"requested_model"`
	}
	if json.Unmarshal([]byte(launchMeta), &stamps) != nil {
		return ""
	}
	return strings.TrimSpace(stamps.RequestedModel)
}

// launchNamedEffort is the effort an original Agent launch named, copied
// onto its §E6 resume carrier with the rest of the agent's identity: the CLI
// records it with the agent and a resume runs at it. A fork's is ignored by
// the CLI and so is not named here. A relative effort ("lower", "higher") is
// returned as is: the frontend shows none for it. Empty when the launch
// named none, and the carrier's own `inherited_effort` applies.
func launchNamedEffort(input launchIdentity) string {
	if strings.TrimSpace(input.SubagentType) == "fork" {
		return ""
	}
	return strings.TrimSpace(input.Effort)
}
