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

// GitLab's management writes and the reads beside them (ForgeManager).
// The title, description, close and reopen are REST PUTs of the merge
// request. Draft is mergeRequestSetDraft. Reviewers, assignees and
// labels are the set mutations with operationMode REMOVE then APPEND, so
// a change touches only whom or what it names, each checked against the
// merge request the mutation answers: a person or label it did not apply
// is a refusal naming it. Re-request and bypass are GraphQL-only.

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

// gitlabMutationErrors is a mutation payload's errors as GitLab's
// refusal.
func gitlabMutationErrors(errs []string) error {
	joined := strings.Join(errs, "; ")
	return &ForgeActionError{Kind: ForgeErrorRefused, Message: "GitLab refused: " + trimSentence(joined) + ".", Err: errors.New(joined)}
}

// gitlabMutation runs one GraphQL mutation and decodes its payload,
// field of the answer, into payload (a struct with Errors). An envelope
// error is classified as writeOutcome does, a missing payload is
// unconfirmed, and the payload's errors are GitLab refusing it.
func gitlabMutation[P any](ctx context.Context, client *forgeapi.Client, op, doc, field string, vars map[string]any, errorsOf func(*P) []string) (*P, error) {
	var raw map[string]*P
	if _, err := client.GraphQL(ctx, op, doc, vars, &raw); err != nil {
		return nil, writeOutcome(err)
	}
	payload := raw[field]
	if payload == nil {
		return nil, &UnconfirmedWriteError{Err: fmt.Errorf("GitLab answered no %s payload", field)}
	}
	if errs := errorsOf(payload); len(errs) > 0 {
		return nil, gitlabMutationErrors(errs)
	}
	return payload, nil
}

type gitlabErrorsPayload struct {
	Errors []string `json:"errors"`
}

func gitlabErrorsOf(p *gitlabErrorsPayload) []string { return p.Errors }

// gitlabMRVars are a mutation's merge request arguments.
func gitlabMRVars(ref PRReference) map[string]any {
	return map[string]any{"projectPath": ref.Project(), "iid": strconv.Itoa(ref.Number)}
}

// gitlabUnsupported refuses a change the host's schema cannot make.
func gitlabUnsupported(ref PRReference, what string) error {
	return &ForgeActionError{Kind: ForgeErrorRefused, Message: fmt.Sprintf("GitLab on %s cannot change %s here; change them on GitLab.", ref.Host, what)}
}

const gitlabSetReviewersMutation = `mutation PRSetReviewers($projectPath: ID!, $iid: String!, $usernames: [String!]!, $mode: MutationOperationMode!) {
  mergeRequestSetReviewers(input: {projectPath: $projectPath, iid: $iid, reviewerUsernames: $usernames, operationMode: $mode}) { errors mergeRequest { reviewers(first: 100) { pageInfo { hasNextPage } nodes { username } } } }
}`

const gitlabSetAssigneesMutation = `mutation PRSetAssignees($projectPath: ID!, $iid: String!, $usernames: [String!]!, $mode: MutationOperationMode!) {
  mergeRequestSetAssignees(input: {projectPath: $projectPath, iid: $iid, assigneeUsernames: $usernames, operationMode: $mode}) { errors mergeRequest { assignees(first: 100) { pageInfo { hasNextPage } nodes { username } } } }
}`

type gitlabUserConnection struct {
	PageInfo githubPageInfo      `json:"pageInfo"`
	Nodes    []gitlabGraphQLUser `json:"nodes"`
}

type gitlabPeoplePayload struct {
	Errors       []string `json:"errors"`
	MergeRequest *struct {
		Reviewers *gitlabUserConnection `json:"reviewers"`
		Assignees *gitlabUserConnection `json:"assignees"`
	} `json:"mergeRequest"`
}

