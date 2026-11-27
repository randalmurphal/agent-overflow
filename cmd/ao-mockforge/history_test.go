package main

import (
	"fmt"
	"slices"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// A repository's pull request history reads through the app's own forge
// code on both forges: GitHub's history pages, a pull request's
// commenting reviews, approvals and conversation comments past its first
// page, and an approval's line comments; GitLab's merge request list by
// update time, its approved and commented events and a merge request a
// comment names that the list did not reach. Each request is one the
// engine validates and each answer one the document's shape and the
// forge's schema admit.
func TestPRHistoryReadsThroughTheAppsForgeCode(t *testing.T) {
	now := time.Now().UTC()
	at := func(hours int) string { return now.Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339) }
	line := 1
	var approvals, commented []forgefake.Review
	var comments []forgefake.Comment
	for i := range 60 {
		approvals = append(approvals, forgefake.Review{Author: fmt.Sprintf("u%d", i), AuthorName: "U", State: "APPROVED", SubmittedAt: at(1)})
		commented = append(commented, forgefake.Review{Author: fmt.Sprintf("c%d", i), AuthorName: "C", State: "COMMENTED", SubmittedAt: at(2)})
		comments = append(comments, forgefake.Comment{Author: fmt.Sprintf("p%d", i), AuthorName: "P", Body: "ok", CreatedAt: at(2)})
	}
	approvals[0].ID = 9001
	github := []forgefake.Pull{
		{Number: 1, Title: "WEB-1 Fix the login", HeadRef: "fix-login", Author: "user1", AuthorName: "User One", State: "merged",
			CreatedAt: at(3), UpdatedAt: at(1), Reviews: append(slices.Clone(approvals), commented...), Comments: comments,
			Threads: []forgefake.Thread{{Path: "a.go", Line: &line, Comments: []forgefake.Comment{{Author: "u0", AuthorName: "U", Body: "nit", CreatedAt: at(1), Review: 9001}}}}},
		{Number: 2, Title: "Cache the session", HeadRef: "WEB-2-cache", CreatedAt: at(5), UpdatedAt: at(4)},
	}
	gitlab := []forgefake.Pull{
		{Number: 1, Title: "WEB-1 Fix the login", HeadRef: "fix-login", Author: "user1", AuthorName: "User One", State: "merged",
			CreatedAt: at(3), UpdatedAt: at(1), Reviews: approvals, Comments: comments},
		{Number: 2, Title: "Cache the session", HeadRef: "WEB-2-cache", CreatedAt: at(5), UpdatedAt: at(4)},
		{Number: 3, Title: "Old", HeadRef: "old", CreatedAt: at(24 * 201), UpdatedAt: at(24 * 200),
			Threads: []forgefake.Thread{{Path: "a.go", Line: &line, Comments: []forgefake.Comment{{Author: "user3", AuthorName: "User Three", Body: "late", CreatedAt: at(1)}}}}},
	}
	r := newRig(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web", Pulls: github},
		{Forge: "gitlab", Project: "grp/web", Pulls: gitlab},
	}})
	stored := func([]int64) (map[int64]gitops.StoredPR, error) { return nil, nil }
	for _, c := range []struct {
		repo      gitops.RepoReference
		numbers   []int64
		commented []string
	}{
		{gitops.RepoReference{Forge: "github", Host: "github.com", Namespace: "example", Repo: "web"}, []int64{1, 2}, []string{"c0", "p0", "u0"}},
		{gitops.RepoReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "web"}, []int64{1, 2, 3}, []string{"p0", "user3"}},
	} {
		h, err := r.core.ReadPRHistory(t.Context(), c.repo, gitops.PRHistoryInput{WindowStart: now.AddDate(0, 0, -180), Stored: stored})
		if err != nil {
			t.Fatalf("%s ReadPRHistory: %v", c.repo.Forge, err)
		}
		var numbers []int64
		for _, pr := range h.PRs {
			numbers = append(numbers, pr.Number)
		}
		approved, commenters := 0, map[string]bool{}
		for _, a := range h.Acts {
			if a.Approved {
				approved++
			} else {
				commenters[a.Person.Login] = true
			}
		}
		slices.Sort(numbers)
		missing := slices.DeleteFunc(slices.Clone(c.commented), func(login string) bool { return commenters[login] })
		if !slices.Equal(numbers, c.numbers) || h.Owed || approved != 60 || len(missing) > 0 || h.PRs[0].State == "" {
			t.Errorf("%s ReadPRHistory = %v, owed %v, %d approvals, commenters missing %v", c.repo.Forge, numbers, h.Owed, approved, missing)
		}
		if _, err := r.core.ReadPRHistory(t.Context(), c.repo, gitops.PRHistoryInput{Walk: h.Walk, WindowStart: now.AddDate(0, 0, -180), Stored: stored}); err != nil {
			t.Fatalf("%s steady ReadPRHistory: %v", c.repo.Forge, err)
		}
	}
	expectAllHandled(t, r)
}
