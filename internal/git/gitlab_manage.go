package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// GitLab's management actions (ForgeManager) and the Details read they
// rest on. The read is one GraphQL request beside the detail read
// (PRManage): reviewers with their review state, assignees, labels with
// their colors, the viewer's userPermissions, and from the schema itself
// (__type, which answers null for a type a server lacks) whether the
// server has mergeRequestReviewerRereview and mergeRequestUpdate's
// overrideRequestedChanges (GitLab 17.x, with requested changes that
// block a merge). A server with the override is read with its
// mergeabilityChecks too (PRManageChecks), remembered per host for
// gitlabCacheTTL, so a document never names a field an older server
// refuses. Writes are REST PUTs of the merge request (title,
// description, state_event, reviewer_ids, assignee_ids, add_labels,
// remove_labels) except the re-review and the bypass, which only GraphQL
// has.

const gitlabManageSelection = `userPermissions { updateMergeRequest adminMergeRequest }
      reviewers(first: 100) { nodes { id username name mergeRequestInteraction { reviewState } } }
      assignees(first: 100) { nodes { id username name } }
      labels(first: 100) { nodes { title color } }`

const gitlabManageSchema = `rereview: __type(name: "MergeRequestReviewerRereviewInput") { name }
  updateInput: __type(name: "MergeRequestUpdateInput") { inputFields { name } }`

const gitlabManageQuery = `query PRManage($fullPath: ID!, $iid: String!) {
  project(fullPath: $fullPath) {
    mergeRequest(iid: $iid) {
      ` + gitlabManageSelection + `
    }
  }
  ` + gitlabManageSchema + `
}`

const gitlabManageChecksQuery = `query PRManageChecks($fullPath: ID!, $iid: String!) {
  project(fullPath: $fullPath) {
    mergeRequest(iid: $iid) {
      ` + gitlabManageSelection + `
      mergeabilityChecks { identifier status }
    }
  }
  ` + gitlabManageSchema + `
}`

type gitlabGraphQLUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

func (u gitlabGraphQLUser) person() PRPerson {
	return PRPerson{Login: u.Username, Name: u.Name, ID: strings.TrimPrefix(u.ID, "gid://gitlab/User/")}
}

type gitlabManageRaw struct {
	Project *struct {
		MergeRequest *struct {
			UserPermissions struct {
				UpdateMergeRequest bool `json:"updateMergeRequest"`
				AdminMergeRequest  bool `json:"adminMergeRequest"`
			} `json:"userPermissions"`
			Reviewers struct {
				Nodes []struct {
					gitlabGraphQLUser
					Interaction *struct {
						ReviewState string `json:"reviewState"`
					} `json:"mergeRequestInteraction"`
				} `json:"nodes"`
			} `json:"reviewers"`
			Assignees struct {
				Nodes []gitlabGraphQLUser `json:"nodes"`
			} `json:"assignees"`
			Labels struct {
				Nodes []struct {
					Title string `json:"title"`
					Color string `json:"color"`
				} `json:"nodes"`
			} `json:"labels"`
			MergeabilityChecks []struct {
				Identifier string `json:"identifier"`
				Status     string `json:"status"`
			} `json:"mergeabilityChecks"`
		} `json:"mergeRequest"`
	} `json:"project"`
	Rereview    *struct{ Name string } `json:"rereview"`
	UpdateInput *struct {
		InputFields []gitlabInputField `json:"inputFields"`
	} `json:"updateInput"`
}

type gitlabInputField struct {
	Name string `json:"name"`
}

// gitlabManage is the Details read and what it adds to readiness.
type gitlabManage struct {
	manage PRManage
	// override is the server's overrideRequestedChanges; bypassed that the
	// merge request's requested-changes check passes while a reviewer
	// still requests changes.
	override, bypassed bool
}

