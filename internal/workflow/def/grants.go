package def

import (
	"fmt"
	"sort"
	"strings"
)

// Grant names one first-party capability a phase's agent may exercise through
// the `ao` CLI (spec §5, D15). The set is closed: a grant is a token scope the
// app hands the phase's provider session, so a name the app cannot enforce is a
// validation finding rather than an ignored declaration.
type Grant string

const (
	// GrantStartRun lets the phase start runs and control the ones it started.
	GrantStartRun Grant = "start-run"
	// GrantSchedule lets the phase create an automation (§11).
	GrantSchedule Grant = "schedule"
	// GrantUpdateNotes lets the phase rewrite an automation's continuity notes.
	GrantUpdateNotes Grant = "update-notes"
	// GrantIntrospect lets the phase read run status, outputs, and listings
	// across its project.
	GrantIntrospect Grant = "introspect"
	// GrantResolve lets the phase decide the parks a run it started rests on:
	// approve/reject a human gate and answer a question. It is separate from
	// start-run on purpose — starting and stopping work is routine, while
	// answering a decision the workflow author routed to a human is authority
	// an author must hand out deliberately.
	GrantResolve Grant = "resolve"
	// GrantRemoteCommands permits commands on explicitly enabled paired peers.
	GrantRemoteCommands Grant = "remote-commands"
)

// grantSet is the closed membership test behind validation and the app's token
// scoping. `report-back` from §5 is deliberately absent in v1: its shape
// depends on profile-bound forge commands that are not settled, and admitting
// the name before the behaviour exists would let a workflow declare a
// capability nothing can honour.
var grantSet = map[Grant]struct{}{
	GrantStartRun:       {},
	GrantSchedule:       {},
	GrantUpdateNotes:    {},
	GrantIntrospect:     {},
	GrantResolve:        {},
	GrantRemoteCommands: {},
}

// KnownGrant reports whether name is a grant the system can enforce.
func KnownGrant(name string) bool {
	_, ok := grantSet[Grant(name)]
	return ok
}

// GrantNames returns the closed grant set, sorted, for diagnostics and the
// CLI's own help output.
func GrantNames() []string {
	names := make([]string, 0, len(grantSet))
	for grant := range grantSet {
		names = append(names, string(grant))
	}
	sort.Strings(names)
	return names
}

// validateGrants enforces the closed set, refuses duplicates, and refuses any
// grant on a phase that runs no provider session. A tool phase is a subprocess
// with no agent to hold credentials, so granting it a capability could only
// ever be a misunderstanding of what the phase does.
func validateGrants(phase Phase, phaseElement string) []Finding {
	if len(phase.Grants) == 0 {
		return nil
	}
	var findings []Finding
	if !phaseHoldsAgentSession(phase) {
		message := "grants require an agent driver: a tool phase runs a command, not a session that could hold the credentials"
		if phase.EffectiveShape() == ShapeFanOut {
			message = "grants require an agent session: every unit and the join of this fan-out runs a command, so nothing would hold the credentials"
		}
		findings = append(findings, finding("phase.grants", phaseElement, message))
	}
	seen := make(map[string]struct{}, len(phase.Grants))
	for _, name := range phase.Grants {
		trimmed := strings.TrimSpace(name)
		if !KnownGrant(trimmed) {
			findings = append(findings, finding("phase.grants", phaseElement,
				fmt.Sprintf("unknown grant %q; available grants are %s", name, strings.Join(GrantNames(), ", "))))
			continue
		}
		if _, duplicate := seen[trimmed]; duplicate {
			findings = append(findings, finding("phase.grants", phaseElement,
				fmt.Sprintf("grant %q is declared more than once", trimmed)))
			continue
		}
		seen[trimmed] = struct{}{}
	}
	return findings
}

// phaseHoldsAgentSession reports whether anything the phase runs is a provider
// session that could hold its injected credentials. A single-shape phase answers
// with its own driver; a fan-out answers with its units and its join, because
// the phase itself runs nothing and grants are still declared on it — the app
// scopes every unit's token from the *phase's* frozen grants
// (`frozenPhaseGrants`, keyed by phase id), so the declaration belongs there and
// is dead only when nothing under it runs an agent.
//
// A call unit contributes nothing here for the same reason a call phase grants
// nothing: it runs no session, and the child workflow's own phases declare what
// they may do.
func phaseHoldsAgentSession(phase Phase) bool {
	if phase.EffectiveShape() != ShapeFanOut {
		return phase.Driver != DriverTool
	}
	for _, unit := range phase.UnitDefinitions() {
		if unitRunsAgent(unit) {
			return true
		}
	}
	return phase.Join != nil && unitRunsAgent(*phase.Join)
}

func unitRunsAgent(unit Unit) bool {
	driver, runsWork := unit.EffectiveDriver()
	return runsWork && driver == DriverAgent
}
