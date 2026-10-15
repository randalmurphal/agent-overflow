package git

import "testing"

func TestCreatesPullRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		command string
		want    bool
	}{
		{`gh pr create --title "Add x" --body ""`, true},
		{`gh pr new --fill`, true},
		{`gh -R acme/tool pr create --fill`, true},
		{`cd /repo && gh pr create --fill | cat`, true},
		{`/usr/bin/zsh -lc "gh pr create --title 'x'"`, true},
		{`/usr/local/bin/gh pr create`, true},
		{`glab mr create --title x --yes`, true},
		{`glab mr new`, true},
		{"git push -u origin feat\nglab mr create -f", true},
		{`gh pr create`, true},
		{`gh pr view 46`, false},
		{`gh pr list --head feat`, false},
		{`glab mr view 12 -F json`, false},
		{`glab api projects/:id/merge_requests/12/discussions -X POST`, false},
		{`git push -u origin feat`, false},
		{`echo prepr create`, false},
		{`ghpr create`, false},
		{`gh issue create --title x`, false},
		{``, false},
	} {
		if got := CreatesPullRequest(tc.command); got != tc.want {
			t.Errorf("CreatesPullRequest(%q) = %v, want %v", tc.command, got, tc.want)
		}
	}
}
