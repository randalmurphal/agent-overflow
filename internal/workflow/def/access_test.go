package def

import "testing"

// TestEffectiveAccessDefaultsToReadOnly pins the direction of the default.
// An omitted `access:` must resolve to read-only in every consumer: it is the
// value that both withholds a worktree and restricts the provider session, so
// forgetting the field can only under-privilege a phase. Defaulting the other
// way would hand an unannotated, unattended phase write access to the project
// root — exactly what spec §9 / decision D22 forbid.
func TestEffectiveAccessDefaultsToReadOnly(t *testing.T) {
	if DefaultAccess != AccessReadOnly {
		t.Fatalf("DefaultAccess = %q, want read-only", DefaultAccess)
	}

	cases := []struct {
		name     string
		declared Access
		want     Access
	}{
		{"unset", "", AccessReadOnly},
		{"explicit read-only", AccessReadOnly, AccessReadOnly},
		{"explicit write", AccessWrite, AccessWrite},
		// A value that is neither resolves to the restrictive side. It is
		// already a validation finding; resolving it permissively would let a
		// typo widen a phase while the author waits for the error.
		{"unrecognised", Access("readonly"), AccessReadOnly},
		{"case mismatch", Access("Write"), AccessReadOnly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Phase{Access: tc.declared}).EffectiveAccess(); got != tc.want {
				t.Errorf("Phase.EffectiveAccess() = %q, want %q", got, tc.want)
			}
			if got := (Unit{Access: tc.declared}).EffectiveAccess(); got != tc.want {
				t.Errorf("Unit.EffectiveAccess() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPhaseWritesCoversPhaseJoinAndUnits proves the write predicate sees every
// place access can be declared. A join or fan-out unit that writes needs a
// worktree just as much as the phase itself.
func TestPhaseWritesCoversPhaseJoinAndUnits(t *testing.T) {
	cases := []struct {
		name  string
		phase Phase
		want  bool
	}{
		{"nothing declared", Phase{}, false},
		{"phase read-only", Phase{Access: AccessReadOnly}, false},
		{"phase writes", Phase{Access: AccessWrite}, true},
		{"join writes", Phase{Join: &Unit{ID: "j", Access: AccessWrite}}, true},
		{"join read-only", Phase{Join: &Unit{ID: "j"}}, false},
		{"one unit writes", Phase{FanOut: []Unit{{ID: "a"}, {ID: "b", Access: AccessWrite}}}, true},
		{"no unit writes", Phase{FanOut: []Unit{{ID: "a"}, {ID: "b", Access: AccessReadOnly}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.phase.Writes(); got != tc.want {
				t.Errorf("Phase.Writes() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDeriveWorkspaceNeedAgreesWithEffectiveAccess is the anti-drift guard
// that motivated extracting the helper: workspace provisioning and the
// session's runtime mode must read the same predicate. A workflow whose only
// writer is a fan-out unit or a join must still get a worktree, and a
// workflow with no writer at all must not.
func TestDeriveWorkspaceNeedAgreesWithEffectiveAccess(t *testing.T) {
	cases := []struct {
		name   string
		phases []Phase
		want   WorkspaceNeed
	}{
		{"no phases", nil, WorkspaceProjectRoot},
		{"all unset", []Phase{{ID: "a"}, {ID: "b"}}, WorkspaceProjectRoot},
		{"explicit read-only", []Phase{{ID: "a", Access: AccessReadOnly}}, WorkspaceProjectRoot},
		{"phase writes", []Phase{{ID: "a"}, {ID: "b", Access: AccessWrite}}, WorkspaceWorktree},
		{"unit writes", []Phase{{ID: "a", FanOut: []Unit{{ID: "u", Access: AccessWrite}}}}, WorkspaceWorktree},
		{"join writes", []Phase{{ID: "a", Join: &Unit{ID: "j", Access: AccessWrite}}}, WorkspaceWorktree},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveWorkspaceNeed(Workflow{Phases: tc.phases})
			if got != tc.want {
				t.Fatalf("DeriveWorkspaceNeed() = %q, want %q", got, tc.want)
			}
			// The two must never disagree: a worktree is provisioned exactly
			// when some phase's effective access is write.
			anyWrites := false
			for _, phase := range tc.phases {
				if phase.Writes() {
					anyWrites = true
				}
			}
			if anyWrites != (got == WorkspaceWorktree) {
				t.Errorf("workspace need %q disagrees with Phase.Writes() = %v", got, anyWrites)
			}
		})
	}
}
