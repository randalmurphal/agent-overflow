package git

import (
	"testing"

	"agent-overflow/internal/testutil"
)

func TestParseBranchList(t *testing.T) {
	// Inputs cover every behavior the parser is responsible for:
	//   - "main"            local, current, default, ahead 3 behind 2
	//   - "feature/demo"    local with no remote counterpart
	//   - "origin"          bare remote name (origin/HEAD symref collapsed
	//                       by refname:short); MUST be dropped
	//   - "origin/main"     remote shadow of a local branch; MUST be dropped
	//   - "origin/feature/demo"  remote shadow of a local branch; MUST be dropped
	//   - "origin/orphan"   remote-only branch; MUST appear as "orphan"
	//   - "origin/HEAD"     standard symref form (some git versions); MUST be dropped
	branches := parseBranchList(
		"main|*|/tmp/repo|ahead 3, behind 2\n"+
			"feature/demo| ||\n"+
			"origin| ||\n"+
			"origin/main| ||\n"+
			"origin/feature/demo| ||\n"+
			"origin/orphan| ||\n"+
			"origin/HEAD| ||\n",
		"main",
		[]string{"origin"},
	)

	var names []string
	byName := make(map[string]GitBranch)
	for _, b := range branches {
		names = append(names, b.Name)
		byName[b.Name] = b
	}

	want := []string{"main", "feature/demo", "orphan"}
	if len(names) != len(want) {
		t.Fatalf("branches = %v, want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Fatalf("branches[%d] = %q, want %q (full list: %v)", i, names[i], n, names)
		}
	}

	if !byName["main"].IsCurrent {
		t.Fatal("expected main to be current")
	}
	if !byName["main"].IsDefault {
		t.Fatal("expected main to be default")
	}
	if byName["main"].AheadCount != 3 || byName["main"].BehindCount != 2 {
		t.Fatalf("main ahead/behind = %d/%d, want 3/2", byName["main"].AheadCount, byName["main"].BehindCount)
	}
	if byName["feature/demo"].IsDefault {
		t.Fatal("expected feature/demo not to be default")
	}
	if byName["feature/demo"].IsCurrent {
		t.Fatal("expected feature/demo not to be current")
	}
	if byName["feature/demo"].AheadCount != 0 || byName["feature/demo"].BehindCount != 0 {
		t.Fatalf("feature/demo ahead/behind = %d/%d, want 0/0 (no upstream configured)", byName["feature/demo"].AheadCount, byName["feature/demo"].BehindCount)
	}
	if byName["orphan"].IsDefault {
		t.Fatal("expected orphan not to be default")
	}
	if byName["orphan"].AheadCount != 0 || byName["orphan"].BehindCount != 0 {
		t.Fatalf("orphan ahead/behind = %d/%d, want 0/0", byName["orphan"].AheadCount, byName["orphan"].BehindCount)
	}
}

func TestParseBranchListPreservesLocalNamedLikeBranch(t *testing.T) {
	// A local branch literally named "feature/HEAD" should pass through
	// (only remote-namespaced HEAD symrefs are dropped).
	branches := parseBranchList("feature/HEAD| ||\nfeature/regular| ||\n", "main", []string{"origin"})
	if len(branches) != 2 {
		t.Fatalf("expected 2 branches, got %d: %+v", len(branches), branches)
	}
}

func TestParseUpstreamTrack(t *testing.T) {
	tests := []struct {
		in     string
		ahead  int
		behind int
	}{
		{"", 0, 0},
		{"gone", 0, 0},
		{"ahead 3", 3, 0},
		{"behind 2", 0, 2},
		{"ahead 3, behind 2", 3, 2},
		{"behind 2, ahead 3", 3, 2},
		{"  ahead 7  ", 7, 0},
		{"ahead notanumber", 0, 0},
		// Defensive - truncated / extended output forms must not panic
		// or produce garbage counts.
		{"ahead", 0, 0},
		{"ahead 3 extra", 0, 0},
		{",", 0, 0},
	}
	for _, tt := range tests {
		ahead, behind := parseUpstreamTrack(tt.in)
		if ahead != tt.ahead || behind != tt.behind {
			t.Errorf("parseUpstreamTrack(%q) = %d/%d, want %d/%d", tt.in, ahead, behind, tt.ahead, tt.behind)
		}
	}
}

func TestParseBranchListProjectsRemoteOnlyDefault(t *testing.T) {
	// When the default branch only exists on the remote (fresh clone with
	// no local checkout yet of main), the projected "main" entry must
	// still be flagged as default so the picker keeps the badge.
	branches := parseBranchList(
		"feature| ||\norigin/main| ||\n",
		"main",
		[]string{"origin"},
	)
	if len(branches) != 2 {
		t.Fatalf("len(branches) = %d, want 2", len(branches))
	}
	var main GitBranch
	for _, b := range branches {
		if b.Name == "main" {
			main = b
		}
	}
	if main.Name != "main" {
		t.Fatalf("expected projected main branch, got names: %+v", branches)
	}
	if !main.IsDefault {
		t.Fatal("expected projected main to be default")
	}
}

func TestListBranchesOnRepository(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	branches, err := core.ListBranches(repo)
	if err != nil {
		t.Fatalf("ListBranches returned error: %v", err)
	}
	if len(branches) == 0 {
		t.Fatal("expected at least one branch")
	}
	if branches[0].Name == "" {
		t.Fatal("expected branch name to be populated")
	}
}

func TestListBranchesIncludesNewBranch(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "branch", "feature/test")

	core := NewCore()
	branches, err := core.ListBranches(repo)
	if err != nil {
		t.Fatalf("ListBranches returned error: %v", err)
	}

	found := false
	for _, b := range branches {
		if b.Name == "feature/test" {
			found = true
			if b.IsCurrent {
				t.Fatal("feature/test should not be current")
			}
		}
	}
	if !found {
		t.Fatal("expected feature/test in branch list")
	}
}

func TestIsDefaultBranchNameEdgeCases(t *testing.T) {
	tests := []struct {
		branch string
		dflt   string
		want   bool
	}{
		{"", "main", false},
		{"main", "", true},
		{"master", "", true},
		{"origin/main", "", true},
		{"origin/master", "", true},
		{"develop", "", false},
		{"develop", "develop", true},
		{"origin/develop", "develop", true},
		{"feature/develop", "develop", true}, // HasSuffix("/develop") matches remote-like patterns
	}

	for _, tt := range tests {
		t.Run(tt.branch+"/"+tt.dflt, func(t *testing.T) {
			got := isDefaultBranchName(tt.branch, tt.dflt)
			if got != tt.want {
				t.Fatalf("isDefaultBranchName(%q, %q) = %v, want %v", tt.branch, tt.dflt, got, tt.want)
			}
		})
	}
}