// SetPRPeople removes, then adds, by username with the set mutations.
func (f *gitlabForge) SetPRPeople(ctx context.Context, ref PRReference, reviewers bool, change PRPeopleChange) error {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	schema, err := f.manageSchema(ctx, client, ref.Host)
	if err != nil {
		return err
	}
	op, doc, field, list := "PRSetAssignees", gitlabSetAssigneesMutation, "mergeRequestSetAssignees", "assignees"
	supported := schema.setAssignees
	if reviewers {
		op, doc, field, list, supported = "PRSetReviewers", gitlabSetReviewersMutation, "mergeRequestSetReviewers", "reviewers", schema.setReviewers
	}
	if !supported {
		return gitlabUnsupported(ref, list)
	}
	step := func(mode string, people []PRPerson) changeStep {
		names := logins(people)
		done, next := stepWords(strings.ToLower(mode), names, list)
		return changeStep{done: done, next: next, run: func() error {
			vars := gitlabMRVars(ref)
			vars["usernames"], vars["mode"] = names, mode
			payload, err := gitlabMutation(ctx, client, op, doc, field, vars, func(p *gitlabPeoplePayload) []string { return p.Errors })
			if err != nil {
				return err
			}
			if payload.MergeRequest == nil {
				return &UnconfirmedWriteError{Err: fmt.Errorf("GitLab answered no merge request after setting its %s", list)}
			}
			now := payload.MergeRequest.Assignees
			if reviewers {
				now = payload.MergeRequest.Reviewers
			}
			if now == nil {
				return &UnconfirmedWriteError{Err: fmt.Errorf("GitLab answered no %s after setting them", list)}
			}
			present := make([]bool, len(names))
			for i, name := range names {
				present[i] = slices.ContainsFunc(now.Nodes, func(u gitlabGraphQLUser) bool { return strings.EqualFold(u.Username, name) })
			}
			return gitlabSetOutcome(mode, names, present, now.PageInfo.HasNextPage, list, func(missed []string) error {
				if mode == "REMOVE" {
					return &ForgeActionError{Kind: ForgeErrorRefused, Message: fmt.Sprintf("GitLab kept %s on the %s.", joinClauses(missed), list)}
				}
				why := "they may not be members of the project"
				if !reviewers {
					why = "they may not be members of the project, or the project takes fewer assignees"
				}
				return &ForgeActionError{Kind: ForgeErrorRefused, Message: fmt.Sprintf("GitLab did not add %s to the %s: %s.", joinClauses(missed), list, why)}
			})
		}}
	}
	var steps []changeStep
	if len(change.Remove) > 0 {
		steps = append(steps, step("REMOVE", change.Remove))
	}
	if len(change.Add) > 0 {
		steps = append(steps, step("APPEND", change.Add))
	}
	return runSteps(steps)
}

// gitlabSetOutcome is a set mutation's result read from the list it
// answered (present[i] for names[i]; more when the list has a next page,
// which the answer does not show). An addition listed, or a removal not
// listed in a whole list, applied; an addition missing from a whole list,
// or a removal still listed, was refused (refusal words those); one the
// list cannot show (not listed in a list with more) is unknown. All
// applied is nil. None applied is the refusal, or unconfirmed when any is
// unknown. Some applied is a PartialWriteError naming both, whose cause
// is the refusal or, when any is unknown, unconfirmed.
func gitlabSetOutcome(mode string, names []string, present []bool, more bool, list string, refusal func(missed []string) error) error {
	var applied, refused, unknown []string
	for i, name := range names {
		listed := present[i]
		switch {
		case mode == "APPEND" && listed, mode == "REMOVE" && !listed && !more:
			applied = append(applied, name)
		case mode == "APPEND" && !more, mode == "REMOVE" && listed:
			refused = append(refused, name)
		default:
			unknown = append(unknown, name)
		}
	}
	if len(refused) == 0 && len(unknown) == 0 {
		return nil
	}
	var cause error
	if len(unknown) > 0 {
		cause = &UnconfirmedWriteError{Err: fmt.Errorf("GitLab answered a partial list of the %s, without %s", list, joinClauses(unknown))}
	} else {
		cause = refusal(refused)
	}
	if len(applied) == 0 {
		return cause
	}
	verb := "add"
	if mode == "REMOVE" {
		verb = "remove"
	}
	done, _ := stepWords(verb, applied, list)
	_, next := stepWords(verb, append(refused, unknown...), list)
	return &PartialWriteError{Done: done, Next: next, Err: cause}
}

func logins(people []PRPerson) []string {
	out := make([]string, 0, len(people))
	for _, person := range people {
		out = append(out, person.Login)
	}
	return out
}

func labelNames(labels []PRLabel) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		out = append(out, firstNonEmpty(label.Name, label.ID))
	}
	return out
}