// readManage is the Details read of mr (see the file comment).
func (f *gitlabForge) readManage(ctx context.Context, client *forgeapi.Client, ref PRReference, mr gitlabMRRaw) (gitlabManage, error) {
	op, doc := "PRManage", gitlabManageQuery
	if known, ok := f.manageOverride.get(ref.Host, f.core.nowFn()); ok && known {
		op, doc = "PRManageChecks", gitlabManageChecksQuery
	}
	var raw gitlabManageRaw
	vars := map[string]any{"fullPath": ref.Project(), "iid": strconv.Itoa(ref.Number)}
	if _, err := client.GraphQL(ctx, op, doc, vars, &raw); err != nil {
		return gitlabManage{}, err
	}
	if raw.Project == nil || raw.Project.MergeRequest == nil {
		return gitlabManage{}, fmt.Errorf("GitLab answered no merge request %s!%d on %s", ref.Project(), ref.Number, ref.Host)
	}
	out := gitlabManage{}
	if raw.UpdateInput != nil {
		out.override = slices.Contains(raw.UpdateInput.InputFields, gitlabInputField{Name: "overrideRequestedChanges"})
	}
	f.manageOverride.put(ref.Host, out.override, f.core.nowFn())
	node := raw.Project.MergeRequest
	manage := PRManage{Reviewers: []PRReviewer{}, Assignees: []PRPerson{}, Labels: []PRLabel{}}
	changes := false
	for _, reviewer := range node.Reviewers.Nodes {
		state := ReviewerPending
		if reviewer.Interaction != nil {
			switch reviewer.Interaction.ReviewState {
			case "APPROVED":
				state = ReviewerApproved
			case "REQUESTED_CHANGES":
				state, changes = ReviewerChangesRequested, true
			case "REVIEWED":
				state = ReviewerCommented
			}
		}
		manage.Reviewers = append(manage.Reviewers, PRReviewer{PRPerson: reviewer.person(), State: state})
	}
	for _, assignee := range node.Assignees.Nodes {
		manage.Assignees = append(manage.Assignees, assignee.person())
	}
	for _, label := range node.Labels.Nodes {
		manage.Labels = append(manage.Labels, PRLabel{Name: label.Title, Color: normalizeColor(label.Color)})
	}
	state := NormalizePRState(mr.State)
	open, update, admin := state == "open", node.UserPermissions.UpdateMergeRequest, node.UserPermissions.AdminMergeRequest
	blocked := strings.EqualFold(mr.DetailedMergeStatus, "requested_changes")
	manage.Can = PRManageCan{
		EditTitle:       update,
		EditDescription: update,
		Reviewers:       open && admin,
		Assignees:       admin,
		Labels:          admin,
		Close:           open && update,
		Reopen:          state == "closed" && update,
		ConvertToDraft:  open && update && !mr.Draft && !mr.WorkInProgress,
		Rerequest:       open && update && raw.Rereview != nil,
		Bypass:          open && blocked && out.override && mr.User.CanMerge,
	}
	out.manage = manage
	for _, check := range node.MergeabilityChecks {
		if check.Identifier == "REQUESTED_CHANGES" && check.Status == "SUCCESS" && changes && open && !blocked {
			out.bypassed = true
		}
	}
	return out, nil
}

// apply puts the Details read on the detail: the changes-requested
// blocker says whether the viewer may bypass it, and requested changes
// GitLab reports overridden read as a settled row.
func (m gitlabManage) apply(detail *PRDetail) {
	detail.Manage = m.manage
	for i := range detail.Readiness.Blockers {
		if detail.Readiness.Blockers[i].Kind == BlockerChangesRequested {
			detail.Readiness.Blockers[i].Bypassable = m.manage.Can.Bypass
		}
	}
	if m.bypassed {
		detail.Readiness.Settled = append(detail.Readiness.Settled, MergeBlocker{Kind: BlockerChangesRequested, Text: "Requested changes bypassed", Bypassed: true})
	}
}

// gitlabPut updates the merge request with body.
func gitlabPut(ctx context.Context, client *forgeapi.Client, ref PRReference, body map[string]any) error {
	_, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPut, Path: gitlabMRPath(ref), Body: body}, nil)
	return writeOutcome(err)
}

// EditPR reads the merge request, refuses a description edit whose
// expected description is no longer its own, and PUTs the edit.
func (f *gitlabForge) EditPR(ctx context.Context, ref PRReference, edit PREdit) error {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	body := map[string]any{}
	if edit.Description != nil {
		mr, err := readGitLabMR(ctx, client, ref)
		if err != nil {
			return err
		}
		if !sameDescription(mr.Description, *edit.ExpectedDescription) {
			return ErrDescriptionChanged
		}
		body["description"] = *edit.Description
	}
	if edit.Title != nil {
		body["title"] = *edit.Title
	}
	return gitlabPut(ctx, client, ref, body)
}

