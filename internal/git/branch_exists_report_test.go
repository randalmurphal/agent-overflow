package git

import "testing"

func TestGitReportsBranchAlreadyExists(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stderr string
		branch string
		want   bool
	}{
		{"git branch", "fatal: a branch named 'review/new' already exists", "review/new", true},
		{"a lost race on the ref", "fatal: cannot lock ref 'refs/heads/review/new': reference already exists", "review/new", true},
		{"another branch", "fatal: a branch named 'main' already exists", "review/new", false},
		{
			"a name under an existing branch",
			"fatal: cannot lock ref 'refs/heads/review/new': 'refs/heads/review' exists; cannot create 'refs/heads/review/new'",
			"review/new", false,
		},
		{"an unrelated failure", "fatal: not a valid object name: 'nope'", "review/new", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gitReportsBranchAlreadyExists(tc.stderr, tc.branch); got != tc.want {
				t.Fatalf("gitReportsBranchAlreadyExists(%q) = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}
