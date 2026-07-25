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
)

// grantSet is the closed membership test behind validation and the app's token
// scoping. `report-back` from §5 is deliberately absent in v1: its shape
// depends on profile-bound forge commands that are not settled, and admitting
// the name before the behaviour exists would let a workflow declare a
// capability nothing can honour.
var grantSet = map[Grant]struct{}{
	GrantStartRun:    {},
	GrantSchedule:    {},
	GrantUpdateNotes: {},
	GrantIntrospect:  {},
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
	if phase.Driver == DriverTool {
		findings = append(findings, finding("phase.grants", phaseElement,
			"grants require an agent driver: a tool phase runs a command, not a session that could hold the credentials"))
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
