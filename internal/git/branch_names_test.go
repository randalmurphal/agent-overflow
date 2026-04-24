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

func TestSanitizeBranchNamePreservingSlashes(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "feature/worktree", want: "feature/worktree"},
		{raw: "Feature Worktree", want: "feature-worktree"},
		{raw: "refs/heads/team/fix login", want: "team/fix-login"},
		{raw: "///bad//name///", want: "bad/name"},
	}

	for _, tt := range tests {
		if got := SanitizeBranchNamePreservingSlashes(tt.raw); got != tt.want {
			t.Fatalf("SanitizeBranchNamePreservingSlashes(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}
