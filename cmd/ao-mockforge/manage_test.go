package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// The review pane's management actions, through the app's own adapters
// against the engine: each write changes what the next read answers and
// leaves the event the forge would.

func lastInvocation(t *testing.T, r *rig, route string) forgefake.Invocation {
	t.Helper()
	all := r.engine.Invocations(0).Invocations
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Route == route {
			return all[i]
		}
	}
	t.Fatalf("no %q invocation", route)
	return forgefake.Invocation{}
}

func decodeRaw(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

func reviewerStates(reviewers []gitops.PRReviewer) map[string]string {
	out := map[string]string{}
	for _, r := range reviewers {
		out[r.Login] = r.State
	}
	return out
}

func logins(people []gitops.PRPerson) []string {
	out := []string{}
	for _, p := range people {
		out = append(out, p.Login)
	}
	return out
}

func labelNames(labels []gitops.PRLabel) []string {
	out := []string{}
	for _, l := range labels {
		out = append(out, l.Name)
	}
	return out
}

func lastEvent(read gitops.PRRead) gitops.ReviewEvent {
	if len(read.Events) == 0 {
		return gitops.ReviewEvent{}
	}
	return read.Events[len(read.Events)-1]
}

func githubManageFixture() forgefake.Fixture {
	return forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{{
		Forge: "github", Project: "acme/manage", ID: 7170, Organization: true,
		Members: []forgefake.Member{{Login: "bob", Name: "Bob"}, {Login: "carol", Name: "Carol"}, {Login: "octo"}},
		Teams:   []forgefake.Team{{Slug: "core", Name: "Core"}},
		Labels:  []forgefake.Label{{Name: "bug", Color: "#D73A4A"}, {Name: "ui"}},
		Pulls: []forgefake.Pull{{
			Number: 3, Title: "Tidy", Body: "Old body", Author: "octo", HeadRef: "tidy", HeadSHA: strings.Repeat("3", 40),
			Reviewers: []string{"bob"}, Labels: []string{"bug"}, Readiness: &forgefake.Readiness{RequiredApprovals: 1},
			Reviews: []forgefake.Review{{Author: "carol", AuthorName: "Carol", State: "CHANGES_REQUESTED"}},
		}},
	}}}
}

