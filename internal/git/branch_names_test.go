package git

import "testing"

func TestBuildTemporaryWorktreeBranchName(t *testing.T) {
	branch := BuildTemporaryWorktreeBranchName()
	if !IsTemporaryWorktreeBranch(branch) {
		t.Fatalf("BuildTemporaryWorktreeBranchName() = %q, want ao-<8-hex>", branch)
	}
}

func TestIsTemporaryWorktreeBranch(t *testing.T) {
	tests := []struct {
		name   string
		branch string
		want   bool
	}{
		{name: "temporary", branch: "ao-a1b2c3d4", want: true},
		{name: "uppercase and padded", branch: " AO-A1B2C3D4 ", want: true},
		{name: "wrong prefix", branch: "feature/a1b2c3d4", want: false},
		{name: "wrong suffix chars", branch: "ao-not-hex!", want: false},
		{name: "wrong suffix length", branch: "ao-a1b2c3d4e5", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTemporaryWorktreeBranch(tt.branch); got != tt.want {
				t.Fatalf("IsTemporaryWorktreeBranch(%q) = %v, want %v", tt.branch, got, tt.want)
			}
		})
	}
}

func TestBuildGeneratedWorktreeBranchName(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "Fix Login!", want: "ao-fix-login"},
		{raw: "refs/heads/feature/reconnect", want: "ao-feature-reconnect"},
		{raw: "ao-quoted-branch", want: "ao-quoted-branch"},
		{raw: ` "  " `, want: "ao-update"},
	}

	for _, tt := range tests {
		if got := BuildGeneratedWorktreeBranchName(tt.raw); got != tt.want {
			t.Fatalf("BuildGeneratedWorktreeBranchName(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestWorktreeBranchNameWithCustomPrefix(t *testing.T) {
	branch := BuildTemporaryWorktreeBranchNameWithPrefix("custom-")
	if !IsTemporaryWorktreeBranchWithPrefix(branch, "custom-") {
		t.Fatalf("BuildTemporaryWorktreeBranchNameWithPrefix() = %q, want custom-<8-hex>", branch)
	}

	if got := BuildGeneratedWorktreeBranchNameWithPrefix("Fix Login!", "custom-"); got != "custom-fix-login" {
		t.Fatalf("BuildGeneratedWorktreeBranchNameWithPrefix() = %q, want custom-fix-login", got)
	}
}

func TestBranchFragmentFromUserMessage(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain sentence", in: "Fix the reconnect bug", want: "Fix the reconnect bug"},
		{name: "keeps first line only", in: "First line\nsecond", want: "First line"},
		{name: "collapses whitespace", in: "too    many\tspaces", want: "too many spaces"},
		{name: "stops at first sentence", in: "First sentence. Second sentence.", want: "First sentence."},
		{name: "empty falls back", in: "   \t\n", want: "update"},
		{name: "question mark ends sentence", in: "Can you fix it? And then some", want: "Can you fix it?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BranchFragmentFromUserMessage(tt.in)
			if got != tt.want {
				t.Fatalf("BranchFragmentFromUserMessage(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFirstSentenceFromMessage(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "period boundary", in: "alpha. beta", want: "alpha."},
		{name: "exclamation boundary", in: "wow! more", want: "wow!"},
		{name: "question boundary", in: "huh? later", want: "huh?"},
		{name: "no terminator returns all", in: "no terminator here", want: "no terminator here"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := firstSentenceFromMessage(tt.in)
			if got != tt.want {
				t.Fatalf("firstSentenceFromMessage(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeBranchNamePreservingSlashes(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "feature/worktree", want: "feature/worktree"},
		{raw: "Feature Worktree", want: "Feature-Worktree"},
		{raw: "BLITZ-73", want: "BLITZ-73"},
		{raw: "refs/heads/team/fix login", want: "team/fix-login"},
		{raw: "///bad//name///", want: "bad/name"},
	}

	for _, tt := range tests {
		if got := SanitizeBranchNamePreservingSlashes(tt.raw); got != tt.want {
			t.Fatalf("SanitizeBranchNamePreservingSlashes(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}
