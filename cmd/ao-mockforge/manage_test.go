package main

import (
	"encoding/json"
	"errors"
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

func TestGitLabManageActionsThroughTheAppsForgeCode(t *testing.T) {
	r := newRig(t, gitlabManageFixture())
	ref := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "manage", Number: 2}
	ctx := forgeapi.WithInteractive(t.Context())

	read := mergeRead(t, r, ref)
	m := read.Detail.Manage
	if got := reviewerStates(m.Reviewers); len(got) != 2 || got["bob"] != gitops.ReviewerPending || got["carol"] != gitops.ReviewerChangesRequested {
		t.Fatalf("reviewers = %+v", m.Reviewers)
	}
	if m.Reviewers[0].ID != "11" || !slices.Equal(labelNames(m.Labels), []string{"bug"}) || m.Labels[0].Color != "#d73a4a" {
		t.Fatalf("reviewer ids %+v, labels %+v", m.Reviewers, m.Labels)
	}
	want := gitops.PRManageCan{EditTitle: true, EditDescription: true, Reviewers: true, Assignees: true, Labels: true, Close: true,
		ConvertToDraft: true, Rerequest: true, Bypass: true}
	if m.Can != want {
		t.Fatalf("can = %+v, want %+v", m.Can, want)
	}
	if b, ok := findBlocker(read.Detail.Readiness, "changes_requested"); !ok || !b.Bypassable || b.Bypassed {
		t.Fatalf("changes requested blocker = %+v (%v)", b, ok)
	}

	// Title and description in one PUT; a stale description is refused
	// before it.
	title, body, stale := "Tidy up", "New body", "Older"
	before := len(r.engine.Invocations(0).Invocations)
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

	// Pickers.
	people, err := r.core.SearchPRCandidates(ctx, ref, gitops.CandidateAssignees, "o")
	if err != nil || !slices.Equal(logins(people.People), []string{"bob", "carol", "octo"}) || people.People[0].ID != "11" || people.More {
		t.Fatalf("assignee candidates = %+v, %v", people, err)
	}
	labels, err := r.core.SearchPRCandidates(ctx, ref, gitops.CandidateLabels, "ui")
	if err != nil || !slices.Equal(labelNames(labels.Labels), []string{"ui"}) {
		t.Fatalf("label candidates = %+v, %v", labels, err)
	}

	// Reviewers and assignees as full id sets, labels as add and remove
	// lists.
	if err := r.core.SetPRPeople(ctx, ref, true, gitops.PRPeopleChange{Remove: []gitops.PRPerson{{Login: "bob", ID: "11"}}}); err != nil {
		t.Fatalf("SetPRPeople reviewers: %v", err)
	}
	if got := decodeRaw(t, lastInvocation(t, r, "glab api update merge request").Body); len(got) != 1 || len(got["reviewer_ids"].([]any)) != 1 || got["reviewer_ids"].([]any)[0] != float64(12) {
		t.Fatalf("reviewers body = %v", got)
	}
	if err := r.core.SetPRPeople(ctx, ref, false, gitops.PRPeopleChange{Add: []gitops.PRPerson{{Login: "bob", ID: "11"}}}); err != nil {
		t.Fatalf("SetPRPeople assignees: %v", err)
	}
	if err := r.core.SetPRLabels(ctx, ref, gitops.PRLabelChange{Add: labels.Labels, Remove: []gitops.PRLabel{{Name: "bug"}}}); err != nil {
		t.Fatalf("SetPRLabels: %v", err)
	}
	if got := decodeRaw(t, lastInvocation(t, r, "glab api update merge request").Body); got["add_labels"] != "ui" || got["remove_labels"] != "bug" {
		t.Fatalf("labels body = %v", got)
	}
	read = mergeRead(t, r, ref)
	m = read.Detail.Manage
	if read.Detail.Title != title || read.Detail.Body != body || !slices.Equal(logins(peopleOf(m.Reviewers)), []string{"carol"}) ||
		!slices.Equal(logins(m.Assignees), []string{"bob"}) || !slices.Equal(labelNames(m.Labels), []string{"ui"}) {
		t.Fatalf("after the changes: title %q, body %q, manage %+v", read.Detail.Title, read.Detail.Body, m)
	}

	// Bypass: the blocker settles as bypassed, from the mergeability
	// check the read now selects.
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
	if routeCount(r, 0, "glab graphql PRManageChecks") == 0 {
		t.Fatal("no read selected the mergeability checks on a server that has them")
	}

	// Re-request carol: she is pending again, with the note.
	if err := r.core.RerequestPRReview(ctx, ref, gitops.PRPerson{Login: "carol", ID: "12"}); err != nil {
		t.Fatalf("RerequestPRReview: %v", err)
	}
	read = mergeRead(t, r, ref)
	if got := reviewerStates(read.Detail.Manage.Reviewers)["carol"]; got != gitops.ReviewerPending || lastEvent(read).Kind != gitops.EventReviewRequested {
		t.Fatalf("after re-request: carol %q, last event %+v", got, lastEvent(read))
	}
	if _, ok := findBlocker(read.Detail.Readiness, "changes_requested"); ok {
		t.Fatalf("blockers after the re-request = %+v", read.Detail.Readiness.Blockers)
	}

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
			t.Fatalf("after %s: state %q, events %+v", step.state, read.Detail.State, read.Events)
		}
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
	edited := gitops.MergeMessage{Title: "Tidy everything", Body: "Squashed."}
	if _, err := r.core.MergePR(ctx, ref, gitops.MergeRequest{Method: "squash", HeadSHA: strings.Repeat("2", 40), CommitMessage: &edited}); err != nil {
		t.Fatalf("MergePR with a message: %v", err)
	}
	if got := decodeRaw(t, lastInvocation(t, r, "glab api merge").Body); got["squash_commit_message"] != "Tidy everything\n\nSquashed." || got["merge_commit_message"] != nil {
		t.Fatalf("merge body = %v", got)
	}
}