func TestGitHubManageActionsThroughTheAppsForgeCode(t *testing.T) {
	r := newRig(t, githubManageFixture())
	ref := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "manage", Number: 3}
	ctx := forgeapi.WithInteractive(t.Context())

	read := mergeRead(t, r, ref)
	m := read.Detail.Manage
	if got := reviewerStates(m.Reviewers); len(got) != 2 || got["bob"] != gitops.ReviewerPending || got["carol"] != gitops.ReviewerChangesRequested {
		t.Fatalf("reviewers = %+v", m.Reviewers)
	}
	if !slices.Equal(labelNames(m.Labels), []string{"bug"}) || m.Labels[0].Color != "#d73a4a" || m.Labels[0].ID == "" || len(m.Assignees) != 0 {
		t.Fatalf("labels %+v, assignees %+v", m.Labels, m.Assignees)
	}
	want := gitops.PRManageCan{EditTitle: true, EditDescription: true, Reviewers: true, Assignees: true, Labels: true, Close: true,
		ConvertToDraft: true, Rerequest: true, Dismiss: true, ReviewerTeams: true}
	if m.Can != want {
		t.Fatalf("can = %+v, want %+v", m.Can, want)
	}
	blocker, ok := findBlocker(read.Detail.Readiness, "changes_requested")
	if !ok || len(blocker.ChangeRequests) != 1 || blocker.ChangeRequests[0].AuthorLogin != "carol" || blocker.ChangeRequests[0].ReviewID == "" {
		t.Fatalf("changes requested blocker = %+v (%v)", blocker, ok)
	}

	// Title alone: the body is left out of the mutation.
	title := "Tidy up"
	if err := r.core.EditPR(ctx, ref, gitops.PREdit{Title: &title}); err != nil {
		t.Fatalf("EditPR title: %v", err)
	}
	if vars := decodeRaw(t, lastInvocation(t, r, "gh graphql EditPR").Variables); vars["title"] != title || vars["body"] != nil {
		t.Fatalf("EditPR variables = %v", vars)
	}
	// A description edit against a body that changed is refused before
	// any write; against the current one it applies.
	stale, body := "Older body", "New body"
	edits := len(r.engine.Invocations(0).Invocations)
	if err := r.core.EditPR(ctx, ref, gitops.PREdit{Description: &body, ExpectedDescription: &stale}); !errors.Is(err, gitops.ErrDescriptionChanged) {
		t.Fatalf("EditPR on a changed description = %v, want ErrDescriptionChanged", err)
	}
	for _, inv := range r.engine.Invocations(edits).Invocations {
		if inv.Route == "gh graphql EditPR" {
			t.Fatal("a stale description edit was sent")
		}
	}
	current := "Old body\r\n"
	if err := r.core.EditPR(ctx, ref, gitops.PREdit{Description: &body, ExpectedDescription: &current}); err != nil {
		t.Fatalf("EditPR description: %v", err)
	}
	read = mergeRead(t, r, ref)
	if read.Detail.Title != title || read.Detail.Body != body {
		t.Fatalf("after edits: title %q, body %q", read.Detail.Title, read.Detail.Body)
	}

	// Pickers: the candidates, bounded, teams included for an organization.
	people, err := r.core.SearchPRCandidates(ctx, ref, gitops.CandidateReviewers, "")
	if err != nil || people.More || !slices.Contains(logins(people.People), "acme/core") || !slices.Contains(logins(people.People), "carol") {
		t.Fatalf("reviewer candidates = %+v, %v", people, err)
	}
	labels, err := r.core.SearchPRCandidates(ctx, ref, gitops.CandidateLabels, "u")
	if err != nil || len(labels.Labels) != 2 { // "bug" and "ui" both hold a u
		t.Fatalf("label candidates = %+v, %v", labels, err)
	}
	ui := labels.Labels[slices.IndexFunc(labels.Labels, func(l gitops.PRLabel) bool { return l.Name == "ui" })]

	// Reviewers: one change adds the team and removes bob, a POST and a
	// DELETE.
	team := gitops.PRPerson{Login: "acme/core", Team: true}
	if err := r.core.SetPRPeople(ctx, ref, true, gitops.PRPeopleChange{Add: []gitops.PRPerson{team}, Remove: []gitops.PRPerson{{Login: "bob"}}}); err != nil {
		t.Fatalf("SetPRPeople reviewers: %v", err)
	}
	if body := decodeRaw(t, lastInvocation(t, r, "gh api request reviewers").Body); body["team_reviewers"] == nil {
		t.Fatalf("request reviewers body = %v", body)
	}
	if body := decodeRaw(t, lastInvocation(t, r, "gh api remove reviewers").Body); body["reviewers"] == nil {
		t.Fatalf("remove reviewers body = %v", body)
	}
	if err := r.core.SetPRPeople(ctx, ref, false, gitops.PRPeopleChange{Add: []gitops.PRPerson{{Login: "bob"}}}); err != nil {
		t.Fatalf("SetPRPeople assignees: %v", err)
	}
	if err := r.core.SetPRLabels(ctx, ref, gitops.PRLabelChange{Add: []gitops.PRLabel{ui}, Remove: []gitops.PRLabel{m.Labels[0]}}); err != nil {
		t.Fatalf("SetPRLabels: %v", err)
	}
	read = mergeRead(t, r, ref)
	m = read.Detail.Manage
	if got := reviewerStates(m.Reviewers); len(got) != 2 || got["acme/core"] != gitops.ReviewerPending || got["carol"] != gitops.ReviewerChangesRequested {
		t.Fatalf("reviewers after the change = %+v", m.Reviewers)
	}
	if !slices.Equal(logins(m.Assignees), []string{"bob"}) || !slices.Equal(labelNames(m.Labels), []string{"ui"}) {
		t.Fatalf("assignees %+v, labels %+v", m.Assignees, m.Labels)
	}
	if got := lastEvent(read); got.Kind != gitops.EventReviewRequested {
		t.Fatalf("last event after requesting a review = %+v", got)
	}
	// A removal is sent as asked, though this read no longer shows the
	// label: GitHub answers it.
	removes := routeCount(r, 0, "gh graphql RemoveLabels")
	if err := r.core.SetPRLabels(ctx, ref, gitops.PRLabelChange{Remove: []gitops.PRLabel{m.Labels[0]}}); err != nil {
		t.Fatalf("removing a label the PR no longer has: %v", err)
	}
	if n := routeCount(r, 0, "gh graphql RemoveLabels"); n != removes+1 {
		t.Fatalf("RemoveLabels sent %d times, want once more", n-removes)
	}
	// Teams are searched for reviewers only.
	teams := routeCount(r, 0, "gh graphql SearchTeams")
	if assignees, err := r.core.SearchPRCandidates(ctx, ref, gitops.CandidateAssignees, ""); err != nil || slices.ContainsFunc(assignees.People, func(p gitops.PRPerson) bool { return p.Team }) {
		t.Fatalf("assignee candidates = %+v, %v", assignees, err)
	}
	if n := routeCount(r, 0, "gh graphql SearchTeams"); n != teams {
		t.Fatalf("an assignee search asked for teams %d times", n-teams)
	}

	// Re-request carol's review; dismiss her change request and the
	// blocker goes.
	if err := r.core.RerequestPRReview(ctx, ref, gitops.PRPerson{Login: "carol"}); err != nil {
		t.Fatalf("RerequestPRReview: %v", err)
	}
	if got := reviewerStates(mergeRead(t, r, ref).Detail.Manage.Reviewers)["carol"]; got != gitops.ReviewerPending {
		t.Fatalf("carol after a re-request = %q", got)
	}
	if err := r.core.DismissPRReview(ctx, ref, blocker.ChangeRequests[0].ReviewID, "Addressed"); err != nil {
		t.Fatalf("DismissPRReview: %v", err)
	}
	read = mergeRead(t, r, ref)
	if _, ok := findBlocker(read.Detail.Readiness, "changes_requested"); ok || lastEvent(read).Kind != gitops.EventReviewDismissed {
		t.Fatalf("after dismiss: blockers %+v, last event %+v", read.Detail.Readiness.Blockers, lastEvent(read))
	}
	// The dismissed review no longer requests changes: a second dismiss
	// is refused before any write.
	if err := r.core.DismissPRReview(ctx, ref, blocker.ChangeRequests[0].ReviewID, "Again"); err == nil {
		t.Fatal("a second dismiss succeeded")
	}

	// Draft, close, reopen: each a state the next read shows, with its
	// event.
	for _, step := range []struct{ state, event, prState string }{
		{gitops.PRStateToDraft, gitops.EventMarkedDraft, "open"},
		{gitops.PRStateClose, gitops.EventClosed, "closed"},
		{gitops.PRStateReopen, gitops.EventReopened, "open"},
	} {
		if err := r.core.SetPRState(ctx, ref, step.state); err != nil {
			t.Fatalf("SetPRState %s: %v", step.state, err)
		}
		read = mergeRead(t, r, ref)
		if read.Detail.State != step.prState || lastEvent(read).Kind != step.event {
			t.Fatalf("after %s: state %q, last event %+v", step.state, read.Detail.State, lastEvent(read))
		}
	}
	if !read.Detail.Draft || read.Detail.Manage.Can.ConvertToDraft || read.Detail.Manage.Can.Reopen || !read.Detail.Manage.Can.Close {
		t.Fatalf("reopened draft: draft %v, can %+v", read.Detail.Draft, read.Detail.Manage.Can)
	}

	// The default squash message, then a merge with it edited: the fake
	// receives the headline and body.
	msg, err := r.core.PRMergeMessage(ctx, ref, "squash")
	if err != nil || msg.Title != "Tidy up (#3)" || msg.Body != body {
		t.Fatalf("PRMergeMessage = %+v, %v", msg, err)
	}
	if err := r.core.MarkPRReady(ctx, ref); err != nil {
		t.Fatalf("MarkPRReady: %v", err)
	}
	if err := r.core.ApprovePR(ctx, ref, strings.Repeat("3", 40)); err != nil {
		t.Fatalf("ApprovePR: %v", err)
	}
	edited := gitops.MergeMessage{Title: "Tidy everything (#3)", Body: "Squashed."}
	if _, err := r.core.MergePR(ctx, ref, gitops.MergeRequest{Method: "squash", HeadSHA: strings.Repeat("3", 40), CommitMessage: &edited}); err != nil {
		t.Fatalf("MergePR with a message: %v", err)
	}
	if vars := decodeRaw(t, lastInvocation(t, r, "gh graphql MergePRMessage").Variables); vars["commitHeadline"] != edited.Title || vars["commitBody"] != edited.Body {
		t.Fatalf("MergePRMessage variables = %v", vars)
	}
}

