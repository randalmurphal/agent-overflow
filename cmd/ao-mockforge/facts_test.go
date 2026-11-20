package main

import (
	"fmt"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// The facts reads send the smallest document that holds a call's keys:
// every slot count of both forges reaches the engine through the app's
// forge code, each answer checked against its document and the forge's
// schema, each key answered.
func TestForgeFactsSendEveryDocument(t *testing.T) {
	var issues []forgefake.Issue
	for i := 1; i <= 160; i++ {
		issues = append(issues, forgefake.Issue{Number: 100 + i, Title: fmt.Sprintf("Issue %d", i)})
	}
	r := newRig(t, forgefake.Fixture{
		Viewer: "alice",
		Accounts: []forgefake.Account{
			{Forge: "github", Login: "octo", Name: "Octo"},
			{Forge: "github", Login: "acme", Kind: "org"},
			{Forge: "gitlab", Login: "alice", Name: "Alice"},
			{Forge: "gitlab", Login: "grp", Kind: "group", Name: "Group", Epics: []forgefake.Issue{{Number: 1, Title: "Epic"}}},
		},
		Repos: []forgefake.Repo{
			{Forge: "github", Project: "acme/facts", Issues: issues,
				Pulls: []forgefake.Pull{{Number: 1, Title: "Pull", Author: "octo", HeadRef: "p", HeadSHA: strings.Repeat("1", 40)}}},
			{Forge: "gitlab", Project: "grp/facts", Issues: issues,
				Pulls:      []forgefake.Pull{{Number: 1, Title: "MR", Author: "alice", HeadRef: "m", HeadSHA: strings.Repeat("1", 40)}},
				Labels:     []forgefake.Label{{Name: "bug", Color: "#d73a4a"}},
				Milestones: []forgefake.Milestone{{Title: "v1"}},
				Snippets:   []forgefake.Snippet{{ID: 3, Title: "Snip"}}},
		},
	})
	github := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "facts", Number: 1}
	gitlab := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "facts", Number: 1}
	next := 101
	refs := func(n int) []string {
		keys := make([]string, n)
		for i := range keys {
			keys[i] = fmt.Sprintf("ref:%%s#%d", next)
			next++
		}
		return keys
	}
	for _, tc := range []struct {
		pr    gitops.PRReference
		keys  []string
		route string
	}{
		{github, append([]string{"ref:acme/facts#1", "user:octo", "user:acme"}, refs(2)...), "gh graphql ForgeFacts8"},
		{github, refs(20), "gh graphql ForgeFacts32"},
		{github, refs(100), "gh graphql ForgeFacts128"},
		{gitlab, []string{"mr:grp/facts!1", "label:grp/facts~bug", "milestone:grp/facts%v1", "snippet:grp/facts$3"}, "glab graphql ForgeFacts4"},
		{gitlab, append([]string{"user:alice", "user:grp", "epic:grp&1"}, refs(7)...), "glab graphql ForgeFacts16"},
		{gitlab, refs(20), "glab graphql ForgeFacts32"},
	} {
		project := tc.pr.Project()
		for i, key := range tc.keys {
			tc.keys[i] = strings.ReplaceAll(key, "%s", project)
		}
		since := len(r.engine.Invocations(0).Invocations)
		got, err := r.core.ResolveFacts(t.Context(), t.TempDir(), tc.pr, gitops.PRRevision{}, tc.keys)
		if err != nil {
			t.Fatalf("%s: %v", tc.route, err)
		}
		for _, fact := range got.Facts {
			if fact.Status != gitops.FactFound {
				t.Errorf("%s: %s = %+v", tc.route, fact.Key, fact)
			}
		}
		if n := routeCount(r, since, tc.route); n != 1 || len(r.engine.Invocations(since).Invocations) != 1 {
			t.Fatalf("%d keys sent %d %s requests of %d, want one", len(tc.keys), n, tc.route, len(r.engine.Invocations(since).Invocations))
		}
	}
}
