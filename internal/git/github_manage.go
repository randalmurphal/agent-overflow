package git

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// GitHub's management actions (ForgeManager) and the Details read they
// rest on. The read rides PRTick's detail (githubManageFields). Writes:
// the title and body are updatePullRequest after a read of the current
// body (GitHub takes no precondition on it); reviewers and assignees are
// REST, addressed by login, which GitHub checks itself, removals first;
// labels are removeLabelsFromLabelable / addLabelsToLabelable after one
// read that proves every label to add belongs to the PR's repository;
// close, reopen and convert to draft are their mutations; a dismissal
// checks that the review is one of the PR's standing change requests.

// githubManageFields is the Details half of the detail selection. The
// viewerCan* flags are the forge's own answer per control.
const githubManageFields = `viewerCanUpdate viewerCanClose viewerCanReopen viewerCanLabel viewerCanAssign ` +
	`reviewRequests(first: 100) { nodes { requestedReviewer { __typename ... on User { login name } ... on Bot { login } ... on Mannequin { login } ... on Team { combinedSlug name } } } } ` +
	`assignees(first: 100) { nodes { login name } } ` +
	`labels(first: 100) { nodes { id name color } }`

// githubPullManageRaw is the Details half of a PR's detail.
type githubPullManageRaw struct {
	ViewerCanUpdate bool `json:"viewerCanUpdate"`
	ViewerCanClose  bool `json:"viewerCanClose"`
	ViewerCanReopen bool `json:"viewerCanReopen"`
	ViewerCanLabel  bool `json:"viewerCanLabel"`
	ViewerCanAssign bool `json:"viewerCanAssign"`
	ReviewRequests  struct {
		Nodes []struct {
			RequestedReviewer *struct {
				Typename     string `json:"__typename"`
				Login        string `json:"login"`
				Name         string `json:"name"`
				CombinedSlug string `json:"combinedSlug"`
			} `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
	Assignees struct {
		Nodes []githubActorRaw `json:"nodes"`
	} `json:"assignees"`
	Labels struct {
		Nodes []githubLabelRaw `json:"nodes"`
	} `json:"labels"`
}

type githubLabelRaw struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

func (l githubLabelRaw) label() PRLabel {
	return PRLabel{ID: l.ID, Name: l.Name, Color: normalizeColor(l.Color)}
}

// githubRoleAtLeast is whether the repository's viewerPermission is at
// least role; a null permission (a GitHub App token) is unknown and
// answers false.
func githubRoleAtLeast(permission, role string) bool {
	order := []string{"READ", "TRIAGE", "WRITE", "MAINTAIN", "ADMIN"}
	have := slices.Index(order, strings.ToUpper(permission))
	return have >= 0 && have >= slices.Index(order, role)
}

// manage is the Details read: reviewers (each requested reviewer
// pending, each other reviewer at their latest review: an approval or a
// change request where one stands, else commented), assignees, labels and
// the viewer's capabilities, each from GitHub's own viewer field where the
// schema has one.
func (in githubMergeInputs) manage() PRManage {
	pull, repo := in.pull, in.repo
	open := in.open()
	closed := strings.EqualFold(pull.State, "CLOSED")
	out := PRManage{Reviewers: []PRReviewer{}, Assignees: []PRPerson{}, Labels: []PRLabel{}}
	seen := map[string]int{}
	add := func(r PRReviewer) {
		key := strings.ToLower(r.Login)
		if i, ok := seen[key]; ok {
			out.Reviewers[i] = r
			return
		}
		seen[key] = len(out.Reviewers)
		out.Reviewers = append(out.Reviewers, r)
	}
	// A reviewer's state and staleness come from one review: the standing
	// approval or change request when there is one, else the latest
	// review (a comment).
	opinions := map[string]githubOpinionRaw{}
	for _, review := range pull.LatestOpinionatedReviews.Nodes {
		opinions[strings.ToLower(review.Author.Login)] = review
	}
	stale := func(commit string) bool { return commit != "" && commit != pull.HeadRefOID }
	for _, review := range pull.LatestReviews.Nodes {
		login := review.Author.Login
		if login == "" || strings.EqualFold(login, pull.Author.Login) {
			continue
		}
		reviewer := PRReviewer{PRPerson: PRPerson{Login: login, Name: review.Author.Name}, State: ReviewerCommented, Stale: stale(review.Commit.OID)}
		if opinion, ok := opinions[strings.ToLower(login)]; ok {
			switch strings.ToUpper(opinion.State) {
			case "APPROVED":
				reviewer.State, reviewer.Stale = ReviewerApproved, stale(opinion.Commit.OID)
			case "CHANGES_REQUESTED":
				reviewer.State, reviewer.Stale = ReviewerChangesRequested, stale(opinion.Commit.OID)
			}
		}
		add(reviewer)
	}
	for _, node := range pull.ReviewRequests.Nodes {
		reviewer := node.RequestedReviewer
		if reviewer == nil {
			continue
		}
		person := PRPerson{Login: reviewer.Login, Name: reviewer.Name}
		if reviewer.Typename == "Team" {
			person = PRPerson{Login: reviewer.CombinedSlug, Name: reviewer.Name, Team: true}
		}
		if person.Login != "" {
			add(PRReviewer{PRPerson: person, State: ReviewerPending})
		}
	}
	for _, assignee := range pull.Assignees.Nodes {
		if assignee.Login != "" {
			out.Assignees = append(out.Assignees, PRPerson{Login: assignee.Login, Name: assignee.Name})
		}
	}
	for _, label := range pull.Labels.Nodes {
		out.Labels = append(out.Labels, label.label())
	}
	// Requesting a review and dismissing one have no viewer field: GitHub
	// gives them to triage and write access.
	triage := githubRoleAtLeast(repo.ViewerPermission, "TRIAGE")
	write := githubRoleAtLeast(repo.ViewerPermission, "WRITE")
	out.Can = PRManageCan{
		EditTitle:       pull.ViewerCanUpdate,
		EditDescription: pull.ViewerCanUpdate,
		Reviewers:       open && triage,
		Assignees:       pull.ViewerCanAssign,
		Labels:          pull.ViewerCanLabel,
		Close:           open && pull.ViewerCanClose,
		Reopen:          closed && pull.ViewerCanReopen,
		ConvertToDraft:  open && !pull.IsDraft && pull.ViewerCanUpdate,
		Rerequest:       open && triage,
		Dismiss:         open && write,
		ReviewerTeams:   repo.Owner != nil && repo.Owner.Typename == "Organization",
	}
	return out
}

const githubEditTargetQuery = `query PREditTarget($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) { pullRequest(number: $number) { id body } }
}`

const githubEditPRMutation = `mutation EditPR($pullRequestID: ID!, $title: String, $body: String) {
  updatePullRequest(input: {pullRequestId: $pullRequestID, title: $title, body: $body}) { pullRequest { id } }
}`

// EditPR reads the PR's node id and body, refuses a description edit
// whose expected body is no longer the PR's, and updates what the edit
// names (an unset variable leaves its field as it is).
func (f *githubForge) EditPR(ctx context.Context, ref PRReference, edit PREdit) error {
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	vars, err := githubPullVars(ref)
	if err != nil {
		return err
	}
	var raw struct {
		Repository *struct {
			PullRequest *struct {
				ID   string `json:"id"`
				Body string `json:"body"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if _, err := client.GraphQL(ctx, "PREditTarget", githubEditTargetQuery, vars, &raw); err != nil {
		return err
	}
	if raw.Repository == nil || raw.Repository.PullRequest == nil || raw.Repository.PullRequest.ID == "" {
		return fmt.Errorf("GitHub answered no pull request %s#%d on %s", ref.Project(), ref.Number, ref.Host)
	}
	pull := raw.Repository.PullRequest
	if edit.Description != nil && !sameDescription(pull.Body, *edit.ExpectedDescription) {
		return ErrDescriptionChanged
	}
	update := map[string]any{"pullRequestID": pull.ID}
	if edit.Title != nil {
		update["title"] = *edit.Title
	}
	if edit.Description != nil {
		update["body"] = *edit.Description
	}
	var answer struct {
		Update *struct {
			PullRequest *struct {
				ID string `json:"id"`
			} `json:"pullRequest"`
		} `json:"updatePullRequest"`
	}
	if _, err := client.GraphQL(ctx, "EditPR", githubEditPRMutation, update, &answer); err != nil {
		return writeOutcome(err)
	}
	if answer.Update == nil || answer.Update.PullRequest == nil {
		return &UnconfirmedWriteError{Err: fmt.Errorf("GitHub reported no pull request after editing %s#%d", ref.Project(), ref.Number)}
	}
	return nil
}

// githubAssigneesMax is how many assignees GitHub keeps on an issue or
// pull request.
const githubAssigneesMax = 10

// githubPeopleBody is a requested_reviewers or assignees body. A team is
// "owner/slug" with the repository's owner, sent as its slug.
func githubPeopleBody(ref PRReference, reviewers bool, people []PRPerson) (map[string][]string, error) {
	if !reviewers {
		return map[string][]string{"assignees": logins(people)}, nil
	}
	body := map[string][]string{"reviewers": {}, "team_reviewers": {}}
	for _, person := range people {
		if !person.Team {
			body["reviewers"] = append(body["reviewers"], person.Login)
			continue
		}
		owner, slug, _ := strings.Cut(person.Login, "/")
		if owner == "" || slug == "" || !strings.EqualFold(owner, ref.Namespace) {
			return nil, &ForgeActionError{Kind: ForgeErrorRefused, Message: fmt.Sprintf("%s is not a team of %s, so it cannot review %s. Search the reviewers again.", person.Login, ref.Namespace, ref.Project())}
		}
		body["team_reviewers"] = append(body["team_reviewers"], slug)
	}
	return body, nil
}

const githubAssigneesQuery = `query PRAssignees($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) { pullRequest(number: $number) { assignees(first: 100) { nodes { login } } } }
}`

// checkAssigneeLimit refuses an assignee change that would leave more
// than githubAssigneesMax assignees, before any write.
func checkAssigneeLimit(ctx context.Context, client *forgeapi.Client, ref PRReference, change PRPeopleChange) error {
	if len(change.Add) == 0 {
		return nil
	}
	vars, err := githubPullVars(ref)
	if err != nil {
		return err
	}
	var raw struct {
		Repository *struct {
			PullRequest *struct {
				Assignees struct {
					Nodes []githubActorRaw `json:"nodes"`
				} `json:"assignees"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if _, err := client.GraphQL(ctx, "PRAssignees", githubAssigneesQuery, vars, &raw); err != nil {
		return err
	}
	if raw.Repository == nil || raw.Repository.PullRequest == nil {
		return fmt.Errorf("GitHub answered no pull request %s#%d on %s", ref.Project(), ref.Number, ref.Host)
	}
	final := map[string]bool{}
	for _, assignee := range raw.Repository.PullRequest.Assignees.Nodes {
		final[strings.ToLower(assignee.Login)] = true
	}
	for _, person := range change.Remove {
		delete(final, strings.ToLower(person.Login))
	}
	for _, person := range change.Add {
		final[strings.ToLower(person.Login)] = true
	}
	if len(final) > githubAssigneesMax {
		return &ForgeActionError{Kind: ForgeErrorRefused, Message: fmt.Sprintf("GitHub takes at most %d assignees, and this change leaves %d on %s#%d. Remove some in the same change.", githubAssigneesMax, len(final), ref.Project(), ref.Number)}
	}
	return nil
}

// SetPRPeople removes, then adds: reviewers through
// pulls/N/requested_reviewers, assignees through issues/N/assignees
// (after checking GitHub's assignee limit). GitHub checks every login (a
// reviewer must be a collaborator, a team the repository's).
func (f *githubForge) SetPRPeople(ctx context.Context, ref PRReference, reviewers bool, change PRPeopleChange) error {
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	path, list := fmt.Sprintf("%s/issues/%d/assignees", githubRepoPath(ref.Repository()), ref.Number), "assignees of"
	if reviewers {
		path, list = githubPullPath(ref)+"/requested_reviewers", "reviewers of"
	}
	removeBody, err := githubPeopleBody(ref, reviewers, change.Remove)
	if err != nil {
		return err
	}
	addBody, err := githubPeopleBody(ref, reviewers, change.Add)
	if err != nil {
		return err
	}
	if !reviewers {
		if err := checkAssigneeLimit(ctx, client, ref, change); err != nil {
			return err
		}
	}
	step := func(method, verb string, people []PRPerson, body map[string][]string) changeStep {
		done, next := stepWords(verb, logins(people), list)
		return changeStep{done: done, next: next, run: func() error {
			_, err := client.JSON(ctx, forgeapi.Request{Method: method, Path: path, Body: body}, nil)
			return writeOutcome(err)
		}}
	}
	var steps []changeStep
	if len(change.Remove) > 0 {
		steps = append(steps, step(http.MethodDelete, "remove", change.Remove, removeBody))
	}
	if len(change.Add) > 0 {
		steps = append(steps, step(http.MethodPost, "add", change.Add, addBody))
	}
	return runSteps(steps)
}

const githubLabelTargetQuery = `query PRLabelTarget($owner: String!, $name: String!, $number: Int!, $ids: [ID!]!) {
  repository(owner: $owner, name: $name) { id pullRequest(number: $number) { id } }
  nodes(ids: $ids) { __typename ... on Label { id repository { id } } }
}`

const githubAddLabelsMutation = `mutation AddLabels($labelableID: ID!, $labelIDs: [ID!]!) {
  addLabelsToLabelable(input: {labelableId: $labelableID, labelIds: $labelIDs}) { clientMutationId }
}`

const githubRemoveLabelsMutation = `mutation RemoveLabels($labelableID: ID!, $labelIDs: [ID!]!) {
  removeLabelsFromLabelable(input: {labelableId: $labelableID, labelIds: $labelIDs}) { clientMutationId }
}`

// SetPRLabels reads the PR's node id and resolves every label to add,
// which must be one of the repository's, then removes and adds. A removal
// is sent as asked and GitHub answers it.
func (f *githubForge) SetPRLabels(ctx context.Context, ref PRReference, change PRLabelChange) error {
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	vars, err := githubPullVars(ref)
	if err != nil {
		return err
	}
	ids := func(labels []PRLabel) []string {
		out := make([]string, 0, len(labels))
		for _, label := range labels {
			out = append(out, label.ID)
		}
		return out
	}
	vars["ids"] = ids(change.Add)
	var raw struct {
		Repository *struct {
			ID          string `json:"id"`
			PullRequest *struct {
				ID string `json:"id"`
			} `json:"pullRequest"`
		} `json:"repository"`
		Nodes []*struct {
			Typename   string `json:"__typename"`
			ID         string `json:"id"`
			Repository *struct {
				ID string `json:"id"`
			} `json:"repository"`
		} `json:"nodes"`
	}
	_, err = client.GraphQL(ctx, "PRLabelTarget", githubLabelTargetQuery, vars, &raw)
	var gqlErr *forgeapi.GraphQLError
	// An id that resolves to nothing is a NOT_FOUND at its nodes index;
	// the check below refuses it by name.
	if err != nil && (!errors.As(err, &gqlErr) || !graphQLProblemsUnder(gqlErr, "nodes")) {
		return err
	}
	if raw.Repository == nil || raw.Repository.PullRequest == nil || raw.Repository.PullRequest.ID == "" {
		return fmt.Errorf("GitHub answered no pull request %s#%d on %s", ref.Project(), ref.Number, ref.Host)
	}
	for i, label := range change.Add {
		if i >= len(raw.Nodes) || raw.Nodes[i] == nil || raw.Nodes[i].Typename != "Label" || raw.Nodes[i].Repository == nil || raw.Nodes[i].Repository.ID != raw.Repository.ID {
			return &ForgeActionError{Kind: ForgeErrorNotFound, Message: fmt.Sprintf("The label %s is not one of %s's labels. Search the labels again.", firstNonEmpty(label.Name, label.ID), ref.Project())}
		}
	}
	pullID := raw.Repository.PullRequest.ID
	step := func(verb, op, doc string, labels []PRLabel) changeStep {
		done, next := stepWords(verb, labelNames(labels), "labels of")
		return changeStep{done: done, next: next, run: func() error {
			_, err := client.GraphQL(ctx, op, doc, map[string]any{"labelableID": pullID, "labelIDs": ids(labels)}, nil)
			return writeOutcome(err)
		}}
	}
	var steps []changeStep
	if len(change.Remove) > 0 {
		steps = append(steps, step("remove", "RemoveLabels", githubRemoveLabelsMutation, change.Remove))
	}
	if len(change.Add) > 0 {
		steps = append(steps, step("add", "AddLabels", githubAddLabelsMutation, change.Add))
	}
	return runSteps(steps)
}

// graphQLProblemsUnder is whether every problem's path starts at field.
func graphQLProblemsUnder(gqlErr *forgeapi.GraphQLError, field string) bool {
	if len(gqlErr.Problems) == 0 {
		return false
	}
	for _, problem := range gqlErr.Problems {
		if len(problem.Path) == 0 || problem.Path[0] != field {
			return false
		}
	}
	return true
}

const githubClosePRMutation = `mutation ClosePR($pullRequestID: ID!) {
  closePullRequest(input: {pullRequestId: $pullRequestID}) { pullRequest { state } }
}`

const githubReopenPRMutation = `mutation ReopenPR($pullRequestID: ID!) {
  reopenPullRequest(input: {pullRequestId: $pullRequestID}) { pullRequest { state } }
}`

const githubConvertToDraftMutation = `mutation ConvertPRToDraft($pullRequestID: ID!) {
  convertPullRequestToDraft(input: {pullRequestId: $pullRequestID}) { pullRequest { isDraft } }
}`

// SetPRState closes, reopens or converts the PR to a draft by its node
// id and reads the new state back.
func (f *githubForge) SetPRState(ctx context.Context, ref PRReference, state string) error {
	client, target, err := f.prTarget(ctx, ref)
	if err != nil {
		return err
	}
	op, doc, field := "ClosePR", githubClosePRMutation, "closePullRequest"
	switch state {
	case PRStateReopen:
		op, doc, field = "ReopenPR", githubReopenPRMutation, "reopenPullRequest"
	case PRStateToDraft:
		op, doc, field = "ConvertPRToDraft", githubConvertToDraftMutation, "convertPullRequestToDraft"
	}
	var raw map[string]*struct {
		PullRequest *struct {
			State   string `json:"state"`
			IsDraft bool   `json:"isDraft"`
		} `json:"pullRequest"`
	}
	if _, err := client.GraphQL(ctx, op, doc, map[string]any{"pullRequestID": target.ID}, &raw); err != nil {
		return writeOutcome(err)
	}
	answer := raw[field]
	if answer == nil || answer.PullRequest == nil {
		return &UnconfirmedWriteError{Err: fmt.Errorf("GitHub reported no pull request state after the change to %s#%d", ref.Project(), ref.Number)}
	}
	return nil
}

// RerequestReview requests a review again: GitHub asks a reviewer who
// already reviewed for a new one.
func (f *githubForge) RerequestReview(ctx context.Context, ref PRReference, reviewer PRPerson) error {
	return f.SetPRPeople(ctx, ref, true, PRPeopleChange{Add: []PRPerson{reviewer}})
}

// DismissReview dismisses reviewID, which must be one of the PR's
// standing change requests (latestOpinionatedReviews), with message.
func (f *githubForge) DismissReview(ctx context.Context, ref PRReference, reviewID, message string) error {
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	reviews, err := githubOpinions(ctx, client, ref)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(reviews, func(r githubOpinionRaw) bool {
		return r.ID == reviewID && strings.EqualFold(r.State, "CHANGES_REQUESTED")
	}) {
		return &ForgeActionError{Kind: ForgeErrorRefused, Message: "That review no longer requests changes on this pull request. Reload to see the reviews.", Err: errors.New("no standing change request with that id")}
	}
	var raw struct {
		Dismiss *struct {
			PullRequestReview *struct {
				State string `json:"state"`
			} `json:"pullRequestReview"`
		} `json:"dismissPullRequestReview"`
	}
	if _, err := client.GraphQL(ctx, "DismissReview", githubDismissReviewMutation, map[string]any{"reviewID": reviewID, "message": message}, &raw); err != nil {
		return writeOutcome(err)
	}
	if raw.Dismiss == nil || raw.Dismiss.PullRequestReview == nil || !strings.EqualFold(raw.Dismiss.PullRequestReview.State, "DISMISSED") {
		return &UnconfirmedWriteError{Err: fmt.Errorf("GitHub did not report review %s dismissed", reviewID)}
	}
	return nil
}

// githubOpinions reads the PR's standing approvals and change requests
// (ViewerApproval's document).
func githubOpinions(ctx context.Context, client *forgeapi.Client, ref PRReference) ([]githubOpinionRaw, error) {
	vars, err := githubPullVars(ref)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Repository *struct {
			PullRequest *struct {
				LatestOpinionatedReviews struct {
					Nodes []githubOpinionRaw `json:"nodes"`
				} `json:"latestOpinionatedReviews"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if _, err := client.GraphQL(ctx, "ViewerApproval", githubViewerApprovalQuery, vars, &raw); err != nil {
		return nil, err
	}
	if raw.Repository == nil || raw.Repository.PullRequest == nil {
		return nil, fmt.Errorf("GitHub answered no pull request %s#%d on %s", ref.Project(), ref.Number, ref.Host)
	}
	return raw.Repository.PullRequest.LatestOpinionatedReviews.Nodes, nil
}

// BypassRequestedChanges is GitLab's; GitHub dismisses a review instead.
func (f *githubForge) BypassRequestedChanges(context.Context, PRReference) error {
	return ErrUnsupportedForge
}

const githubSearchPeopleQuery = `query SearchAssignable($owner: String!, $name: String!, $query: String!) {
  repository(owner: $owner, name: $name) {
    owner { __typename login }
    assignableUsers(first: 20, query: $query) { pageInfo { hasNextPage } nodes { login name } }
  }
}`

const githubSearchTeamsQuery = `query SearchTeams($org: String!, $query: String!) {
  organization(login: $org) { teams(first: 20, query: $query) { pageInfo { hasNextPage } nodes { combinedSlug name } } }
}`

const githubSearchLabelsQuery = `query SearchLabels($owner: String!, $name: String!, $query: String!) {
  repository(owner: $owner, name: $name) { labels(first: 20, query: $query) { pageInfo { hasNextPage } nodes { id name color } } }
}`

// SearchCandidates answers the repository's assignable users (reviewers
// and assignees alike; GitHub requests reviews from the same set) and,
// for reviewers on an organization's repository, its teams in a request
// of their own, whose failure of any kind (a token without read:org)
// leaves the users. Labels are the repository's.
func (f *githubForge) SearchCandidates(ctx context.Context, ref PRReference, kind, query string) (PRCandidates, error) {
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return PRCandidates{}, err
	}
	owner, name, err := splitGitHubProject(ref.Project())
	if err != nil {
		return PRCandidates{}, err
	}
	vars := map[string]any{"owner": owner, "name": name, "query": query}
	out := PRCandidates{People: []PRPerson{}, Labels: []PRLabel{}}
	if kind == CandidateLabels {
		var raw struct {
			Repository *struct {
				Labels struct {
					PageInfo githubPageInfo   `json:"pageInfo"`
					Nodes    []githubLabelRaw `json:"nodes"`
				} `json:"labels"`
			} `json:"repository"`
		}
		if _, err := client.GraphQL(ctx, "SearchLabels", githubSearchLabelsQuery, vars, &raw); err != nil {
			return PRCandidates{}, err
		}
		if raw.Repository == nil {
			return PRCandidates{}, fmt.Errorf("GitHub answered no repository %s on %s", ref.Project(), ref.Host)
		}
		for _, label := range raw.Repository.Labels.Nodes {
			out.Labels = append(out.Labels, label.label())
		}
		out.More = raw.Repository.Labels.PageInfo.HasNextPage
		return out, nil
	}
	var raw struct {
		Repository *struct {
			Owner struct {
				Typename string `json:"__typename"`
				Login    string `json:"login"`
			} `json:"owner"`
			AssignableUsers struct {
				PageInfo githubPageInfo   `json:"pageInfo"`
				Nodes    []githubActorRaw `json:"nodes"`
			} `json:"assignableUsers"`
		} `json:"repository"`
	}
	if _, err := client.GraphQL(ctx, "SearchAssignable", githubSearchPeopleQuery, vars, &raw); err != nil {
		return PRCandidates{}, err
	}
	if raw.Repository == nil {
		return PRCandidates{}, fmt.Errorf("GitHub answered no repository %s on %s", ref.Project(), ref.Host)
	}
	for _, user := range raw.Repository.AssignableUsers.Nodes {
		out.People = append(out.People, PRPerson{Login: user.Login, Name: user.Name})
	}
	out.More = raw.Repository.AssignableUsers.PageInfo.HasNextPage
	if kind != CandidateReviewers || raw.Repository.Owner.Typename != "Organization" {
		return out, nil
	}
	var teams struct {
		Organization *struct {
			Teams struct {
				PageInfo githubPageInfo `json:"pageInfo"`
				Nodes    []struct {
					CombinedSlug string `json:"combinedSlug"`
					Name         string `json:"name"`
				} `json:"nodes"`
			} `json:"teams"`
		} `json:"organization"`
	}
	if _, err := client.GraphQL(ctx, "SearchTeams", githubSearchTeamsQuery, map[string]any{"org": raw.Repository.Owner.Login, "query": query}, &teams); err != nil || teams.Organization == nil {
		return out, nil
	}
	for _, team := range teams.Organization.Teams.Nodes {
		out.People = append(out.People, PRPerson{Login: team.CombinedSlug, Name: team.Name, Team: true})
	}
	out.More = out.More || teams.Organization.Teams.PageInfo.HasNextPage
	return out, nil
}

const githubMergeMessageQuery = `query PRMergeMessage($owner: String!, $name: String!, $number: Int!, $method: PullRequestMergeMethod!) {
  repository(owner: $owner, name: $name) { pullRequest(number: $number) { viewerMergeHeadlineText(mergeType: $method) viewerMergeBodyText(mergeType: $method) } }
}`

// DefaultMergeMessage is GitHub's default headline and body of a merge
// with method, as the viewer would see them (the repository's commit
// message setting applied).
func (f *githubForge) DefaultMergeMessage(ctx context.Context, ref PRReference, method string) (MergeMessage, error) {
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return MergeMessage{}, err
	}
	vars, err := githubPullVars(ref)
	if err != nil {
		return MergeMessage{}, err
	}
	vars["method"] = githubMergeMethodEnum(method)
	var raw struct {
		Repository *struct {
			PullRequest *struct {
				Headline string `json:"viewerMergeHeadlineText"`
				Body     string `json:"viewerMergeBodyText"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if _, err := client.GraphQL(ctx, "PRMergeMessage", githubMergeMessageQuery, vars, &raw); err != nil {
		return MergeMessage{}, err
	}
	if raw.Repository == nil || raw.Repository.PullRequest == nil {
		return MergeMessage{}, fmt.Errorf("GitHub answered no pull request %s#%d on %s", ref.Project(), ref.Number, ref.Host)
	}
	return MergeMessage{Title: raw.Repository.PullRequest.Headline, Body: raw.Repository.PullRequest.Body}, nil
}