// What GitHub refuses reaches the app as a refusal and changes nothing:
// a reviewer GitHub will not take, a label of another repository, a
// viewer without triage, and the teams search of a token without
// read:org, which still answers the users.
func TestGitHubManageRefusalsThroughTheAppsForgeCode(t *testing.T) {
	fixture := githubManageFixture()
	fixture.Repos[0].TeamsRefused = true
	other := forgefake.Repo{Forge: "github", Project: "acme/other", ID: 7171, Labels: []forgefake.Label{{Name: "foreign", ID: "LA_foreign"}}}
	fixture.Repos = append(fixture.Repos, other)
	r := newRig(t, fixture)
	ref := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "manage", Number: 3}
	ctx := forgeapi.WithInteractive(t.Context())

	people, err := r.core.SearchPRCandidates(ctx, ref, gitops.CandidateReviewers, "")
	if err != nil || slices.ContainsFunc(people.People, func(p gitops.PRPerson) bool { return p.Team }) || !slices.Contains(logins(people.People), "bob") {
		t.Fatalf("candidates without read:org = %+v, %v; want the users alone", people, err)
	}
	// The author cannot be a reviewer: GitHub's 422.
	err = r.core.SetPRPeople(ctx, ref, true, gitops.PRPeopleChange{Add: []gitops.PRPerson{{Login: "octo"}}})
	if got := explained(t, ref, gitops.ActionReviewers, err); got.Kind != gitops.ForgeErrorRefused {
		t.Fatalf("author as reviewer = %+v", got)
	}
	err = r.core.SetPRLabels(ctx, ref, gitops.PRLabelChange{Add: []gitops.PRLabel{{ID: "LA_foreign", Name: "foreign"}}})
	if got := explained(t, ref, gitops.ActionLabels, err); got.Kind != gitops.ForgeErrorNotFound || routeCount(r, 0, "gh graphql AddLabels") != 0 {
		t.Fatalf("a foreign label = %+v, %d AddLabels sent", got, routeCount(r, 0, "gh graphql AddLabels"))
	}

	fixture.Repos[0].Merge = &forgefake.RepoMerge{Permission: "READ"}
	if _, err := r.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	read := mergeRead(t, r, ref)
	if c := read.Detail.Manage.Can; c.Reviewers || c.Assignees || c.Labels || c.Dismiss || c.Rerequest || c.EditTitle || c.Close {
		t.Fatalf("a reader's flags = %+v", c)
	}
	// A reader's review request is GitHub's 403; its assign is silently
	// ignored, as GitHub ignores assignees from a user without push.
	err = r.core.SetPRPeople(ctx, ref, true, gitops.PRPeopleChange{Add: []gitops.PRPerson{{Login: "carol"}}})
	if got := explained(t, ref, gitops.ActionReviewers, err); got.Kind != gitops.ForgeErrorForbidden && got.Kind != gitops.ForgeErrorRefused {
		t.Fatalf("a reader's review request = %+v", got)
	}
	if err := r.core.SetPRPeople(ctx, ref, false, gitops.PRPeopleChange{Add: []gitops.PRPerson{{Login: "bob"}}}); err != nil {
		t.Fatalf("a reader's assign = %v, want GitHub's silent success", err)
	}
	m := mergeRead(t, r, ref).Detail.Manage
	if got := logins(peopleOf(m.Reviewers)); len(got) != 2 || !slices.Contains(got, "bob") || !slices.Contains(got, "carol") || len(m.Assignees) != 0 {
		t.Fatalf("after refusals: reviewers %+v, assignees %+v", m.Reviewers, m.Assignees)
	}
}

