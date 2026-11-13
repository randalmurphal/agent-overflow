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
// REST, addressed by login, which GitHub checks itself; labels are
// addLabelsToLabelable / removeLabelsFromLabelable after one read that
// proves every label id belongs to the PR's repository or the PR; close,
// reopen and convert to draft are their mutations; a dismissal checks
// that the review is one of the PR's standing change requests.

// githubManageFields is the Details half of the detail selection. The
// viewerCan* flags are the forge's own answer per control.
const githubManageFields = `viewerCanUpdate viewerCanClose viewerCanReopen viewerCanLabel ` +
	`reviewRequests(first: 100) { nodes { requestedReviewer { __typename ... on User { login name } ... on Bot { login } ... on Mannequin { login } ... on Team { combinedSlug name } } } } ` +
	`assignees(first: 100) { nodes { login name } } ` +
	`labels(first: 100) { nodes { id name color } }`

// githubPullManageRaw is the Details half of a PR's detail.
type githubPullManageRaw struct {
	ViewerCanUpdate bool `json:"viewerCanUpdate"`
	ViewerCanClose  bool `json:"viewerCanClose"`
	ViewerCanReopen bool `json:"viewerCanReopen"`
	ViewerCanLabel  bool `json:"viewerCanLabel"`
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
// the viewer's capabilities. Requesting reviews and assigning take triage
// access on GitHub; dismissing a review takes write.
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
	opinions := map[string]string{}
	for _, review := range pull.LatestOpinionatedReviews.Nodes {
		opinions[strings.ToLower(review.Author.Login)] = strings.ToUpper(review.State)
	}
	for _, review := range pull.LatestReviews.Nodes {
		login := review.Author.Login
		if login == "" || strings.EqualFold(login, pull.Author.Login) {
			continue
		}
		state := ReviewerCommented
		switch opinions[strings.ToLower(login)] {
		case "APPROVED":
			state = ReviewerApproved
		case "CHANGES_REQUESTED":
			state = ReviewerChangesRequested
		}
		add(PRReviewer{PRPerson: PRPerson{Login: login, Name: review.Author.Name}, State: state, Stale: review.Commit.OID != "" && review.Commit.OID != pull.HeadRefOID})
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
	triage := githubRoleAtLeast(repo.ViewerPermission, "TRIAGE")
	write := githubRoleAtLeast(repo.ViewerPermission, "WRITE")
	out.Can = PRManageCan{
		EditTitle:       pull.ViewerCanUpdate,
		EditDescription: pull.ViewerCanUpdate,
		Reviewers:       open && triage,
		Assignees:       triage,
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

// githubPeopleBody is a requested_reviewers or assignees body.
func githubPeopleBody(reviewers bool, people []PRPerson) map[string][]string {
	if !reviewers {
		logins := make([]string, 0, len(people))
		for _, person := range people {
			logins = append(logins, person.Login)
		}
		return map[string][]string{"assignees": logins}
	}
	body := map[string][]string{"reviewers": {}, "team_reviewers": {}}
	for _, person := range people {
		if person.Team {
			_, slug, _ := strings.Cut(person.Login, "/")
			body["team_reviewers"] = append(body["team_reviewers"], slug)
		} else {
			body["reviewers"] = append(body["reviewers"], person.Login)
		}
	}
	return body
}

// SetPRPeople adds, then removes: reviewers through
// pulls/N/requested_reviewers, assignees through issues/N/assignees.
// GitHub checks every login (a reviewer must be a collaborator, a team
// the repository's).
func (f *githubForge) SetPRPeople(ctx context.Context, ref PRReference, reviewers bool, change PRPeopleChange) error {
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("%s/issues/%d/assignees", githubRepoPath(ref.Repository()), ref.Number)
	if reviewers {
		path = githubPullPath(ref) + "/requested_reviewers"
	}
	var writes []func() error
	if len(change.Add) > 0 {
		writes = append(writes, func() error {
			_, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: path, Body: githubPeopleBody(reviewers, change.Add)}, nil)
			return err
		})
	}
	if len(change.Remove) > 0 {
		writes = append(writes, func() error {
			_, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodDelete, Path: path, Body: githubPeopleBody(reviewers, change.Remove)}, nil)
			return err
		})
	}
	return runWrites(writes)
}

// runWrites runs a change's writes in order and stops at the first
// failure, classified as writeOutcome does; one after an applied write
// is a partial change.
func runWrites(writes []func() error) error {
	for i, write := range writes {
		if err := write(); err != nil {
			if i > 0 {
				return &UnconfirmedWriteError{Err: err, Partial: true}
			}
			return writeOutcome(err)
		}
	}
	return nil
}

const githubLabelTargetQuery = `query PRLabelTarget($owner: String!, $name: String!, $number: Int!, $ids: [ID!]!) {
  repository(owner: $owner, name: $name) { id pullRequest(number: $number) { id labels(first: 100) { nodes { id } } } }
  nodes(ids: $ids) { __typename ... on Label { id repository { id } } }
}`

const githubAddLabelsMutation = `mutation AddLabels($labelableID: ID!, $labelIDs: [ID!]!) {
  addLabelsToLabelable(input: {labelableId: $labelableID, labelIds: $labelIDs}) { clientMutationId }
}`

