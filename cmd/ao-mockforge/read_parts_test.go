package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// A token that cannot read team reviewers (no read:org) or check runs
// (a fine-grained token) leaves those parts out of the PR read through
// the app's forge code: the PR publishes, the detail and the pipeline
// say what is not shown, the readiness has no checks row it cannot
// know, and the next read leaves the parts out at once.
func TestGitHubReadLeavesOutWhatTheTokenCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name        string
		teams       bool
		cannotRead  []string
		ticks       int // PRTick requests of the first read
		unavailable []string
		checksRow   bool
	}{
		{"teams and check runs", true, []string{"checks"}, 3, []string{
			"Team reviewers are not shown: your GitHub token lacks the `read:org` scope. Create a token with that scope and log gh in with it.",
			"Check runs are not shown: GitHub does not let this fine-grained token read them.",
		}, false},
		{"workflow runs", false, []string{"actions"}, 2, []string{
			"Workflow runs (each check's workflow, log and re-run) are not shown: GitHub does not let this fine-grained token read them.",
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{{
				Forge: "github", Project: "acme/parts", Organization: true, Teams: []forgefake.Team{{Slug: "core", Name: "Core"}},
				TeamsRefused: tc.teams, TokenCannotRead: tc.cannotRead,
				Pulls: []forgefake.Pull{{
					Number: 1, Title: "Parts", Author: "octo", HeadRef: "parts", HeadSHA: strings.Repeat("1", 40),
					TeamReviewers: []string{"acme/core"}, Reviewers: []string{"bob"},
					Readiness: &forgefake.Readiness{RequiredChecks: []string{"unit"}},
					CI:        &forgefake.Pipeline{ID: 900, Name: "CI", Jobs: []forgefake.Job{{ID: 9001, Name: "unit", Status: "success"}}},
					Events:    []forgefake.Event{{Kind: "review_requested", Ref: "acme/core", Actor: "octo", At: "2026-01-01T00:00:00Z"}},
				}},
			}}})
			ref := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "parts", Number: 1}
			read := mergeRead(t, r, ref)
			if got := routeCount(r, 0, "gh graphql PRTick"); got != tc.ticks {
				t.Fatalf("PRTick requests = %d, want %d", got, tc.ticks)
			}
			d := read.Detail
			if !slices.Equal(d.Unavailable, tc.unavailable) {
				t.Fatalf("unavailable = %q\nwant %q", d.Unavailable, tc.unavailable)
			}
			if d.Manage.Unavailable != "" || !slices.ContainsFunc(d.Manage.Reviewers, func(p gitops.PRReviewer) bool { return p.Login == "bob" }) {
				t.Fatalf("manage = %+v, want the user reviewers", d.Manage)
			}
			if d.Manage.Can.ReviewerTeams == tc.teams {
				t.Fatalf("team reviewers offered = %v with teams refused %v", d.Manage.Can.ReviewerTeams, tc.teams)
			}
			hasChecksRow := slices.ContainsFunc(append(d.Readiness.Blockers, d.Readiness.Settled...), func(b gitops.MergeBlocker) bool { return b.Kind == gitops.BlockerChecks })
			if hasChecksRow != tc.checksRow {
				t.Fatalf("readiness = %+v, want a checks row %v", d.Readiness, tc.checksRow)
			}
			if !strings.HasPrefix(read.CI.ActionsUnavailable, tc.unavailable[len(tc.unavailable)-1]) {
				t.Fatalf("pipeline actions unavailable = %q", read.CI.ActionsUnavailable)
			}

			since := len(r.engine.Invocations(0).Invocations)
			mergeRead(t, r, ref)
			if got := routeCount(r, since, "gh graphql PRTick"); got != 1 {
				t.Fatalf("the next read sent %d PRTick requests, want the one without the parts", got)
			}
			for _, inv := range r.engine.Invocations(since).Invocations {
				if inv.Route != "gh graphql PRTick" {
					continue
				}
				var vars map[string]any
				if err := json.Unmarshal(inv.Variables, &vars); err != nil {
					t.Fatal(err)
				}
				if vars["wantTeams"] == tc.teams || (vars["wantCheckRuns"] == false) != slices.Contains(tc.cannotRead, "checks") {
					t.Fatalf("the next read's variables = %v", vars)
				}
			}
		})
	}
}