// A PR's author may request reviews with read access: GitHub's review
// requests reference says pull request authors can. Dismissing stays
// with write access.
func TestGitHubAuthorWithReadRequestsReviews(t *testing.T) {
	fixture := githubManageFixture()
	fixture.Viewer = "octo"
	fixture.Repos[0].Merge = &forgefake.RepoMerge{Permission: "READ"}
	r := newRig(t, fixture)
	ref := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "manage", Number: 3}
	ctx := forgeapi.WithInteractive(t.Context())
	m := mergeRead(t, r, ref).Detail.Manage
	if !m.Can.Reviewers || !m.Can.Rerequest || m.Can.Dismiss {
		t.Fatalf("the author's flags = %+v", m.Can)
	}
	if err := r.core.SetPRPeople(ctx, ref, true, gitops.PRPeopleChange{Add: []gitops.PRPerson{{Login: "carol"}}}); err != nil {
		t.Fatalf("the author requesting a review: %v", err)
	}
	if got := reviewerStates(mergeRead(t, r, ref).Detail.Manage.Reviewers)["carol"]; got != gitops.ReviewerPending {
		t.Fatalf("carol after the author's request = %q", got)
	}
}

func peopleOf(reviewers []gitops.PRReviewer) []gitops.PRPerson {
	out := []gitops.PRPerson{}
	for _, r := range reviewers {
		out = append(out, r.PRPerson)
	}
	return out
}

func findBlocker(readiness gitops.MergeReadiness, kind string) (gitops.MergeBlocker, bool) {
	for _, b := range readiness.Blockers {
		if b.Kind == kind {
			return b, true
		}
	}
	return gitops.MergeBlocker{}, false
}

func gitlabManageFixture() forgefake.Fixture {
	return forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{{
		Forge: "gitlab", Project: "grp/manage", ID: 8180,
		Members: []forgefake.Member{{Login: "bob", Name: "Bob", ID: 11}, {Login: "carol", Name: "Carol", ID: 12}, {Login: "octo", ID: 13}},
		Labels:  []forgefake.Label{{Name: "bug", Color: "#D73A4A"}, {Name: "ui"}},
		Pulls: []forgefake.Pull{{
			Number: 2, Title: "Tidy", Body: "Old body", Author: "octo", HeadRef: "tidy", HeadSHA: strings.Repeat("2", 40),
			Reviewers: []string{"bob", "carol"}, ChangeRequests: []string{"carol"}, Labels: []string{"bug"},
		}},
	}}}
}

