package git

import "testing"

func TestBuildTemporaryWorktreeBranchName(t *testing.T) {
	branch := BuildTemporaryWorktreeBranchName()
	if !IsTemporaryWorktreeBranch(branch) {
		t.Fatalf("BuildTemporaryWorktreeBranchName() = %q, want forge/<8-hex>", branch)
	}
}

func TestIsTemporaryWorktreeBranch(t *testing.T) {
	tests := []struct {
		name   string
		branch string
		want   bool
	}{
		{name: "temporary", branch: "forge/a1b2c3d4", want: true},
		{name: "uppercase and padded", branch: " FORGE/A1B2C3D4 ", want: true},
		{name: "wrong prefix", branch: "feature/a1b2c3d4", want: false},
		{name: "wrong suffix chars", branch: "forge/not-hex!", want: false},
		{name: "wrong suffix length", branch: "forge/a1b2c3d4e5", want: false},
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
		{raw: "Fix Login!", want: "forge/fix-login"},
		{raw: "refs/heads/feature/reconnect", want: "forge/feature/reconnect"},
		{raw: "forge/quoted-branch", want: "forge/quoted-branch"},
		{raw: ` "  " `, want: "forge/update"},
	}

	for _, tt := range tests {
		if got := BuildGeneratedWorktreeBranchName(tt.raw); got != tt.want {
			t.Fatalf("BuildGeneratedWorktreeBranchName(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}