// SetPRPeople reads the merge request's reviewers or assignees and PUTs
// the whole set with the change applied: GitLab's REST update replaces
// the set and has no add or remove of one.
func (f *gitlabForge) SetPRPeople(ctx context.Context, ref PRReference, reviewers bool, change PRPeopleChange) error {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	mr, err := readGitLabMR(ctx, client, ref)
	if err != nil {
		return err
	}
	current, key := mr.Assignees, "assignee_ids"
	if reviewers {
		current, key = mr.Reviewers, "reviewer_ids"
	}
	remove := map[int64]bool{}
	for _, person := range change.Remove {
		id, _ := gitlabUserID(person.ID)
		remove[id] = true
	}
	ids := []int64{}
	for _, user := range current {
		if user.ID > 0 && !remove[user.ID] && !slices.Contains(ids, user.ID) {
			ids = append(ids, user.ID)
		}
	}
	for _, person := range change.Add {
		if id, _ := gitlabUserID(person.ID); !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return gitlabPut(ctx, client, ref, map[string]any{key: ids})
}

// gitlabLabelNames is a comma list of label names, as add_labels takes
// them.
func gitlabLabelNames(labels []PRLabel) string {
	names := make([]string, 0, len(labels))
	for _, label := range labels {
		names = append(names, label.Name)
	}
	return strings.Join(names, ",")
}

// SetPRLabels adds and removes by name in one PUT.
func (f *gitlabForge) SetPRLabels(ctx context.Context, ref PRReference, change PRLabelChange) error {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	body := map[string]any{}
	if len(change.Add) > 0 {
		body["add_labels"] = gitlabLabelNames(change.Add)
	}
	if len(change.Remove) > 0 {
		body["remove_labels"] = gitlabLabelNames(change.Remove)
	}
	return gitlabPut(ctx, client, ref, body)
}

// SetPRState closes or reopens with state_event; a draft is GitLab's
// title prefix, so converting reads the title and PUTs it prefixed.
func (f *gitlabForge) SetPRState(ctx context.Context, ref PRReference, state string) error {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	switch state {
	case PRStateClose, PRStateReopen:
		return gitlabPut(ctx, client, ref, map[string]any{"state_event": state})
	}
	mr, err := readGitLabMR(ctx, client, ref)
	if err != nil {
		return err
	}
	if mr.Draft || mr.WorkInProgress || gitlabDraftlessTitle(mr.Title) != mr.Title {
		return nil
	}
	return gitlabPut(ctx, client, ref, map[string]any{"title": "Draft: " + mr.Title})
}

const gitlabRereviewMutation = `mutation PRRereview($projectPath: ID!, $iid: String!, $userId: UserID!) {
  mergeRequestReviewerRereview(input: {projectPath: $projectPath, iid: $iid, userId: $userId}) { errors }
}`

const gitlabBypassMutation = `mutation PRBypassChanges($projectPath: ID!, $iid: String!) {
  mergeRequestUpdate(input: {projectPath: $projectPath, iid: $iid, overrideRequestedChanges: true}) { errors }
}`

// gitlabMutation runs one GraphQL mutation whose payload lists errors:
// any is GitLab refusing it.
func gitlabMutation(ctx context.Context, client *forgeapi.Client, op, doc, field string, vars map[string]any) error {
	var raw map[string]*struct {
		Errors []string `json:"errors"`
	}
	if _, err := client.GraphQL(ctx, op, doc, vars, &raw); err != nil {
		return writeOutcome(err)
	}
	payload := raw[field]
	if payload == nil {
		return &UnconfirmedWriteError{Err: fmt.Errorf("GitLab answered no %s payload", field)}
	}
	if len(payload.Errors) > 0 {
		return &ForgeActionError{Kind: ForgeErrorRefused, Message: "GitLab refused: " + trimSentence(strings.Join(payload.Errors, "; ")) + ".", Err: errors.New(strings.Join(payload.Errors, "; "))}
	}
	return nil
}

// RerequestReview asks reviewer, one of the merge request's reviewers
// (GitLab refuses anyone else), for a new review.
func (f *gitlabForge) RerequestReview(ctx context.Context, ref PRReference, reviewer PRPerson) error {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	id, _ := gitlabUserID(reviewer.ID)
	vars := map[string]any{"projectPath": ref.Project(), "iid": strconv.Itoa(ref.Number), "userId": "gid://gitlab/User/" + strconv.FormatInt(id, 10)}
	return gitlabMutation(ctx, client, "PRRereview", gitlabRereviewMutation, "mergeRequestReviewerRereview", vars)
}

// DismissReview is GitHub's; GitLab bypasses requested changes instead.
func (f *gitlabForge) DismissReview(context.Context, PRReference, string, string) error {
	return ErrUnsupportedForge
}

// BypassRequestedChanges overrides the merge request's requested changes;
// GitLab takes it only from a user who may merge.
func (f *gitlabForge) BypassRequestedChanges(ctx context.Context, ref PRReference) error {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	vars := map[string]any{"projectPath": ref.Project(), "iid": strconv.Itoa(ref.Number)}
	return gitlabMutation(ctx, client, "PRBypassChanges", gitlabBypassMutation, "mergeRequestUpdate", vars)
}

// SearchCandidates answers the project's members (reviewers and
// assignees alike) or labels matching query, first page only.
func (f *gitlabForge) SearchCandidates(ctx context.Context, ref PRReference, kind, query string) (PRCandidates, error) {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return PRCandidates{}, err
	}
	path := gitlabProjectPath(ref.Project()) + "/users"
	if kind == CandidateLabels {
		path = gitlabProjectPath(ref.Project()) + "/labels"
	}
	values := url.Values{"per_page": {strconv.Itoa(candidatePageSize)}}
	if query != "" {
		values.Set("search", query)
	}
	resp, err := client.Do(ctx, forgeapi.Request{Path: path, Query: values})
	if err != nil {
		return PRCandidates{}, err
	}
	out := PRCandidates{People: []PRPerson{}, Labels: []PRLabel{}, More: strings.TrimSpace(resp.Header.Get("X-Next-Page")) != ""}
	if kind == CandidateLabels {
		var labels []struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		}
		if err := json.Unmarshal(resp.Body, &labels); err != nil {
			return PRCandidates{}, fmt.Errorf("GitLab labels: decode response: %w", err)
		}
		for _, label := range labels {
			out.Labels = append(out.Labels, PRLabel{Name: label.Name, Color: normalizeColor(label.Color)})
		}
		return out, nil
	}
	var users []gitlabUserRaw
	if err := json.Unmarshal(resp.Body, &users); err != nil {
		return PRCandidates{}, fmt.Errorf("GitLab project users: decode response: %w", err)
	}
	for _, user := range users {
		out.People = append(out.People, user.person())
	}
	return out, nil
}