func routeCount(r *rig, since int, route string) int {
	n := 0
	for _, inv := range r.engine.Invocations(since).Invocations {
		if inv.Route == route {
			n++
		}
	}
	return n
}

func gitlabManageRef() gitops.PRReference {
	return gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "manage", Number: 2}
}

func findLabel(t *testing.T, labels []gitops.PRLabel, name string) gitops.PRLabel {
	t.Helper()
	for _, label := range labels {
		if label.Name == name {
			return label
		}
	}
	t.Fatalf("no label %s in %+v", name, labels)
	return gitops.PRLabel{}
}

func TestGitLabManageActionsThroughTheAppsForgeCode(t *testing.T) {
	r := newRig(t, gitlabManageFixture())
	ref := gitlabManageRef()
	ctx := forgeapi.WithInteractive(t.Context())

	// A cold host probes the schema, then reads with the checks it has;
	// a warm one reads alone.
	read := mergeRead(t, r, ref)
	if routeCount(r, 0, "glab graphql PRManageSchema") != 1 || routeCount(r, 0, "glab graphql PRManageChecks") != 1 || routeCount(r, 0, "glab graphql PRManage") != 0 {
		t.Fatalf("cold read routes = %v", r.engine.Invocations(0).Invocations)
	}
	m := read.Detail.Manage
	if got := reviewerStates(m.Reviewers); len(got) != 2 || got["bob"] != gitops.ReviewerPending || got["carol"] != gitops.ReviewerChangesRequested {
		t.Fatalf("reviewers = %+v", m.Reviewers)
	}
	bug := findLabel(t, m.Labels, "bug")
	if m.Reviewers[0].ID != "11" || !strings.HasPrefix(bug.ID, "gid://gitlab/ProjectLabel/") || bug.Color != "#d73a4a" || m.Unavailable != "" {
		t.Fatalf("reviewer ids %+v, labels %+v, unavailable %q", m.Reviewers, m.Labels, m.Unavailable)
	}
	want := gitops.PRManageCan{EditTitle: true, EditDescription: true, Reviewers: true, Assignees: true, Labels: true, Close: true,
		ConvertToDraft: true, Rerequest: true, Bypass: true}
	if m.Can != want {
		t.Fatalf("can = %+v, want %+v", m.Can, want)
	}
	if b, ok := findBlocker(read.Detail.Readiness, "changes_requested"); !ok || !b.Bypassable || b.Bypassed {
		t.Fatalf("changes requested blocker = %+v (%v)", b, ok)
	}
	before := len(r.engine.Invocations(0).Invocations)
	mergeRead(t, r, ref)
	if routeCount(r, before, "glab graphql PRManageSchema") != 0 || routeCount(r, before, "glab graphql PRManageChecks") != 1 {
		t.Fatalf("warm read routes = %v", r.engine.Invocations(before).Invocations)
	}

	// Title and description in one PUT; a stale description is refused
	// before it.
	title, body, stale := "Tidy up", "New body", "Older"
	before = len(r.engine.Invocations(0).Invocations)
	if err := r.core.EditPR(ctx, ref, gitops.PREdit{Title: &title, Description: &body, ExpectedDescription: &stale}); !errors.Is(err, gitops.ErrDescriptionChanged) {
		t.Fatalf("EditPR on a changed description = %v", err)
	}
	if n := routeCount(r, before, "glab api update merge request"); n != 0 {
		t.Fatalf("a stale edit sent %d updates", n)
	}
	current := "Old body"
	if err := r.core.EditPR(ctx, ref, gitops.PREdit{Title: &title, Description: &body, ExpectedDescription: &current}); err != nil {
		t.Fatalf("EditPR: %v", err)
	}
	if got := decodeRaw(t, lastInvocation(t, r, "glab api update merge request").Body); got["title"] != title || got["description"] != body || len(got) != 2 {
		t.Fatalf("edit body = %v", got)
	}

	// Pickers: members by REST, labels by GraphQL with their ids.
	people, err := r.core.SearchPRCandidates(ctx, ref, gitops.CandidateAssignees, "o")
	if err != nil || !slices.Equal(logins(people.People), []string{"bob", "carol", "octo"}) || people.People[0].ID != "11" || people.More {
		t.Fatalf("assignee candidates = %+v, %v", people, err)
	}
	labels, err := r.core.SearchPRCandidates(ctx, ref, gitops.CandidateLabels, "ui")
	if err != nil || !slices.Equal(labelNames(labels.Labels), []string{"ui"}) || labels.Labels[0].ID == "" {
		t.Fatalf("label candidates = %+v, %v", labels, err)
	}

	// Reviewers: remove bob and add octo, REMOVE then APPEND by username,
	// leaving whoever else is there; assignees: add bob, then remove him.
	if err := r.core.SetPRPeople(ctx, ref, true, gitops.PRPeopleChange{Add: []gitops.PRPerson{{Login: "octo"}}, Remove: []gitops.PRPerson{{Login: "bob"}}}); err != nil {
		t.Fatalf("SetPRPeople reviewers: %v", err)
	}
	var modes []string
	for _, inv := range r.engine.Invocations(0).Invocations {
		if inv.Route == "glab graphql PRSetReviewers" {
			vars := decodeRaw(t, inv.Variables)
			modes = append(modes, vars["mode"].(string)+" "+strings.Join(anyStrings(vars["usernames"]), ","))
		}
	}
	if !slices.Equal(modes, []string{"REMOVE bob", "APPEND octo"}) {
		t.Fatalf("reviewer writes = %q", modes)
	}
	if err := r.core.SetPRPeople(ctx, ref, false, gitops.PRPeopleChange{Add: []gitops.PRPerson{{Login: "bob"}, {Login: "carol"}}}); err != nil {
		t.Fatalf("SetPRPeople assignees: %v", err)
	}
	if err := r.core.SetPRPeople(ctx, ref, false, gitops.PRPeopleChange{Remove: []gitops.PRPerson{{Login: "bob"}}}); err != nil {
		t.Fatalf("SetPRPeople remove an assignee: %v", err)
	}
	if err := r.core.SetPRLabels(ctx, ref, gitops.PRLabelChange{Add: labels.Labels, Remove: []gitops.PRLabel{bug}}); err != nil {
		t.Fatalf("SetPRLabels: %v", err)
	}
	read = mergeRead(t, r, ref)
	m = read.Detail.Manage
	if read.Detail.Title != title || read.Detail.Body != body || !slices.Equal(logins(peopleOf(m.Reviewers)), []string{"carol", "octo"}) ||
		!slices.Equal(logins(m.Assignees), []string{"carol"}) || !slices.Equal(labelNames(m.Labels), []string{"ui"}) {
		t.Fatalf("after the changes: title %q, body %q, manage %+v", read.Detail.Title, read.Detail.Body, m)
	}
	if lastEventOf(read, gitops.EventReviewRequested).Ref != "octo" {
		t.Fatalf("events after adding a reviewer = %+v", read.Events)
	}

	// Bypass: the blocker settles as bypassed, from the mergeability check.
	if err := r.core.BypassPRRequestedChanges(ctx, ref); err != nil {
		t.Fatalf("BypassPRRequestedChanges: %v", err)
	}
	read = mergeRead(t, r, ref)
	settled := gitops.MergeReadiness{Blockers: read.Detail.Readiness.Settled}
	if b, ok := findBlocker(settled, "changes_requested"); !ok || !b.Bypassed || read.Detail.Manage.Can.Bypass {
		t.Fatalf("after bypass: settled %+v, can %+v", read.Detail.Readiness.Settled, read.Detail.Manage.Can)
	}
	if _, ok := findBlocker(read.Detail.Readiness, "changes_requested"); ok {
		t.Fatalf("after bypass: blockers %+v", read.Detail.Readiness.Blockers)
	}

	// Re-request carol: she is pending again, with the note.
	if err := r.core.RerequestPRReview(ctx, ref, gitops.PRPerson{Login: "carol", ID: "12"}); err != nil {
		t.Fatalf("RerequestPRReview: %v", err)
	}
	read = mergeRead(t, r, ref)
	if got := reviewerStates(read.Detail.Manage.Reviewers)["carol"]; got != gitops.ReviewerPending || lastEvent(read).Kind != gitops.EventReviewRequested {
		t.Fatalf("after re-request: carol %q, last event %+v", got, lastEvent(read))
	}

	// Draft is mergeRequestSetDraft; close and reopen are state events,
	// the reopen recorded as GitLab records it ("opened"). Each step adds
	// its event; the order among events of one millisecond is not
	// asserted, as notes and state events tie on it.
	kindCount := func(read gitops.PRRead, kind string) int {
		n := 0
		for _, event := range read.Events {
			if event.Kind == kind {
				n++
			}
		}
		return n
	}
	for _, step := range []struct{ state, event, prState string }{
		{gitops.PRStateToDraft, gitops.EventMarkedDraft, "open"},
		{gitops.PRStateClose, gitops.EventClosed, "closed"},
		{gitops.PRStateReopen, gitops.EventReopened, "open"},
	} {
		before := kindCount(read, step.event)
		if err := r.core.SetPRState(ctx, ref, step.state); err != nil {
			t.Fatalf("SetPRState %s: %v", step.state, err)
		}
		read = mergeRead(t, r, ref)
		if read.Detail.State != step.prState || kindCount(read, step.event) != before+1 {
			t.Fatalf("after %s: state %q, events %+v", step.state, read.Detail.State, read.Events)
		}
	}
	if vars := decodeRaw(t, lastInvocation(t, r, "glab graphql PRSetDraft").Variables); vars["draft"] != true {
		t.Fatalf("set draft variables = %v", vars)
	}
	if !read.Detail.Draft || read.Detail.Title != "Draft: "+title {
		t.Fatalf("draft: %v, title %q", read.Detail.Draft, read.Detail.Title)
	}

	msg, err := r.core.PRMergeMessage(ctx, ref, "merge")
	if err != nil || msg.Title != "Merge branch 'tidy' into 'main'" || !strings.Contains(msg.Body, "See merge request grp/manage!2") {
		t.Fatalf("merge message = %+v, %v", msg, err)
	}
	if err := r.core.MarkPRReady(ctx, ref); err != nil {
		t.Fatalf("MarkPRReady: %v", err)
	}
	if got := mergeRead(t, r, ref).Detail; got.Draft || got.Title != title {
		t.Fatalf("ready: draft %v, title %q", got.Draft, got.Title)
	}
	edited := gitops.MergeMessage{Title: "Tidy everything", Body: "Squashed."}
	if _, err := r.core.MergePR(ctx, ref, gitops.MergeRequest{Method: "squash", HeadSHA: strings.Repeat("2", 40), CommitMessage: &edited}); err != nil {
		t.Fatalf("MergePR with a message: %v", err)
	}
	if got := decodeRaw(t, lastInvocation(t, r, "glab api merge").Body); got["squash_commit_message"] != "Tidy everything\n\nSquashed." || got["merge_commit_message"] != nil {
		t.Fatalf("merge body = %v", got)
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, item := range v.([]any) {
		out = append(out, item.(string))
	}
	return out
}

func lastEventOf(read gitops.PRRead, kind string) gitops.ReviewEvent {
	for i := len(read.Events) - 1; i >= 0; i-- {
		if read.Events[i].Kind == kind {
			return read.Events[i]
		}
	}
	return gitops.ReviewEvent{}
}

// What GitLab does not apply is refused by name: a non-member reviewer, a
// label id it does not know.
func TestGitLabManageMissesAreRefusals(t *testing.T) {
	fixture := gitlabManageFixture()
	r := newRig(t, fixture)
	ref := gitlabManageRef()
	ctx := forgeapi.WithInteractive(t.Context())

	err := r.core.SetPRPeople(ctx, ref, true, gitops.PRPeopleChange{Add: []gitops.PRPerson{{Login: "eve"}}})
	if got := explained(t, ref, gitops.ActionReviewers, err); got.Kind != gitops.ForgeErrorRefused || !strings.Contains(got.Message, "eve") {
		t.Fatalf("a non-member reviewer = %+v", got)
	}
	err = r.core.SetPRLabels(ctx, ref, gitops.PRLabelChange{Add: []gitops.PRLabel{{ID: "gid://gitlab/ProjectLabel/999", Name: "gone"}}})
	if got := explained(t, ref, gitops.ActionLabels, err); got.Kind != gitops.ForgeErrorNotFound || !strings.Contains(got.Message, "gone") {
		t.Fatalf("an unknown label = %+v", got)
	}
	// Remove bob, then a refused add: the change is partial and says what
	// it did.
	err = r.core.SetPRPeople(ctx, ref, true, gitops.PRPeopleChange{Remove: []gitops.PRPerson{{Login: "bob"}}, Add: []gitops.PRPerson{{Login: "eve"}}})
	if got := explained(t, ref, gitops.ActionReviewers, err); !strings.HasPrefix(got.Message, "Removed bob from the reviewers of grp/manage!2; adding eve was refused: ") {
		t.Fatalf("partial = %+v", got)
	}
	if got := logins(peopleOf(mergeRead(t, r, ref).Detail.Manage.Reviewers)); !slices.Equal(got, []string{"carol"}) {
		t.Fatalf("reviewers after the partial change = %v", got)
	}
}

// A batch GitLab applied in part is a partial change naming both halves;
// an addition the answered list cannot show (it has a next page) may
// have applied, never a refusal.
func TestGitLabManagePartialAndInconclusiveBatches(t *testing.T) {
	fixture := gitlabManageFixture()
	r := newRig(t, fixture)
	ref := gitlabManageRef()
	ctx := forgeapi.WithInteractive(t.Context())

	err := r.core.SetPRPeople(ctx, ref, false, gitops.PRPeopleChange{Add: []gitops.PRPerson{{Login: "octo"}, {Login: "eve"}}})
	got := explained(t, ref, gitops.ActionAssignees, err)
	if _, partial := errors.AsType[*gitops.PartialWriteError](err); !partial || got.Kind != gitops.ForgeErrorRefused ||
		!strings.HasPrefix(got.Message, "Added octo to the assignees of grp/manage!2; adding eve was refused: GitLab did not add eve") {
		t.Fatalf("a batch applied in part = %v (%+v)", err, got)
	}

	// 100 reviewers: the answered list is full and has a next page.
	members := []forgefake.Member{}
	reviewers := []string{}
	for i := range 100 {
		login := fmt.Sprintf("m%03d", i)
		members = append(members, forgefake.Member{Login: login})
		reviewers = append(reviewers, login)
	}
	fixture.Repos[0].Members = append(fixture.Repos[0].Members, append(members, forgefake.Member{Login: "zed"})...)
	fixture.Repos[0].Pulls[0].Reviewers = reviewers
	fixture.Repos[0].Pulls[0].ChangeRequests = nil
	if _, err := r.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	err = r.core.SetPRPeople(ctx, ref, true, gitops.PRPeopleChange{Add: []gitops.PRPerson{{Login: "zed"}}})
	got = explained(t, ref, gitops.ActionReviewers, err)
	if _, unsure := errors.AsType[*gitops.UnconfirmedWriteError](err); !unsure || got.Kind != gitops.ForgeErrorUnconfirmed || strings.Contains(got.Message, "refused") {
		t.Fatalf("an addition the answer cannot show = %v (%+v)", err, got)
	}
	var full []any
	for _, inv := range r.engine.Invocations(0).Invocations {
		if inv.Route == "glab graphql PRSetReviewers" {
			full = append(full, inv)
		}
	}
	if len(full) != 1 {
		t.Fatalf("PRSetReviewers sent %d times", len(full))
	}
}

// An older GitLab without the rereview mutation, the override or the set
// mutations' operationMode offers none of them, never selects the
// mergeability checks, and refuses the writes if sent anyway (before
// sending the people and label ones); a reporter may change nothing.
func TestGitLabManageOnAnOlderServerThroughTheAppsForgeCode(t *testing.T) {
	fixture := gitlabManageFixture()
	fixture.Repos[0].ServerLacks = []string{"rereview", "overrideRequestedChanges", "operationMode"}
	r := newRig(t, fixture)
	ref := gitlabManageRef()
	ctx := forgeapi.WithInteractive(t.Context())

	for range 2 {
		read := mergeRead(t, r, ref)
		if c := read.Detail.Manage.Can; c.Rerequest || c.Bypass || c.Reviewers || c.Assignees || c.Labels || !c.EditTitle {
			t.Fatalf("an older server's flags = %+v", c)
		}
		if b, ok := findBlocker(read.Detail.Readiness, "changes_requested"); !ok || b.Bypassable {
			t.Fatalf("blocker = %+v (%v)", b, ok)
		}
	}
	if n := routeCount(r, 0, "glab graphql PRManageChecks"); n != 0 {
		t.Fatalf("PRManageChecks sent %d times to a server without it", n)
	}
	if n := routeCount(r, 0, "glab graphql PRManageSchema"); n != 1 {
		t.Fatalf("schema probes over two reads = %d, want 1 (a negative answer is cached)", n)
	}
	if got := explained(t, ref, gitops.ActionRerequest, r.core.RerequestPRReview(ctx, ref, gitops.PRPerson{Login: "carol", ID: "12"})); got.Kind != gitops.ForgeErrorRefused && got.Kind != gitops.ForgeErrorForge {
		t.Fatalf("rereview on an older server = %+v", got)
	}
	if got := explained(t, ref, gitops.ActionBypass, r.core.BypassPRRequestedChanges(ctx, ref)); got.Kind != gitops.ForgeErrorRefused && got.Kind != gitops.ForgeErrorForge {
		t.Fatalf("bypass on an older server = %+v", got)
	}
	if got := explained(t, ref, gitops.ActionReviewers, r.core.SetPRPeople(ctx, ref, true, gitops.PRPeopleChange{Add: []gitops.PRPerson{{Login: "octo"}}})); got.Kind != gitops.ForgeErrorRefused || routeCount(r, 0, "glab graphql PRSetReviewers") != 0 {
		t.Fatalf("reviewers on an older server = %+v", got)
	}

	fixture.Repos[0].Merge = &forgefake.RepoMerge{Permission: "reporter"}
	fixture.Repos[0].ServerLacks = nil
	if _, err := r.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	read := mergeRead(t, r, ref)
	if c := read.Detail.Manage.Can; c != (gitops.PRManageCan{}) {
		t.Fatalf("a reporter's flags = %+v", c)
	}
	title := "Hijack"
	if got := explained(t, ref, gitops.ActionEditPR, r.core.EditPR(ctx, ref, gitops.PREdit{Title: &title})); got.Kind != gitops.ForgeErrorForbidden && got.Kind != gitops.ForgeErrorRefused {
		t.Fatalf("a reporter's edit = %+v", got)
	}
	if mergeRead(t, r, ref).Detail.Title != "Tidy" {
		t.Fatal("a refused edit changed the title")
	}
}
