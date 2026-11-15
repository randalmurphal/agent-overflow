package git

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// GitLab's merge actions (ForgeMerger). Readiness and MergeOptions are
// read in gitlab_merge_read.go. Every action returns the transport's
// error as is (*forgeapi.StatusError, *forgeapi.ForbiddenError, ...); the
// app layer explains it (ExplainForgeError). See
// docs/specs/pull-requests.md#actions.

// Approve posts the viewer's approval of headSHA; GitLab refuses it when
// the merge request's head has moved past it.
func (f *gitlabForge) Approve(ctx context.Context, ref PRReference, headSHA string) error {
	headSHA = strings.TrimSpace(headSHA)
	if !mergeHeadSHAPattern.MatchString(headSHA) {
		return errors.New("approve requires the head commit sha the caller saw")
	}
	client, err := f.writeClient(ctx, ref)
	if err != nil {
		return err
	}
	request := forgeapi.Request{Method: http.MethodPost, Path: gitlabMRPath(ref) + "/approve", Body: map[string]string{"sha": headSHA}}
	_, err = client.JSON(ctx, request, nil)
	return err
}

// Unapprove withdraws the viewer's approval. GitLab keeps one approval per
// user, so the viewer's is the one POST .../unapprove removes.
func (f *gitlabForge) Unapprove(ctx context.Context, ref PRReference) error {
	client, err := f.writeClient(ctx, ref)
	if err != nil {
		return err
	}
	_, err = client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: gitlabMRPath(ref) + "/unapprove"}, nil)
	return err
}

// gitlabMergeAnswer is the merge request PUT .../merge answers with.
type gitlabMergeAnswer struct {
	State                     string `json:"state"`
	MergeWhenPipelineSucceeds bool   `json:"merge_when_pipeline_succeeds"`
	MergeCommitSHA            string `json:"merge_commit_sha"`
	SquashCommitSHA           string `json:"squash_commit_sha"`
	SHA                       string `json:"sha"`
}

// Merge merges now or arms auto-merge with PUT .../merge. GitLab's merge
// takes no method: the project's merge_method decides merge, rebase_merge
// or fast-forward, so req.Method must be one the project allows
// (gitlabMethods) and only squash is sent. Auto-merge sends auto_merge
// ("If true, the merge request is set to auto merge", GitLab 17.11 and
// later). The answer is the merge request: merged is Merged, an armed
// merge_when_pipeline_succeeds is AutoMerge, and a merge request still
// open with neither is a merge GitLab runs asynchronously, which the
// pump's next poll shows. A merge train project joins the train instead
// (gitlabJoinMergeTrain).
func (f *gitlabForge) Merge(ctx context.Context, ref PRReference, req MergeRequest) (MergeOutcome, error) {
	if err := req.Validate(); err != nil {
		return MergeOutcome{}, err
	}
	client, err := f.writeClient(ctx, ref)
	if err != nil {
		return MergeOutcome{}, err
	}
	settings, err := f.projectSettings(ctx, client, ref.Repository())
	if err != nil {
		return MergeOutcome{}, err
	}
	allowed := gitlabMethods(settings, false)
	if len(allowed) == 0 {
		allowed = []string{MergeMethodMerge}
	}
	if !slices.Contains(allowed, req.Method) {
		return MergeOutcome{}, fmt.Errorf("%s does not allow the %s merge method (allowed: %s)", ref.Project(), req.Method, strings.Join(allowed, ", "))
	}
	if settings.MergeTrainsEnabled {
		return gitlabJoinMergeTrain(ctx, client, ref, req)
	}
	body := map[string]any{
		"sha":                         strings.TrimSpace(req.HeadSHA),
		"squash":                      req.Method == MergeMethodSquash,
		"should_remove_source_branch": req.DeleteBranch,
	}
	if m := req.CommitMessage; m != nil {
		// GitLab takes the whole message; the squash commit's is its own
		// field.
		key := "merge_commit_message"
		if req.Method == MergeMethodSquash {
			key = "squash_commit_message"
		}
		body[key] = m.String()
	}
	if req.AutoMerge {
		body["auto_merge"] = true
	}
	var answer gitlabMergeAnswer
	_, err = client.JSON(ctx, forgeapi.Request{Method: http.MethodPut, Path: gitlabMRPath(ref) + "/merge", Body: body}, &answer)
	if err != nil {
		return MergeOutcome{}, err
	}
	switch {
	case NormalizePRState(answer.State) == "merged":
		sha := answer.MergeCommitSHA
		if sha == "" {
			sha = answer.SquashCommitSHA
		}
		if sha == "" {
			sha = answer.SHA
		}
		return MergeOutcome{Merged: true, SHA: sha}, nil
	case answer.MergeWhenPipelineSucceeds:
		return MergeOutcome{AutoMerge: true}, nil
	}
	return MergeOutcome{}, nil
}