// gitlabUserRaw is a REST user with its id: a merge request's reviewer or
// assignee, a project member.
type gitlabUserRaw struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

func (u gitlabUserRaw) person() PRPerson {
	return PRPerson{Login: u.Username, Name: u.Name, ID: strconv.FormatInt(u.ID, 10)}
}

const gitlabMergeMessageQuery = `query PRMergeMessage($fullPath: ID!, $iid: String!) {
  project(fullPath: $fullPath) { mergeRequest(iid: $iid) { defaultMergeCommitMessage defaultSquashCommitMessage } }
}`

// DefaultMergeMessage is GitLab's default merge or squash commit message,
// the project's commit templates applied.
func (f *gitlabForge) DefaultMergeMessage(ctx context.Context, ref PRReference, method string) (MergeMessage, error) {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return MergeMessage{}, err
	}
	var raw struct {
		Project *struct {
			MergeRequest *struct {
				Merge  string `json:"defaultMergeCommitMessage"`
				Squash string `json:"defaultSquashCommitMessage"`
			} `json:"mergeRequest"`
		} `json:"project"`
	}
	vars := map[string]any{"fullPath": ref.Project(), "iid": strconv.Itoa(ref.Number)}
	if _, err := client.GraphQL(ctx, "PRMergeMessage", gitlabMergeMessageQuery, vars, &raw); err != nil {
		return MergeMessage{}, err
	}
	if raw.Project == nil || raw.Project.MergeRequest == nil {
		return MergeMessage{}, fmt.Errorf("GitLab answered no merge request %s!%d on %s", ref.Project(), ref.Number, ref.Host)
	}
	if method == MergeMethodSquash {
		return splitMergeMessage(raw.Project.MergeRequest.Squash), nil
	}
	return splitMergeMessage(raw.Project.MergeRequest.Merge), nil
}

// gitlabStateEventRaw is one resource state event: GitLab records a
// close, a reopen and a merge as these rather than as system notes.
type gitlabStateEventRaw struct {
	ID        int64           `json:"id"`
	User      gitlabAuthorRaw `json:"user"`
	CreatedAt string          `json:"created_at"`
	State     string          `json:"state"`
}

// readGitLabStateEvents reads every page of the merge request's state
// events.
func readGitLabStateEvents(ctx context.Context, client *forgeapi.Client, ref PRReference) ([]gitlabStateEventRaw, error) {
	request := forgeapi.Request{Path: gitlabMRPath(ref) + "/resource_state_events", Query: url.Values{"per_page": {strconv.Itoa(gitlabPageSize)}}}
	var out []gitlabStateEventRaw
	page := 1
	err := client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		var events []gitlabStateEventRaw
		if err := json.Unmarshal(resp.Body, &events); err != nil {
			return false, fmt.Errorf("GitLab state events: decode response: %w", err)
		}
		out = append(out, events...)
		return advanceGitLabPage(resp, &page), nil
	})
	return out, err
}

// gitlabStateEvents maps state events to forge events.
func gitlabStateEvents(states []gitlabStateEventRaw) []ReviewEvent {
	var out []ReviewEvent
	for _, state := range states {
		kind := map[string]string{"closed": EventClosed, "reopened": EventReopened, "merged": EventMerged}[state.State]
		if kind == "" {
			continue
		}
		out = append(out, ReviewEvent{ID: "state:" + strconv.FormatInt(state.ID, 10), Kind: kind, ActorLogin: state.User.Username, ActorName: state.User.Name, At: state.CreatedAt})
	}
	return out
}
