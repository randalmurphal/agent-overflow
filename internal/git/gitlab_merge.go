package git

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"

	"agent-overflow/internal/forgeapi"
)

// GitLab's merge actions (ForgeMerger). Readiness and MergeOptions are
// read in gitlab_merge_read.go. Every action returns the transport's
// error as is (*forgeapi.StatusError, *forgeapi.ForbiddenError, ...); the
// app layer explains it (ExplainForgeError). See
// docs/specs/pull-requests.md#actions.

// gitlabRouteMemory remembers, per host and for the process, a server
// without cancel_auto_merge (before 19.5) that answered
// cancel_merge_when_pipeline_succeeds.
type gitlabRouteMemory struct {
	mu           sync.Mutex
	legacyCancel map[string]bool
}

func (m *gitlabRouteMemory) get(host string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.legacyCancel[host]
}

func (m *gitlabRouteMemory) remember(host string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.legacyCancel == nil {
		m.legacyCancel = make(map[string]bool)
	}
	m.legacyCancel[host] = true
}

// Approve posts the viewer's approval of headSHA; GitLab refuses it when
// the merge request's head has moved past it.
func (f *gitlabForge) Approve(ctx context.Context, ref PRReference, headSHA string) error {
	headSHA = strings.TrimSpace(headSHA)
	if !mergeHeadSHAPattern.MatchString(headSHA) {
		return errors.New("approve requires the head commit sha the caller saw")
	}
	client, err := f.core.gitlabAPI(ref.Host)
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
	client, err := f.core.gitlabAPI(ref.Host)
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
// and merge_when_pipeline_succeeds together: 17.11+ reads either, and an
// older server ignores the auto_merge it does not declare and arms on the
// second. The answer is the merge request: merged is Merged, an armed
// merge_when_pipeline_succeeds is AutoMerge, and a merge request still
// open with neither is a merge GitLab runs asynchronously, which the
// pump's next poll shows.
func (f *gitlabForge) Merge(ctx context.Context, ref PRReference, req MergeRequest) (MergeOutcome, error) {
	if err := req.Validate(); err != nil {
		return MergeOutcome{}, err
	}
	client, err := f.core.gitlabAPI(ref.Host)
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
	body := map[string]any{
		"sha":                         strings.TrimSpace(req.HeadSHA),
		"squash":                      req.Method == MergeMethodSquash,
		"should_remove_source_branch": req.DeleteBranch,
	}
	if req.AutoMerge {
		body["auto_merge"] = true
		body["merge_when_pipeline_succeeds"] = true
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

// CancelAutoMerge disarms an armed auto-merge with POST
// .../cancel_auto_merge. A server without that route (404) is asked
// .../cancel_merge_when_pipeline_succeeds, remembered for the host once it
// answers; a 404 there too is the merge request gone and the first 404 is
// returned. The older route answers 201 with {"status":"error"} when
// nothing was armed, which is an error here.
func (f *gitlabForge) CancelAutoMerge(ctx context.Context, ref PRReference) error {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	if !f.routes.get(ref.Host) {
		_, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: gitlabMRPath(ref) + "/cancel_auto_merge"}, nil)
		if !errors.Is(err, forgeapi.ErrNotFound) {
			return err
		}
		answered, legacyErr := f.cancelLegacy(ctx, client, ref)
		if errors.Is(legacyErr, forgeapi.ErrNotFound) {
			return err
		}
		if answered {
			f.routes.remember(ref.Host)
		}
		return legacyErr
	}
	_, err = f.cancelLegacy(ctx, client, ref)
	return err
}

// cancelLegacy posts .../cancel_merge_when_pipeline_succeeds. answered
// reports that the route exists: it answered with its status hash.
func (f *gitlabForge) cancelLegacy(ctx context.Context, client *forgeapi.Client, ref PRReference) (answered bool, err error) {
	var answer struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	request := forgeapi.Request{Method: http.MethodPost, Path: gitlabMRPath(ref) + "/cancel_merge_when_pipeline_succeeds"}
	if _, err := client.JSON(ctx, request, &answer); err != nil {
		return false, err
	}
	if answer.Status == "error" {
		reason := strings.TrimSpace(answer.Message)
		if reason == "" {
			reason = "no auto-merge was set"
		}
		return true, fmt.Errorf("GitLab did not cancel the auto-merge: %s", reason)
	}
	return true, nil
}

// UpdateBranch asks GitLab to rebase the source branch onto the target
// (PUT .../rebase, 202). The rebase runs asynchronously; the pump's next
// poll shows the new head. GitLab's rebase takes no expected head.
func (f *gitlabForge) UpdateBranch(ctx context.Context, ref PRReference, _ string) error {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	_, err = client.JSON(ctx, forgeapi.Request{Method: http.MethodPut, Path: gitlabMRPath(ref) + "/rebase"}, nil)
	return err
}

// gitlabDraftPrefix is GitLab's draft title prefix (MergeRequest
// DRAFT_REGEX): [draft], (draft) or draft:, any case, with the space
// after it.
var gitlabDraftPrefix = regexp.MustCompile(`(?i)^(\[draft\]|\(draft\)|draft:)+\s*`)

// gitlabMaxDraftPrefixes bounds the prefixes stripped, as GitLab does.
const gitlabMaxDraftPrefixes = 20

// gitlabDraftlessTitle is title with every leading draft prefix removed,
// as GitLab's draftless_title computes it.
func gitlabDraftlessTitle(title string) string {
	for range gitlabMaxDraftPrefixes {
		next := gitlabDraftPrefix.ReplaceAllString(title, "")
		if next == title {
			break
		}
		title = next
	}
	return title
}

// MarkReady takes the merge request out of draft: GitLab's draft state is
// its title prefix, so the current title is read and PUT without it. A
// title with no prefix is already ready and sends nothing.
func (f *gitlabForge) MarkReady(ctx context.Context, ref PRReference) error {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	mr, err := readGitLabMR(ctx, client, ref)
	if err != nil {
		return err
	}
	title := gitlabDraftlessTitle(mr.Title)
	if title == mr.Title {
		return nil
	}
	if strings.TrimSpace(title) == "" {
		return errors.New("the merge request title is only a draft prefix; give it a title on GitLab")
	}
	_, err = client.JSON(ctx, forgeapi.Request{Method: http.MethodPut, Path: gitlabMRPath(ref), Body: map[string]string{"title": title}}, nil)
	return err
}
