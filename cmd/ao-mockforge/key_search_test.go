package main

import (
	"slices"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// A ticket's Work reads through the app's own forge code on both forges:
// the key search (GitHub's two aliased searches in one request, GitLab's
// title search in every state) and the lookup of a branch in every state,
// each request one the engine validates and each answer one the
// document's shape and the forge's schema admit.
func TestTicketWorkReadsThroughTheAppsForgeCode(t *testing.T) {
	pulls := []forgefake.Pull{
		{Number: 1, Title: "WEB-1 Fix the login", HeadRef: "fix-login", Author: "user1", AuthorName: "User 1"},
		{Number: 2, Title: "Cache the session", HeadRef: "WEB-1-cache", State: "merged"},
		{Number: 3, Title: "Move the host", HeadRef: "feature", State: "closed", Draft: true},
	}
	r := newRig(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web", Pulls: pulls},
		{Forge: "gitlab", Project: "grp/web", Pulls: pulls},
	}})
	for _, tc := range []struct {
		repo   gitops.RepoReference
		origin string
		found  []int
	}{
		{gitops.RepoReference{Forge: "github", Host: "github.com", Namespace: "example", Repo: "web"}, "git@github.com:example/web.git", []int{1, 2}},
		{gitops.RepoReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "web"}, "https://gitlab.com/grp/web.git", []int{1}},
	} {
		got, err := r.core.SearchPRsByKey(t.Context(), tc.repo, "WEB-1")
		if err != nil {
			t.Fatalf("%s SearchPRsByKey: %v", tc.repo.Forge, err)
		}
		var numbers []int
		for _, row := range got.Rows {
			numbers = append(numbers, row.Ref.Number)
		}
		if !slices.Equal(numbers, tc.found) || got.Rows[0].Author.Name != "User 1" || got.Rows[0].HeadRefName != "fix-login" {
			t.Errorf("%s SearchPRsByKey = %+v; want %v", tc.repo.Forge, got, tc.found)
		}
		byHead, err := r.core.ListPRsByHead(t.Context(), checkout(t, tc.origin), "feature")
		if err != nil || len(byHead) != 1 || byHead[0].Ref.Number != 3 || byHead[0].State != "closed" || !byHead[0].Draft {
			t.Errorf("%s ListPRsByHead = %+v, %v; want the closed draft 3", tc.repo.Forge, byHead, err)
		}
	}
	expectAllHandled(t, r)
}
