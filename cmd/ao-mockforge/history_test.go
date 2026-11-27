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
// code on both forges: GitHub's history pages, a pull request's reviews
// past its first page, the repository's review comments and a pull
// request one of them names that the list did not reach; GitLab's merge
// request list by update time, its approved and commented events and a
// merge request a comment names. Each request is one the engine
// validates and each answer one the document's shape and the forge's
// schema admit.
func TestPRHistoryReadsThroughTheAppsForgeCode(t *testing.T) {
	now := time.Now().UTC()
	at := func(hours int) string { return now.Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339) }
	line := 1
	var reviews []forgefake.Review
	for i := range 60 {
		reviews = append(reviews, forgefake.Review{Author: fmt.Sprintf("u%d", i), AuthorName: "U", State: "APPROVED", SubmittedAt: at(1)})
	}
	pulls := []forgefake.Pull{
		{Number: 1, Title: "WEB-1 Fix the login", HeadRef: "fix-login", Author: "user1", AuthorName: "User 1", State: "merged",
			CreatedAt: at(3), UpdatedAt: at(2), Reviews: reviews,
			Comments: []forgefake.Comment{{Author: "user2", AuthorName: "User 2", Body: "ok", CreatedAt: at(2)}}},
		{Number: 2, Title: "Cache the session", HeadRef: "WEB-2-cache", CreatedAt: at(5), UpdatedAt: at(4)},
		{Number: 3, Title: "Old", HeadRef: "old", CreatedAt: at(24 * 201), UpdatedAt: at(24 * 200),
			Threads: []forgefake.Thread{{Path: "a.go", Line: &line, Comments: []forgefake.Comment{{Author: "user3", AuthorName: "User 3", Body: "late", CreatedAt: at(1)}}}}},
	}
	r := newRig(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web", Pulls: pulls},
		{Forge: "gitlab", Project: "grp/web", Pulls: pulls},
	}})
	for _, repo := range []gitops.RepoReference{
		{Forge: "github", Host: "github.com", Namespace: "example", Repo: "web"},
		{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "web"},
	} {
		h, err := r.core.ReadPRHistory(t.Context(), repo, gitops.PRHistoryInput{WindowStart: now.AddDate(0, 0, -180),
			Known: func([]int64) (map[int64]bool, error) { return nil, nil }})
		if err != nil {
			t.Fatalf("%s ReadPRHistory: %v", repo.Forge, err)
		}
		var numbers []int64
		approvals, lineComment := 0, false
		acts := slices.Clone(h.Acts)
		for _, pr := range h.PRs {
			numbers = append(numbers, pr.Number)
			acts = append(acts, pr.Acts...)
		}
		for _, a := range acts {
			if a.Approved {
				approvals++
			}
			lineComment = lineComment || a.Number == 3 && a.Person.Login == "user3" && !a.Approved
		}
		slices.Sort(numbers)
		if !slices.Equal(numbers, []int64{1, 2, 3}) || h.Owed || approvals != 60 || !lineComment || h.PRs[0].State == "" {
			t.Errorf("%s ReadPRHistory = %v, owed %v, %d approvals, line comment %v", repo.Forge, numbers, h.Owed, approvals, lineComment)
		}
		if _, err := r.core.ReadPRHistory(t.Context(), repo, gitops.PRHistoryInput{Walk: h.Walk, WindowStart: now.AddDate(0, 0, -180),
			Known: func([]int64) (map[int64]bool, error) { return nil, nil }}); err != nil {
			t.Fatalf("%s steady ReadPRHistory: %v", repo.Forge, err)
		}
	}
	expectAllHandled(t, r)
}