const gitlabSetLabelsMutation = `mutation PRSetLabels($projectPath: ID!, $iid: String!, $labelIds: [LabelID!]!, $mode: MutationOperationMode!) {
  mergeRequestSetLabels(input: {projectPath: $projectPath, iid: $iid, labelIds: $labelIds, operationMode: $mode}) { errors mergeRequest { labels(first: 100) { pageInfo { hasNextPage } nodes { id } } } }
}`

type gitlabLabelsPayload struct {
	Errors       []string `json:"errors"`
	MergeRequest *struct {
		Labels *struct {
			PageInfo githubPageInfo    `json:"pageInfo"`
			Nodes    []gitlabLabelNode `json:"nodes"`
		} `json:"labels"`
	} `json:"mergeRequest"`
}

// SetPRLabels removes, then adds, by label id with mergeRequestSetLabels.
func (f *gitlabForge) SetPRLabels(ctx context.Context, ref PRReference, change PRLabelChange) error {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	schema, err := f.manageSchema(ctx, client, ref.Host)
	if err != nil {
		return err
	}
	if !schema.setLabels {
		return gitlabUnsupported(ref, "labels")
	}
	step := func(mode string, labels []PRLabel) changeStep {
		done, next := stepWords(strings.ToLower(mode), labelNames(labels), "labels")
		return changeStep{done: done, next: next, run: func() error {
			ids := make([]string, 0, len(labels))
			for _, label := range labels {
				ids = append(ids, label.ID)
			}
			vars := gitlabMRVars(ref)
			vars["labelIds"], vars["mode"] = ids, mode
			payload, err := gitlabMutation(ctx, client, "PRSetLabels", gitlabSetLabelsMutation, "mergeRequestSetLabels", vars, func(p *gitlabLabelsPayload) []string { return p.Errors })
			if err != nil {
				return err
			}
			if payload.MergeRequest == nil || payload.MergeRequest.Labels == nil {
				return &UnconfirmedWriteError{Err: errors.New("GitLab answered no merge request labels after setting them")}
			}
			now := payload.MergeRequest.Labels
			present := make([]bool, len(labels))
			for i, label := range labels {
				present[i] = slices.ContainsFunc(now.Nodes, func(n gitlabLabelNode) bool { return n.ID == label.ID })
			}
			return gitlabSetOutcome(mode, labelNames(labels), present, now.PageInfo.HasNextPage, "labels", func(missed []string) error {
				if mode == "REMOVE" {
					return &ForgeActionError{Kind: ForgeErrorRefused, Message: fmt.Sprintf("GitLab kept the label %s.", joinClauses(missed))}
				}
				return &ForgeActionError{Kind: ForgeErrorNotFound, Message: fmt.Sprintf("GitLab did not add the label %s: it is not one of %s's labels. Search the labels again.", joinClauses(missed), ref.Project())}
			})
		}}
	}
	var steps []changeStep
	if len(change.Remove) > 0 {
		steps = append(steps, step("REMOVE", change.Remove))
	}
	if len(change.Add) > 0 {
		steps = append(steps, step("APPEND", change.Add))
	}
	return runSteps(steps)
}

const gitlabSetDraftMutation = `mutation PRSetDraft($projectPath: ID!, $iid: String!, $draft: Boolean!) {
  mergeRequestSetDraft(input: {projectPath: $projectPath, iid: $iid, draft: $draft}) { errors mergeRequest { draft } }
}`

// gitlabSetDraft marks the merge request draft or ready with
// mergeRequestSetDraft, which leaves the rest of the title as it is now.
func gitlabSetDraft(ctx context.Context, client *forgeapi.Client, ref PRReference, draft bool) error {
	vars := gitlabMRVars(ref)
	vars["draft"] = draft
	type payload struct {
		Errors       []string `json:"errors"`
		MergeRequest *struct {
			Draft bool `json:"draft"`
		} `json:"mergeRequest"`
	}
	answer, err := gitlabMutation(ctx, client, "PRSetDraft", gitlabSetDraftMutation, "mergeRequestSetDraft", vars, func(p *payload) []string { return p.Errors })
	if err != nil {
		return err
	}
	if answer.MergeRequest == nil || answer.MergeRequest.Draft != draft {
		return &UnconfirmedWriteError{Err: fmt.Errorf("GitLab did not report %s!%d's draft state after setting it", ref.Project(), ref.Number)}
	}
	return nil
}

