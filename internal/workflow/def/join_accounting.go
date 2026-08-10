package def

import (
	"fmt"
	"strings"
)

// The merge-join contract (`accounts_for_units: true` on a `join:`).
//
// A fan-out's join is the element that decides what SHIPS: it consolidates the
// lanes and its envelope is the phase's envelope, so whatever it does not
// mention simply does not exist downstream. The live failure this contract
// exists for is a merge join that stopped at its first conflict and reported a
// clean-looking result — an approved lane's work was neither merged nor named,
// and nothing in the run said so.
//
// The contract is deliberately a CONTRACT and not a merge driver. The engine
// never merges anything and never runs a script of its own; what it does is
// refuse a `done` join envelope whose `merged` / `blocked` lists are not
// exactly the units the join was run over. How the lanes are actually
// reconciled stays authorable content — see the reference script in the
// `port-campaign` starter.
const (
	// JoinMergedOutput lists the unit ids whose work the join took.
	JoinMergedOutput = "merged"
	// JoinBlockedOutput lists the units the join could not take, each with the
	// reason it could not.
	JoinBlockedOutput = "blocked"
	// JoinBlockedUnitField names the blocked unit.
	JoinBlockedUnitField = "unit"
	// JoinBlockedReasonField says why it was blocked.
	JoinBlockedReasonField = "reason"
)

// joinAccountingFindings holds an opted-in fan-out's PHASE outputs to the shape
// the engine will verify at run time. The declarations and the verification
// have to agree: a definition whose `merged` is declared as an array of numbers
// would validate clean and then fail every single join attempt, which is the
// class of error a dry-run exists to catch.
//
// The findings land on the PHASE element rather than on the join, because a
// join declares no outputs of its own — the phase's `outputs:` are what the
// author has to edit.
func joinAccountingFindings(phase Phase, phaseElement string) []Finding {
	if phase.Join == nil || !phase.Join.AccountsForUnits {
		return nil
	}
	outputs := PhaseOutputs(phase)
	var findings []Finding
	add := func(message string) {
		findings = append(findings, finding("join.accounting", phaseElement, message))
	}
	merged, declared := outputs[JoinMergedOutput]
	switch {
	case !declared:
		add(fmt.Sprintf("a join with accounts_for_units must answer an output %q; declare it on this phase as an array of unit ids", JoinMergedOutput))
	case merged.Optional:
		add(fmt.Sprintf("output %q must not be optional: a done join owes the list even when it is empty", JoinMergedOutput))
	case merged.Schema.Type != "array" || merged.Schema.Items == nil || merged.Schema.Items.Type != "string":
		add(fmt.Sprintf("output %q must be an array of strings; each entry is the id of a unit the join took", JoinMergedOutput))
	}
	blocked, declared := outputs[JoinBlockedOutput]
	switch {
	case !declared:
		add(fmt.Sprintf("a join with accounts_for_units must answer an output %q; declare it on this phase as an array of {%s, %s} objects",
			JoinBlockedOutput, JoinBlockedUnitField, JoinBlockedReasonField))
	case blocked.Optional:
		add(fmt.Sprintf("output %q must not be optional: a done join owes the list even when it is empty", JoinBlockedOutput))
	case blocked.Schema.Type != "array" || blocked.Schema.Items == nil || blocked.Schema.Items.Type != "object":
		add(fmt.Sprintf("output %q must be an array of objects; each entry names the unit that was not taken and why", JoinBlockedOutput))
	default:
		for _, field := range []string{JoinBlockedUnitField, JoinBlockedReasonField} {
			property, ok := blocked.Schema.Items.Properties[field]
			if !ok || property.Type != "string" {
				add(fmt.Sprintf("output %q items must declare a string property %q", JoinBlockedOutput, field))
				continue
			}
			if !contains(blocked.Schema.Items.Required, field) {
				add(fmt.Sprintf("output %q items must require %q", JoinBlockedOutput, field))
			}
		}
	}
	return findings
}

// accountingFindings verifies one `done` join envelope against the exact set of
// units the join ran over: every unit accounted for exactly once, and nothing
// accounted for that was not a unit of this fan-out.
//
// It reports nothing when the two lists are not readable as arrays. An absent,
// null, or wrongly-typed `merged` has already been reported once by the
// declared-output rules, and re-reporting it here as "every unit is
// unaccounted for" would bury the one finding the join has to act on under one
// per lane. That is the ONE thing it stays quiet about: a malformed ENTRY is
// reported where it sits, because nothing else will. The provider's schema
// catches it for an agent join, but a tool join hand-writes its envelope under
// no schema at all, and an entry that is not a usable unit id would otherwise
// be skipped in silence and survive the retry that fixed everything named.
func (c EnvelopeContract) accountingFindings(outputs map[string]any) []EnvelopeFinding {
	merged, mergedOK := outputs[JoinMergedOutput].([]any)
	blocked, blockedOK := outputs[JoinBlockedOutput].([]any)
	if !mergedOK || !blockedOK {
		return nil
	}
	expected := make(map[string]bool, len(c.accounted))
	for _, id := range c.accounted {
		expected[id] = true
	}
	seen := make(map[string]bool, len(c.accounted))
	var findings []EnvelopeFinding
	report := func(path, message string) {
		findings = append(findings, EnvelopeFinding{Path: path, Message: message})
	}
	// claim records one accounting entry, or says why it is not one. An id it
	// cannot use is reported rather than skipped: a silently ignored entry
	// leaves the unit it was meant to account for reported as missing, and the
	// junk that caused it survives the correction.
	claim := func(path string, value any) {
		id, ok := value.(string)
		switch {
		case !ok:
			report(path, fmt.Sprintf("must be a unit id string, not %T", value))
		case strings.TrimSpace(id) == "":
			report(path, "must be a unit id; a blank entry accounts for nothing")
		case !expected[id]:
			report(path, fmt.Sprintf("unit %q is not one of the units this join ran over", id))
		case seen[id]:
			report(path, fmt.Sprintf("unit %q is accounted for more than once; a unit is either merged or blocked, never both and never twice", id))
		default:
			seen[id] = true
		}
	}
	for index, entry := range merged {
		claim(fmt.Sprintf("$.outputs.%s[%d]", JoinMergedOutput, index), entry)
	}
	for index, entry := range blocked {
		path := fmt.Sprintf("$.outputs.%s[%d]", JoinBlockedOutput, index)
		object, ok := entry.(map[string]any)
		if !ok {
			report(path, fmt.Sprintf("must be a {%s, %s} object, not %T", JoinBlockedUnitField, JoinBlockedReasonField, entry))
			continue
		}
		if reason, _ := object[JoinBlockedReasonField].(string); strings.TrimSpace(reason) == "" {
			report(path+"."+JoinBlockedReasonField,
				"must say why the unit could not be taken; a blocked unit with no reason is a lane nobody can repair")
		}
		claim(path+"."+JoinBlockedUnitField, object[JoinBlockedUnitField])
	}
	// Ordered by the accounted set rather than by map iteration, so two runs of
	// the same failure produce the same feedback text.
	for _, id := range c.accounted {
		if !seen[id] {
			findings = append(findings, EnvelopeFinding{Path: "$.outputs",
				Message: fmt.Sprintf("unit %q is neither merged nor blocked; every unit of this fan-out must appear in exactly one of the two lists", id)})
		}
	}
	return findings
}