const githubRemoveLabelsMutation = `mutation RemoveLabels($labelableID: ID!, $labelIDs: [ID!]!) {
  removeLabelsFromLabelable(input: {labelableId: $labelableID, labelIds: $labelIDs}) { clientMutationId }
}`

// SetPRLabels reads the PR's node id and labels and resolves every label
// to add, then adds and removes. A label to add must be one of the
// repository's; a label to remove that the PR no longer carries is
// already removed.
func (f *githubForge) SetPRLabels(ctx context.Context, ref PRReference, change PRLabelChange) error {
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	vars, err := githubPullVars(ref)
	if err != nil {
		return err
	}
	adds := make([]string, 0, len(change.Add))
	for _, label := range change.Add {
		adds = append(adds, label.ID)
	}
	vars["ids"] = adds
	var raw struct {
		Repository *struct {
			ID          string `json:"id"`
			PullRequest *struct {
				ID     string `json:"id"`
				Labels struct {
					Nodes []githubLabelRaw `json:"nodes"`
				} `json:"labels"`
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
	current := map[string]bool{}
	for _, label := range raw.Repository.PullRequest.Labels.Nodes {
		current[label.ID] = true
	}
	var removes []string
	for _, label := range change.Remove {
		if current[label.ID] {
			removes = append(removes, label.ID)
		}
	}
	pullID := raw.Repository.PullRequest.ID
	var writes []func() error
	if len(adds) > 0 {
		writes = append(writes, func() error {
			_, err := client.GraphQL(ctx, "AddLabels", githubAddLabelsMutation, map[string]any{"labelableID": pullID, "labelIDs": adds}, nil)
			return err
		})
	}
	if len(removes) > 0 {
		writes = append(writes, func() error {
			_, err := client.GraphQL(ctx, "RemoveLabels", githubRemoveLabelsMutation, map[string]any{"labelableID": pullID, "labelIDs": removes}, nil)
			return err
		})
	}
	return runWrites(writes)
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

const githubSearchPeopleQuery = `query SearchAssignable($owner: String!, $name: String!, $query: String!, $withTeams: Boolean!) {
  repository(owner: $owner, name: $name) {
    assignableUsers(first: 20, query: $query) { pageInfo { hasNextPage } nodes { login name } }
    owner { ... on Organization @include(if: $withTeams) { teams(first: 20, query: $query) { pageInfo { hasNextPage } nodes { combinedSlug name } } } }
  }
}`

const githubSearchLabelsQuery = `query SearchLabels($owner: String!, $name: String!, $query: String!) {
  repository(owner: $owner, name: $name) { labels(first: 20, query: $query) { pageInfo { hasNextPage } nodes { id name color } } }
}`

// SearchCandidates answers the repository's assignable users (reviewers
// and assignees alike; GitHub requests reviews from the same set) and,
// for reviewers, its organization's teams when the login can read them:
// a refusal confined to the teams leaves the users. Labels are the
// repository's.
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
	vars["withTeams"] = kind == CandidateReviewers
	var raw struct {
		Repository *struct {
			AssignableUsers struct {
				PageInfo githubPageInfo   `json:"pageInfo"`
				Nodes    []githubActorRaw `json:"nodes"`
			} `json:"assignableUsers"`
			Owner *struct {
				Teams *struct {
					PageInfo githubPageInfo `json:"pageInfo"`
					Nodes    []struct {
						CombinedSlug string `json:"combinedSlug"`
						Name         string `json:"name"`
					} `json:"nodes"`
				} `json:"teams"`
			} `json:"owner"`
		} `json:"repository"`
	}
	_, err = client.GraphQL(ctx, "SearchAssignable", githubSearchPeopleQuery, vars, &raw)
	if err != nil {
		gqlErr, ok := errors.AsType[*forgeapi.GraphQLError](err)
		if !ok || !graphQLProblemsAt(gqlErr, "repository", "owner", "teams") || raw.Repository == nil {
			return PRCandidates{}, err
		}
	}
	if raw.Repository == nil {
		return PRCandidates{}, fmt.Errorf("GitHub answered no repository %s on %s", ref.Project(), ref.Host)
	}
	for _, user := range raw.Repository.AssignableUsers.Nodes {
		out.People = append(out.People, PRPerson{Login: user.Login, Name: user.Name})
	}
	out.More = raw.Repository.AssignableUsers.PageInfo.HasNextPage
	if owner := raw.Repository.Owner; owner != nil && owner.Teams != nil {
		for _, team := range owner.Teams.Nodes {
			out.People = append(out.People, PRPerson{Login: team.CombinedSlug, Name: team.Name, Team: true})
		}
		out.More = out.More || owner.Teams.PageInfo.HasNextPage
	}
	return out, nil
}

// graphQLProblemsAt is whether every problem's path begins with path.
func graphQLProblemsAt(gqlErr *forgeapi.GraphQLError, path ...string) bool {
	if len(gqlErr.Problems) == 0 {
		return false
	}
	for _, problem := range gqlErr.Problems {
		if len(problem.Path) < len(path) {
			return false
		}
		for i, segment := range path {
			if fmt.Sprint(problem.Path[i]) != segment {
				return false
			}
		}
	}
	return true
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