// SetPRState closes or reopens with state_event, and converts to a draft
// with mergeRequestSetDraft.
func (f *gitlabForge) SetPRState(ctx context.Context, ref PRReference, state string) error {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	if state == PRStateToDraft {
		return gitlabSetDraft(ctx, client, ref, true)
	}
	return gitlabPut(ctx, client, ref, map[string]any{"state_event": state})
}

const gitlabRereviewMutation = `mutation PRRereview($projectPath: ID!, $iid: String!, $userId: UserID!) {
  mergeRequestReviewerRereview(input: {projectPath: $projectPath, iid: $iid, userId: $userId}) { errors }
}`

const gitlabBypassMutation = `mutation PRBypassChanges($projectPath: ID!, $iid: String!) {
  mergeRequestUpdate(input: {projectPath: $projectPath, iid: $iid, overrideRequestedChanges: true}) { errors }
}`

// RerequestReview asks reviewer, one of the merge request's reviewers
// (GitLab refuses anyone else), for a new review, by user id.
func (f *gitlabForge) RerequestReview(ctx context.Context, ref PRReference, reviewer PRPerson) error {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	id, _ := gitlabUserID(reviewer.ID)
	vars := gitlabMRVars(ref)
	vars["userId"] = "gid://gitlab/User/" + strconv.FormatInt(id, 10)
	_, err = gitlabMutation(ctx, client, "PRRereview", gitlabRereviewMutation, "mergeRequestReviewerRereview", vars, gitlabErrorsOf)
	return err
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
	_, err = gitlabMutation(ctx, client, "PRBypassChanges", gitlabBypassMutation, "mergeRequestUpdate", gitlabMRVars(ref), gitlabErrorsOf)
	return err
}

const gitlabSearchLabelsQuery = `query SearchProjectLabels($fullPath: ID!, $search: String) {
  project(fullPath: $fullPath) { labels(first: 20, searchTerm: $search, includeAncestorGroups: true) { pageInfo { hasNextPage } nodes { id title color } } }
}`

// SearchCandidates answers the project's members (reviewers and
// assignees alike, REST) or its labels and its groups' (GraphQL, with
// their ids) matching query, first page only.
func (f *gitlabForge) SearchCandidates(ctx context.Context, ref PRReference, kind, query string) (PRCandidates, error) {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return PRCandidates{}, err
	}
	out := PRCandidates{People: []PRPerson{}, Labels: []PRLabel{}}
	if kind == CandidateLabels {
		vars := map[string]any{"fullPath": ref.Project()}
		if query != "" {
			vars["search"] = query
		}
		var raw struct {
			Project *struct {
				Labels struct {
					PageInfo githubPageInfo    `json:"pageInfo"`
					Nodes    []gitlabLabelNode `json:"nodes"`
				} `json:"labels"`
			} `json:"project"`
		}
		if _, err := client.GraphQL(ctx, "SearchProjectLabels", gitlabSearchLabelsQuery, vars, &raw); err != nil {
			return PRCandidates{}, err
		}
		if raw.Project == nil {
			return PRCandidates{}, fmt.Errorf("GitLab answered no project %s on %s", ref.Project(), ref.Host)
		}
		for _, label := range raw.Project.Labels.Nodes {
			out.Labels = append(out.Labels, label.label())
		}
		out.More = raw.Project.Labels.PageInfo.HasNextPage
		return out, nil
	}
	values := url.Values{"per_page": {strconv.Itoa(candidatePageSize)}}
	if query != "" {
		values.Set("search", query)
	}
	resp, err := client.Do(ctx, forgeapi.Request{Path: gitlabProjectPath(ref.Project()) + "/users", Query: values})
	if err != nil {
		return PRCandidates{}, err
	}
	var users []gitlabUserRaw
	if err := json.Unmarshal(resp.Body, &users); err != nil {
		return PRCandidates{}, fmt.Errorf("GitLab project users: decode response: %w", err)
	}
	for _, user := range users {
		out.People = append(out.People, user.person())
	}
	out.More = strings.TrimSpace(resp.Header.Get("X-Next-Page")) != ""
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
