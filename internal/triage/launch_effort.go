package triage

import (
	"encoding/json"
	"strings"
)

// launchStampKeys are the facts the Claude parser stamps on an Agent launch
// (and `inherited_effort` on a §E6 resume carrier) about the moment it
// happened (internal/provider/claude/subagent_effort.go): the effort the
// agent runs at absent a named one, the higher tiers it may be instead
// (claude-tui), the model the launch's alias resolved to, and the effort
// a claude-tui agent's first request ran at. The effort stamps may arrive
// with the agent's first model report. A later delivery of the same row
// would stamp a later moment, so a row's first state wins. An effort and its
// alternatives are one stamp: they settle together.
var launchStampGroups = []launchStampGroup{
	{keys: []string{"inherited_effort", "inherited_effort_alternatives"}},
	{keys: []string{"requested_model"}},
	{keys: []string{"run_effort"}, reported: true},
}

type launchStampGroup struct {
	keys []string
	// reported marks a stamp the agent's own request reported, not one read
	// off the launch moment: a row without it takes it from any delivery.
	reported bool
}

// launchStampFields picks the launch stamps out of a tool start's meta, so
// a model report that arrives before its launch row can be held for it.
func launchStampFields(meta json.RawMessage) map[string]json.RawMessage {
	if !mentionsLaunchStamp(string(meta)) {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(meta, &fields) != nil {
		return nil
	}
	stamps := map[string]json.RawMessage{}
	for _, group := range launchStampGroups {
		for _, key := range group.keys {
			if value, ok := fields[key]; ok {
				stamps[key] = value
			}
		}
	}
	if len(stamps) == 0 {
		return nil
	}
	return stamps
}

// holdLaunchStamps adds next to the stamps held for a row that does not
// exist yet. The stamps arrive in separate updates, so each stamp keeps its
// first value.
func holdLaunchStamps(held, next map[string]json.RawMessage) map[string]json.RawMessage {
	if len(held) == 0 {
		return next
	}
	for _, group := range launchStampGroups {
		settled := false
		for _, key := range group.keys {
			if _, ok := held[key]; ok {
				settled = true
			}
		}
		if settled {
			continue
		}
		for _, key := range group.keys {
			if value, ok := next[key]; ok {
				held[key] = value
			}
		}
	}
	return held
}

// keepLaunchStamps settles the launch stamps of a row whose tool start is
// delivered again (a later process, a mirror re-projection): the existing
// row's state wins in both directions, so a row stored without a stamp never
// gains one from a replay, except a reported stamp.
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
	for _, group := range launchStampGroups {
		had, has := false, false
		for _, key := range group.keys {
			_, inPrior := prior[key]
			_, inNext := next[key]
			had, has = had || inPrior, has || inNext
		}
		if !had && (mayAdd || group.reported || !has) {
			continue
		}
		for _, key := range group.keys {
			value, inPrior := prior[key]
			current, inNext := next[key]
			switch {
			case inPrior && string(current) != string(value):
				next[key] = value
				changed = true
			case !inPrior && inNext:
				delete(next, key)
				changed = true
			}
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
	for _, group := range launchStampGroups {
		for _, key := range group.keys {
			if strings.Contains(meta, `"`+key+`"`) {
				return true
			}
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