// gitlabJoinMergeTrain is Merge on a merge train project (Premium,
// Ultimate), which never merges around the train: a PUT .../merge there
// merges at once ("The commits from the merge request are merged,
// ignoring the status of the merge train"). It POSTs
// merge_trains/merge_requests/N with the head and squash; auto_merge
// "If true, the merge request is added to the merge train when the
// checks pass. When false or unspecified, the merge request is added
// directly to the merge train." (merge trains API, 18.0). Both arm the
// merge request's auto-merge (merge_when_pipeline_succeeds), which
// CancelAutoMerge cancels, taking it off the train. The train takes no
// commit message and no branch delete, so either is refused before the
// write: GitLab writes the message, and the merge request's own "Delete
// source branch" applies.
func gitlabJoinMergeTrain(ctx context.Context, client *forgeapi.Client, ref PRReference, req MergeRequest) (MergeOutcome, error) {
	switch {
	case req.CommitMessage != nil:
		return MergeOutcome{}, &ForgeActionError{Kind: ForgeErrorRefused, Message: "This project merges through a merge train, which takes no commit message."}
	case req.DeleteBranch:
		return MergeOutcome{}, &ForgeActionError{Kind: ForgeErrorRefused, Message: "This project merges through a merge train, which deletes the source branch by the merge request's own setting."}
	}
	body := map[string]any{
		"sha":        strings.TrimSpace(req.HeadSHA),
		"squash":     req.Method == MergeMethodSquash,
		"auto_merge": req.AutoMerge,
	}
	if _, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: gitlabProjectPath(ref.Project()) + "/merge_trains/merge_requests/" + strconv.Itoa(ref.Number), Body: body}, nil); err != nil {
		return MergeOutcome{}, err
	}
	return MergeOutcome{AutoMerge: true}, nil
}

// CancelAutoMerge disarms an armed auto-merge, on a merge train project
// also removing the merge request from its train ("If the merge request
// is on a merge train, this also removes it from the train"). The route
// follows the server's release: POST .../cancel_auto_merge from
// gitlabCancelAutoMergeSince, which answers 409 when nothing is armed,
// else POST .../cancel_merge_when_pipeline_succeeds, which answers 201
// with {"status":"error"} then, an error here.
func (f *gitlabForge) CancelAutoMerge(ctx context.Context, ref PRReference) error {
	client, server, err := f.writeServer(ctx, ref)
	if err != nil {
		return err
	}
	if server.has(gitlabCancelAutoMergeSince) {
		_, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: gitlabMRPath(ref) + "/cancel_auto_merge"}, nil)
		return err
	}
	var answer struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	request := forgeapi.Request{Method: http.MethodPost, Path: gitlabMRPath(ref) + "/cancel_merge_when_pipeline_succeeds"}
	if _, err := client.JSON(ctx, request, &answer); err != nil {
		return err
	}
	if answer.Status == "error" {
		reason := strings.TrimSpace(answer.Message)
		if reason == "" {
			reason = "no auto-merge was set"
		}
		return fmt.Errorf("GitLab did not cancel the auto-merge: %s", reason)
	}
	return nil
}

// UpdateBranch asks GitLab to rebase the source branch onto the target
// (PUT .../rebase, 202). The rebase runs asynchronously; the pump's next
// poll shows the new head. GitLab's rebase takes no expected head.
func (f *gitlabForge) UpdateBranch(ctx context.Context, ref PRReference, _ string) error {
	client, err := f.writeClient(ctx, ref)
	if err != nil {
		return err
	}
	_, err = client.JSON(ctx, forgeapi.Request{Method: http.MethodPut, Path: gitlabMRPath(ref) + "/rebase"}, nil)
	return err
}

// MarkReady takes the merge request out of draft with
// mergeRequestSetDraft, which drops the title's draft prefix on the
// server, so no title read here is written back.
func (f *gitlabForge) MarkReady(ctx context.Context, ref PRReference) error {
	client, err := f.writeClient(ctx, ref)
	if err != nil {
		return err
	}
	return gitlabSetDraft(ctx, client, ref, false)
}