// An older GitLab without the rereview mutation or the override offers
// neither, never selects the mergeability checks, and refuses both
// writes if sent anyway; a reporter may change nothing.
func TestGitLabManageOnAnOlderServerThroughTheAppsForgeCode(t *testing.T) {
	fixture := gitlabManageFixture()
	fixture.Repos[0].ServerLacks = []string{"rereview", "overrideRequestedChanges"}
	r := newRig(t, fixture)
	ref := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "manage", Number: 2}
	ctx := forgeapi.WithInteractive(t.Context())

	for range 2 {
		read := mergeRead(t, r, ref)
		if c := read.Detail.Manage.Can; c.Rerequest || c.Bypass || !c.Reviewers {
			t.Fatalf("an older server's flags = %+v", c)
		}
		if b, ok := findBlocker(read.Detail.Readiness, "changes_requested"); !ok || b.Bypassable {
			t.Fatalf("blocker = %+v (%v)", b, ok)
		}
	}
	if n := routeCount(r, 0, "glab graphql PRManageChecks"); n != 0 {
		t.Fatalf("PRManageChecks sent %d times to a server without it", n)
	}
	if got := explained(t, ref, gitops.ActionRerequest, r.core.RerequestPRReview(ctx, ref, gitops.PRPerson{Login: "carol", ID: "12"})); got.Kind != gitops.ForgeErrorRefused && got.Kind != gitops.ForgeErrorForge {
		t.Fatalf("rereview on an older server = %+v", got)
	}
	if got := explained(t, ref, gitops.ActionBypass, r.core.BypassPRRequestedChanges(ctx, ref)); got.Kind != gitops.ForgeErrorRefused && got.Kind != gitops.ForgeErrorForge {
		t.Fatalf("bypass on an older server = %+v", got)
	}

	fixture.Repos[0].Merge = &forgefake.RepoMerge{Permission: "reporter"}
	if _, err := r.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	read := mergeRead(t, r, ref)
	if c := read.Detail.Manage.Can; c != (gitops.PRManageCan{}) {
		t.Fatalf("a reporter's flags = %+v", c)
	}
	title := "Hijack"
	if got := explained(t, ref, gitops.ActionEdit, r.core.EditPR(ctx, ref, gitops.PREdit{Title: &title})); got.Kind != gitops.ForgeErrorForbidden && got.Kind != gitops.ForgeErrorRefused {
		t.Fatalf("a reporter's edit = %+v", got)
	}
	if mergeRead(t, r, ref).Detail.Title != "Tidy" {
		t.Fatal("a refused edit changed the title")
	}
}
