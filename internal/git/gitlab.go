package git

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// gitlabForge implements Forge over the GitLab REST API through the
// Core's forge API transport. Only CreatePR runs glab.
type gitlabForge struct {
	core *Core
	// facts holds the facts read of every forge kind's keys
	// (forge_facts_gitlab.go).
	facts *factCache[ForgeFact]
	// projects caches each login's merge settings of a project, access
	// its access level there and viewers its account on a host
	// (gitlab_merge_read.go).
	projects gitlabCache[gitlabProjectSettings]
	access   gitlabCache[int]
	viewers  gitlabCache[gitlabViewer]
	// activity is each host's viewer activity, read once per list round
	// (pr_list_gitlab.go).
	activity gitlabActivityRounds
	// pipelineTags remembers which listed pipelines ran on a tag
	// (ci_gitlab.go).
	pipelineTags gitlabPipelineTags
	// servers is each host's release and edition (gitlab_version.go).
	servers gitlabCache[gitlabServer]
	// stateEventRetries holds the merge requests whose state events read
	// failed, until it is due again (gitlab_manage.go).
	stateEventRetries gitlabStateEventRetries
	// reactions is each login's reaction reads per merge request
	// (gitlab_reactions.go).
	reactions gitlabReactionMemory
}

// resetCaches forgets what the adapter remembers of each host and
// project (Core.ResetForgeCaches).
func (f *gitlabForge) resetCaches() {
	f.projects.clear()
	f.access.clear()
	f.viewers.clear()
	f.pipelineTags.forget()
	f.servers.clear()
	f.stateEventRetries.clear()
	f.reactions.clear()
	f.facts.clear()
}

func (f *gitlabForge) ID() string         { return "gitlab" }
func (f *gitlabForge) BinaryName() string { return "glab" }

// CreatePR opens an MR via glab and returns the created URL. The
// `draft` flag becomes `glab mr create --draft`. We rely on glab's
// default "use the current branch" behaviour rather than reading
// HEAD ourselves — same model gh uses, and avoids a hard dep on git
// being on PATH inside tests that exercise the missing-glab path.
func (f *gitlabForge) CreatePR(ctx context.Context, cwd, title, body, base string, draft bool) (string, error) {
	if strings.TrimSpace(title) == "" {
		return "", errors.New("merge request title is required")
	}
	args := []string{
		"mr", "create",
		"--title", title,
		"--description", body,
		"--yes",
		"--no-editor",
	}
	if base = strings.TrimSpace(base); base != "" {
		args = append(args, "--target-branch", base)
	}
	if draft {
		args = append(args, "--draft")
	}
	// Interactive for the same reason as `gh pr create`: glab pushes the
	// source branch itself when the remote does not have it yet, and that
	// nested `git push` inherits our environment.
	result, err := f.core.runBinaryInteractive(ctx, "glab", cwd, args...)
	if err != nil {
		if _, ok := errors.AsType[*exec.Error](err); ok || errors.Is(err, exec.ErrNotFound) {
			return "", forgeapi.MissingCLIError(forgeapi.ForgeGitLab, err)
		}
		return "", err
	}
	if result.exitCode != 0 {
		return "", commandFailure("glab mr create", result)
	}

	url := extractMRCreateURL(result.stdout)
	if url == "" {
		return "", errors.New("glab mr create returned empty URL")
	}
	return url, nil
}

// gitlabOriginProject is the GitLab project cwd's origin names and the
// API client of its host.
func (f *gitlabForge) gitlabOriginProject(cwd string) (*forgeapi.Client, RepoReference, error) {
	forge, host, project, err := f.core.originCoordinates(cwd)
	if err != nil {
		return nil, RepoReference{}, err
	}
	if forge != "gitlab" {
		return nil, RepoReference{}, &OriginUnknownError{Cwd: cwd, Reason: "the origin is not a GitLab repository"}
	}
	namespace, name, err := SplitProjectForForge(forge, project)
	if err != nil {
		return nil, RepoReference{}, &OriginUnknownError{Cwd: cwd, Reason: "the origin URL does not name a gitlab repository"}
	}
	client, err := f.core.gitlabAPI(host)
	if err != nil {
		return nil, RepoReference{}, err
	}
	return client, RepoReference{Forge: forge, Host: host, Namespace: namespace, Repo: name}, nil
}

// gitlabOpenMRsMaxPages bounds the pages one merge request lookup by
// source branch reads past forks' merge requests from a branch of the
// same name.
const gitlabOpenMRsMaxPages = 5

// gitlabHeadMRRaw is one merge request of a lookup by source branch, in
// the full view, the one that carries source_project_id.
type gitlabHeadMRRaw struct {
	WebURL          string          `json:"web_url"`
	IID             int             `json:"iid"`
	Title           string          `json:"title"`
	State           string          `json:"state"`
	Draft           bool            `json:"draft"`
	SourceBranch    string          `json:"source_branch"`
	Author          gitlabAuthorRaw `json:"author"`
	ProjectID       int64           `json:"project_id"`
	SourceProjectID int64           `json:"source_project_id"`
	TargetBranch    string          `json:"target_branch"`
}

// ListOpenPRs returns the open merge requests whose source is the given
// branch of cwd's origin project itself (gitlabMRsByHead).
func (f *gitlabForge) ListOpenPRs(ctx context.Context, cwd, head string) ([]GitPR, error) {
	raw, err := f.gitlabMRsByHead(ctx, cwd, head, "opened")
	if err != nil {
		return nil, err
	}
	pulls := make([]GitPR, 0, len(raw))
	for _, r := range raw {
		pulls = append(pulls, GitPR{URL: r.WebURL, Number: r.IID, Title: r.Title, State: NormalizePRState(r.State), BaseBranch: r.TargetBranch})
	}
	return pulls, nil
}

// ListPRsByHead returns the merge requests in any state whose source is
// the given branch of cwd's origin project itself (gitlabMRsByHead).
func (f *gitlabForge) ListPRsByHead(ctx context.Context, cwd, head string) ([]PRFound, error) {
	raw, err := f.gitlabMRsByHead(ctx, cwd, head, "all")
	if err != nil {
		return nil, err
	}
	found := make([]PRFound, 0, len(raw))
	for _, r := range raw {
		row, err := prFound(r.WebURL, r.Title, r.State, r.Draft, PRListAuthor{Login: r.Author.Username, Name: r.Author.Name}, r.SourceBranch)
		if err != nil {
			return nil, err
		}
		found = append(found, row)
	}
	return found, nil
}

// gitlabMRsByHead returns the merge requests in state whose source is the
// given branch of cwd's origin project itself: those opened in the origin
// from its own branch, and, when the origin is a fork, those opened in
// the project it was forked from (projectSettings, cached) from the
// fork's branch. The source_branch filter also matches another fork's
// branch of the same name, which is someone else's work: a merge request
// counts only when its source project is the origin. GitLab keeps one
// open merge request per source branch and target, so a read of the open
// ones stops at the first that holds one, the origin's before the
// parent's; a read of every state reads both. The full view is read
// because the simple one carries no source_project_id.
func (f *gitlabForge) gitlabMRsByHead(ctx context.Context, cwd, head, state string) ([]gitlabHeadMRRaw, error) {
	sourceBranch := strings.TrimSpace(head)
	if sourceBranch == "" {
		return nil, errors.New("merge request source branch is required")
	}
	client, origin, err := f.gitlabOriginProject(cwd)
	if err != nil {
		return nil, err
	}
	pulls, err := gitlabMRsByHeadIn(ctx, client, gitlabProjectPath(origin.Project()), sourceBranch, state, func(source, target int64) bool { return source == target })
	if err != nil || (state == "opened" && len(pulls) > 0) {
		return pulls, err
	}
	settings, err := f.projectSettings(ctx, client, origin)
	if err != nil {
		return nil, err
	}
	if settings.ForkedFrom == nil || settings.ForkedFrom.ID == 0 || settings.ID == 0 {
		return pulls, nil
	}
	upstream, err := gitlabMRsByHeadIn(ctx, client, gitlabProjectPath(strconv.FormatInt(settings.ForkedFrom.ID, 10)), sourceBranch, state, func(source, _ int64) bool { return source == settings.ID })
	if err != nil {
		return nil, err
	}
	return append(pulls, upstream...), nil
}

// gitlabMRsByHeadIn reads project's merge requests in state from a source
// branch named sourceBranch that keep accepts by their source and target
// project ids, at most gitlabOpenMRsMaxPages pages: of the open ones,
// until a page holds one.
func gitlabMRsByHeadIn(ctx context.Context, client *forgeapi.Client, project, sourceBranch, state string, keep func(source, target int64) bool) ([]gitlabHeadMRRaw, error) {
	request := forgeapi.Request{
		Path:         project + "/merge_requests",
		Query:        url.Values{"state": {state}, "source_branch": {sourceBranch}, "per_page": {strconv.Itoa(gitlabPageSize)}},
		MaxBodyBytes: gitlabMRListMaxBody,
	}
	pulls := []gitlabHeadMRRaw{}
	page, pages := 1, 0
	err := client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		var raw []gitlabHeadMRRaw
		if err := json.Unmarshal(resp.Body, &raw); err != nil {
			return false, fmt.Errorf("GitLab merge requests of branch %s: decode response: %w", sourceBranch, err)
		}
		for _, r := range raw {
			if keep(r.SourceProjectID, r.ProjectID) {
				pulls = append(pulls, r)
			}
		}
		pages++
		return (state != "opened" || len(pulls) == 0) && pages < gitlabOpenMRsMaxPages && len(raw) == gitlabPageSize && advanceGitLabPage(resp, &page), nil
	})
	if err != nil {
		return nil, err
	}
	return pulls, nil
}

// gitlabPageSize is the page size of every list the forge reads, the
// REST API's maximum.
const gitlabPageSize = 100

// ListMergedPRHeads fetches up to limit recently merged MRs' source-branch
// heads, most recently updated first: the origin's, and when the origin
// is a fork, those among its source project's latest limit whose source
// was the fork. `sha` is the source branch's last commit before merge,
// exactly the pre-squash tip prune needs.
func (f *gitlabForge) ListMergedPRHeads(ctx context.Context, cwd string, limit int) ([]MergedPRHead, error) {
	if limit <= 0 {
		return nil, errors.New("merged MR list limit must be positive")
	}
	client, origin, err := f.gitlabOriginProject(cwd)
	if err != nil {
		return nil, err
	}
	heads, err := gitlabMergedHeads(ctx, client, gitlabProjectPath(origin.Project()), limit, func(int64) bool { return true })
	if err != nil {
		return nil, err
	}
	settings, err := f.projectSettings(ctx, client, origin)
	if err != nil {
		return heads, err
	}
	if settings.ForkedFrom == nil || settings.ForkedFrom.ID == 0 || settings.ID == 0 {
		return heads, nil
	}
	upstream, err := gitlabMergedHeads(ctx, client, gitlabProjectPath(strconv.FormatInt(settings.ForkedFrom.ID, 10)), limit, func(source int64) bool { return source == settings.ID })
	if err != nil {
		return heads, err
	}
	return append(heads, upstream...), nil
}

// gitlabMergedHeads reads project's latest limit merged merge requests,
// keeping those keep accepts by their source project id.
func gitlabMergedHeads(ctx context.Context, client *forgeapi.Client, project string, limit int, keep func(source int64) bool) ([]MergedPRHead, error) {
	perPage := min(limit, gitlabPageSize)
	request := forgeapi.Request{
		Path: project + "/merge_requests",
		Query: url.Values{
			"state": {"merged"}, "per_page": {strconv.Itoa(perPage)},
			"order_by": {"updated_at"}, "sort": {"desc"},
		},
		MaxBodyBytes: gitlabMRListMaxBody,
	}
	heads := make([]MergedPRHead, 0, perPage)
	page, read := 1, 0
	err := client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		var raw []struct {
			SourceBranch    string `json:"source_branch"`
			SHA             string `json:"sha"`
			WebURL          string `json:"web_url"`
			SourceProjectID int64  `json:"source_project_id"`
		}
		if err := json.Unmarshal(resp.Body, &raw); err != nil {
			return false, fmt.Errorf("GitLab merged merge requests: decode response: %w", err)
		}
		for _, r := range raw {
			if read == limit {
				break
			}
			read++
			if keep(r.SourceProjectID) {
				heads = append(heads, MergedPRHead{HeadRefName: r.SourceBranch, HeadOid: r.SHA, URL: r.WebURL})
			}
		}
		return read < limit && len(raw) == perPage && advanceGitLabPage(resp, &page), nil
	})
	if err != nil {
		return nil, err
	}
	return heads, nil
}

// advanceGitLabPage reports whether resp names a next page past *page,
// the one it answered, and moves *page to it. A next page that does not
// advance ends the read rather than looping on it.
func advanceGitLabPage(resp *forgeapi.Response, page *int) bool {
	next, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("X-Next-Page")))
	if err != nil || next <= *page {
		return false
	}
	*page = next
	return true
}

// gitlabProjectPath is the REST path of a project, its full path escaped
// into one segment.
func gitlabProjectPath(project string) string {
	return "projects/" + url.PathEscape(project)
}

func gitlabMRPath(ref PRReference) string {
	return gitlabProjectPath(ref.Project()) + "/merge_requests/" + strconv.Itoa(ref.Number)
}

func gitlabDiscussionPath(ref PRReference, discussionID string) string {
	return gitlabMRPath(ref) + "/discussions/" + url.PathEscape(discussionID)
}

// ReadPR reads the merge request once and derives every wanted part from
// it: the detail adds the approvals, the management read (PRManage,
// readManage), the project's merge settings (read once per login per
// project per gitlabCacheTTL) and the viewer's login (once per login per
// host per gitlabCacheTTL), the threads and the events the discussions
// (paged; the system notes are events, the rest threads), the events
// also the resource state events (closes, reopens and merges; a failure
// leaves them out, gitlabForge.stateEvents), CI
// the head pipeline's rows (paged, readGitLabPipeline). A part not
// wanted costs no request. prev and stepsFor are unused: the
// rows are one read the previous observation cannot stand in for, and
// GitLab jobs have no steps.
func (f *gitlabForge) ReadPR(ctx context.Context, ref PRReference, want PRReadParts, _ *CIPipeline, _ []string) (PRRead, error) {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return PRRead{}, err
	}
	mr, err := readGitLabMR(ctx, client, ref)
	if err != nil {
		return PRRead{}, err
	}
	var out PRRead
	var notes []gitlabNoteRaw
	var states []gitlabStateEventRaw
	var threads []ReviewThread
	if want.Threads || want.Events {
		var system []gitlabNoteRaw
		var truncated bool
		threads, system, truncated, err = readGitLabDiscussions(ctx, client, ref, gitlabThreadContext{headSHA: mr.headSHA(), webURL: mr.WebURL})
		if err != nil {
			return PRRead{}, err
		}
		notes = system
		if want.Threads {
			out.Threads = threads
			if truncated {
				out.ThreadsTruncated = threadsTruncatedSentence(ref)
			}
		}
	}
	if want.Events {
		states = f.stateEvents(ctx, client, ref)
	}
	// The detail's armed auto-merge takes its time and wording from the
	// latest auto-merge event when the same read has the notes.
	events := gitlabEvents(mr, notes, states)
	if want.Events {
		out.Events = events
	}
	if want.Detail {
		var approvals gitlabApprovalsRaw
		if _, err := client.JSON(ctx, forgeapi.Request{Path: gitlabMRPath(ref) + "/approvals"}, &approvals); err != nil {
			return PRRead{}, err
		}
		settings, err := f.projectSettings(ctx, client, ref.Repository())
		if err != nil {
			return PRRead{}, err
		}
		// The role words why the viewer cannot merge.
		access := 0
		if !mr.User.CanMerge {
			if access, err = f.viewerAccess(ctx, client, ref.Repository()); err != nil {
				return PRRead{}, err
			}
		}
		// The Details read is the detail's own: its failure is said in
		// the detail and hides nothing else.
		manage, err := f.readManage(ctx, client, ref, mr)
		if err != nil {
			if ctx.Err() != nil {
				return PRRead{}, ctx.Err()
			}
			manage = unavailableManage(ref, err)
		}
		viewer := f.viewerLogin(ctx, client, ref.Host)
		out.Detail = gitlabPRDetail(mr, gitlabApprovalVerdicts(approvals))
		gitlabFillMerge(&out.Detail, mr, approvals, settings, access, events, viewer)
		if settings.MergeTrainsEnabled && NormalizePRState(mr.State) == "open" {
			if out.Detail.Merge.Queue, err = readGitLabMergeTrain(ctx, client, ref, mr, mr.User.CanMerge || out.Detail.ViewerIsAuthor); err != nil {
				return PRRead{}, err
			}
		}
		manage.apply(&out.Detail)
	}
	if want.Threads {
		// Who may edit and delete a note is its author and, for every note,
		// a maintainer or owner (GitLab's notes carry no permission). The
		// login and the role are the detail's cached reads, so a tick that
		// reads both costs no request of its own for them.
		access, err := f.viewerAccess(ctx, client, ref.Repository())
		if err != nil {
			return PRRead{}, err
		}
		viewer := f.viewerLogin(ctx, client, ref.Host)
		markGitLabViewerNotes(out.Threads, viewer, access)
		gateGitLabThreadResolve(out.Threads, viewer != "" && strings.EqualFold(viewer, mr.Author.Username), access)
		gateGitLabSuggestions(out.Threads, access)
		out.ReactionsUnavailable = f.fillReactions(ctx, client, ref, out.Threads)
	}
	if want.CI {
		if out.CI, err = f.readGitLabPipeline(ctx, client, ref.Repository(), mr.HeadPipeline); err != nil {
			return PRRead{}, err
		}
		out.HasCI = true
	}
	return out, nil
}

// readGitLabMR reads the merge request itself. The single-MR endpoint is
// the one that carries diff_refs and head_pipeline; the list omits both.
func readGitLabMR(ctx context.Context, client *forgeapi.Client, ref PRReference) (gitlabMRRaw, error) {
	var mr gitlabMRRaw
	if _, err := client.JSON(ctx, forgeapi.Request{Path: gitlabMRPath(ref)}, &mr); err != nil {
		return gitlabMRRaw{}, err
	}
	return mr, nil
}

// gitlabMRRaw is the subset of a GitLab merge request the forge reads.
type gitlabMRRaw struct {
	IID                 int             `json:"iid"`
	Title               string          `json:"title"`
	Description         string          `json:"description"`
	SourceBranch        string          `json:"source_branch"`
	TargetBranch        string          `json:"target_branch"`
	SHA                 string          `json:"sha"`
	WebURL              string          `json:"web_url"`
	State               string          `json:"state"`
	Draft               bool            `json:"draft"`
	WorkInProgress      bool            `json:"work_in_progress"`
	ChangesCount        string          `json:"changes_count"`
	HasConflicts        bool            `json:"has_conflicts"`
	DetailedMergeStatus string          `json:"detailed_merge_status"`
	Author              gitlabAuthorRaw `json:"author"`
	DiffRefs            struct {
		BaseSHA  string `json:"base_sha"`
		HeadSHA  string `json:"head_sha"`
		StartSHA string `json:"start_sha"`
	} `json:"diff_refs"`
	HeadPipeline *gitlabPipelineRaw `json:"head_pipeline"`
	// The merge fields (gitlab_merge_read.go). GitLab answers a user it
	// has none for as an object of nulls.
	ProjectID                   int64           `json:"project_id"`
	SourceProjectID             int64           `json:"source_project_id"`
	MergedAt                    string          `json:"merged_at"`
	MergedBy                    gitlabAuthorRaw `json:"merged_by"`
	ClosedAt                    string          `json:"closed_at"`
	ClosedBy                    gitlabAuthorRaw `json:"closed_by"`
	MergeUser                   gitlabAuthorRaw `json:"merge_user"`
	MergeWhenPipelineSucceeds   bool            `json:"merge_when_pipeline_succeeds"`
	MergeAfter                  string          `json:"merge_after"`
	Squash                      bool            `json:"squash"`
	ForceRemoveSourceBranch     *bool           `json:"force_remove_source_branch"`
	BlockingDiscussionsResolved bool            `json:"blocking_discussions_resolved"`
	MergeCommitSHA              string          `json:"merge_commit_sha"`
	// Reviewers and Assignees are the sets a reviewer or assignee change
	// replaces (gitlab_manage.go).
	Reviewers []gitlabUserRaw `json:"reviewers"`
	Assignees []gitlabUserRaw `json:"assignees"`
	User      struct {
		CanMerge bool `json:"can_merge"`
	} `json:"user"`
}

// gitlabPipelineRaw is a pipeline as GitLab answers it: a merge
// request's head pipeline, a pipeline list row or one pipeline read.
type gitlabPipelineRaw struct {
	ID        int64  `json:"id"`
	Status    string `json:"status"`
	WebURL    string `json:"web_url"`
	Ref       string `json:"ref"`
	SHA       string `json:"sha"`
	Source    string `json:"source"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// headSHA is the merge request's head commit: the diff's head, else the
// source branch's.
func (m gitlabMRRaw) headSHA() string {
	if m.DiffRefs.HeadSHA != "" {
		return m.DiffRefs.HeadSHA
	}
	return m.SHA
}

func (m gitlabMRRaw) diffRefs() *PRDiffRefs {
	return &PRDiffRefs{BaseSHA: m.DiffRefs.BaseSHA, HeadSHA: m.DiffRefs.HeadSHA, StartSHA: m.DiffRefs.StartSHA}
}

type gitlabApprovalsRaw struct {
	ApprovalsRequired int  `json:"approvals_required"`
	ApprovalsLeft     int  `json:"approvals_left"`
	UserHasApproved   bool `json:"user_has_approved"`
	UserCanApprove    bool `json:"user_can_approve"`
	// RequirePasswordToApprove is the project's "Require user
	// re-authentication to approve" with password sign-in enabled
	// (Enterprise Edition; ee/lib/ee/api/entities/approval_state.rb at
	// v18.0.0-ee). The approve endpoint then needs approval_password:
	// "Required if Require user re-authentication to approve is enabled".
	RequirePasswordToApprove bool `json:"require_password_to_approve"`
	ApprovedBy               []struct {
		ApprovedAt string          `json:"approved_at"`
		User       gitlabAuthorRaw `json:"user"`
	} `json:"approved_by"`
}

func gitlabPRDetail(raw gitlabMRRaw, approvals []ReviewVerdict) PRDetail {
	return PRDetail{
		Number:         raw.IID,
		Title:          raw.Title,
		Body:           raw.Description,
		AuthorLogin:    raw.Author.Username,
		AuthorName:     raw.Author.Name,
		State:          NormalizePRState(raw.State),
		Draft:          raw.Draft || raw.WorkInProgress,
		HeadRefName:    raw.SourceBranch,
		BaseRefName:    raw.TargetBranch,
		BaseRefSHA:     raw.DiffRefs.StartSHA,
		HeadSHA:        raw.headSHA(),
		URL:            raw.WebURL,
		ChangedFiles:   parseGitLabChangesCount(raw.ChangesCount),
		ReviewDecision: gitlabReviewDecision(approvals),
		LatestReviews:  approvals,
		Checks:         gitlabCheckSummary(raw.HeadPipeline),
		Mergeability:   normalizeGitLabMergeability(raw.HasConflicts, raw.DetailedMergeStatus),
		DiffRefs:       raw.diffRefs(),
	}
}

func parseGitLabChangesCount(value string) int {
	value = strings.TrimSuffix(strings.TrimSpace(value), "+")
	n, _ := strconv.Atoi(value)
	return n
}

func gitlabCheckSummary(pipeline *gitlabPipelineRaw) CheckSummary {
	if pipeline == nil || pipeline.Status == "" {
		return CheckSummary{}
	}
	check := CheckStatus{Kind: "Pipeline", Name: "Pipeline", Status: pipeline.Status, DetailsURL: pipeline.WebURL}
	summary := CheckSummary{Total: 1, Checks: []CheckStatus{check}}
	// GitLab's pipeline statuses, not GitHub's conclusions that
	// addCheckBucket sorts.
	switch NormalizeCIStatus(pipeline.Status, "") {
	case CIStatusSuccess:
		summary.Success++
	case CIStatusFailed:
		summary.Failure++
	case CIStatusCanceled:
		summary.Canceled++
	case CIStatusSkipped:
		summary.Skipped++
	default:
		summary.Pending++
	}
	return summary
}

func gitlabApprovalVerdicts(raw gitlabApprovalsRaw) []ReviewVerdict {
	out := make([]ReviewVerdict, 0, len(raw.ApprovedBy))
	for _, approval := range raw.ApprovedBy {
		if approval.User.Username == "" {
			continue
		}
		out = append(out, ReviewVerdict{
			AuthorLogin: approval.User.Username,
			AuthorName:  approval.User.Name,
			State:       "APPROVED",
			SubmittedAt: approval.ApprovedAt,
		})
	}
	return out
}

func gitlabReviewDecision(approvals []ReviewVerdict) string {
	if len(approvals) == 0 {
		return ""
	}
	return "APPROVED"
}

func normalizeGitLabMergeability(hasConflicts bool, detailed string) string {
	if hasConflicts {
		return MergeabilityConflicts
	}
	switch strings.ToLower(strings.TrimSpace(detailed)) {
	case "checking":
		return MergeabilityChecking
	case "conflict", "cannot_be_merged":
		return MergeabilityConflicts
	default:
		return MergeabilityClean
	}
}

// gitlabThreadContext is what normalizing a discussion needs beside it:
// the merge request's current head (outdated threads) and its web URL
// (each note's link).
type gitlabThreadContext struct {
	headSHA, webURL string
}

// gitlabMaintainerAccess is the access level GitLab lets edit and
// delete any note ("Edit comments posted by other users": Maintainer and
// Owner).
const gitlabMaintainerAccess = 40

// markGitLabViewerNotes sets who may edit and delete each note: the
// viewer on its own notes, and on every note with access (the viewer's
// project role) of a maintainer or owner.
func markGitLabViewerNotes(threads []ReviewThread, viewer string, access int) {
	maintainer := access >= gitlabMaintainerAccess
	for i := range threads {
		for j := range threads[i].Comments {
			comment := &threads[i].Comments[j]
			may := maintainer || (viewer != "" && strings.EqualFold(comment.AuthorLogin, viewer))
			comment.ViewerCanEdit, comment.ViewerCanDelete = may, may
		}
	}
}

// gateGitLabThreadResolve says on each resolvable thread that the viewer
// cannot resolve it: GitLab lets the merge request's author and the
// developer role and above resolve threads, and its discussions carry no
// permission. An unknown role (0) gates nothing; the forge refuses.
func gateGitLabThreadResolve(threads []ReviewThread, author bool, access int) {
	if author || access == 0 || access >= gitlabDeveloperAccess {
		return
	}
	for i := range threads {
		if threads[i].IsResolvable {
			threads[i].CannotResolveReason = gitlabCannotResolveReason
		}
	}
}

const gitlabCannotResolveReason = "Only the merge request's author and people with the Developer role or above can resolve threads."

// readGitLabDiscussions reads the merge request's discussions, at most
// prReadMaxPages pages (truncated says there were more): the human notes
// as threads normalized against the MR's current head commit, and the
// system notes in GitLab's order, which are the events' source
// (gitlabEvents).
func readGitLabDiscussions(ctx context.Context, client *forgeapi.Client, ref PRReference, at gitlabThreadContext) (_ []ReviewThread, _ []gitlabNoteRaw, truncated bool, _ error) {
	request := forgeapi.Request{
		Path:         gitlabMRPath(ref) + "/discussions",
		Query:        url.Values{"per_page": {strconv.Itoa(gitlabPageSize)}},
		MaxBodyBytes: gitlabDiscussionsMaxBody,
	}
	var threads []ReviewThread
	var system []gitlabNoteRaw
	page, pages := 1, 0
	err := client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		pages++
		var discussions []gitlabDiscussionRaw
		if err := json.Unmarshal(resp.Body, &discussions); err != nil {
			return false, fmt.Errorf("GitLab merge request discussions: decode response: %w", err)
		}
		threads = append(threads, gitlabReviewThreads(discussions, at)...)
		for _, discussion := range discussions {
			for _, note := range discussion.Notes {
				if note.System {
					system = append(system, note)
				}
			}
		}
		more := advanceGitLabPage(resp, &page)
		truncated = more && pages >= prReadMaxPages
		return more && !truncated, nil
	})
	if err != nil {
		return nil, nil, false, err
	}
	return threads, system, truncated, nil
}

func gitlabReviewThreads(discussions []gitlabDiscussionRaw, at gitlabThreadContext) []ReviewThread {
	threads := make([]ReviewThread, 0, len(discussions))
	for _, discussion := range discussions {
		thread, ok := normalizeGitLabDiscussion(discussion, at)
		if !ok {
			continue
		}
		threads = append(threads, thread)
	}
	return threads
}

type gitlabDiscussionRaw struct {
	ID       string          `json:"id"`
	Notes    []gitlabNoteRaw `json:"notes"`
	Resolved *bool           `json:"resolved"`
}

type gitlabNoteRaw struct {
	ID         int64              `json:"id"`
	Body       string             `json:"body"`
	System     bool               `json:"system"`
	CreatedAt  string             `json:"created_at"`
	Position   *gitlabPositionRaw `json:"position"`
	Resolvable bool               `json:"resolvable"`
	Resolved   *bool              `json:"resolved"`
	Author     gitlabAuthorRaw    `json:"author"`
	// Suggestions are the note's suggestion blocks as GitLab parsed them.
	Suggestions []gitlabSuggestionRaw `json:"suggestions"`
}

// gitlabAuthorRaw is GitLab's user summary: MR author, note author and
// approval user all carry username and name.
type gitlabAuthorRaw struct {
	Username string `json:"username"`
	Name     string `json:"name"`
}

type gitlabPositionRaw struct {
	BaseSHA      string              `json:"base_sha,omitempty"`
	StartSHA     string              `json:"start_sha,omitempty"`
	HeadSHA      string              `json:"head_sha,omitempty"`
	OldPath      string              `json:"old_path,omitempty"`
	NewPath      string              `json:"new_path,omitempty"`
	PositionType string              `json:"position_type,omitempty"`
	OldLine      *int                `json:"old_line,omitempty"`
	NewLine      *int                `json:"new_line,omitempty"`
	LineRange    *gitlabLineRangeRaw `json:"line_range,omitempty"`
}

type gitlabLineRangeRaw struct {
	Start gitlabLineRangePoint `json:"start"`
	End   gitlabLineRangePoint `json:"end"`
}

type gitlabLineRangePoint struct {
	LineCode string `json:"line_code,omitempty"`
	Type     string `json:"type,omitempty"`
	OldLine  *int   `json:"old_line,omitempty"`
	NewLine  *int   `json:"new_line,omitempty"`
}

func normalizeGitLabDiscussion(discussion gitlabDiscussionRaw, at gitlabThreadContext) (ReviewThread, bool) {
	currentHeadSHA := at.headSHA
	var root *gitlabNoteRaw
	comments := make([]ReviewComment, 0, len(discussion.Notes))
	resolvable := discussion.Resolved != nil
	resolved := false
	if discussion.Resolved != nil {
		resolved = *discussion.Resolved
	}
	for i := range discussion.Notes {
		note := discussion.Notes[i]
		if note.System {
			continue
		}
		if root == nil {
			// The first human note anchors the thread: a positioned note
			// makes it a diff thread, an unpositioned one a PR-level
			// conversation thread.
			root = &discussion.Notes[i]
			resolvable = resolvable || note.Resolvable
			if note.Resolved != nil {
				resolved = *note.Resolved
			}
		}
		comment := ReviewComment{
			ID:          strconv.FormatInt(note.ID, 10),
			AuthorLogin: note.Author.Username,
			AuthorName:  note.Author.Name,
			Body:        note.Body,
			CreatedAt:   note.CreatedAt,
			DatabaseID:  note.ID,
		}
		if at.webURL != "" {
			comment.URL = at.webURL + "#note_" + comment.ID
		}
		comment.Suggestions = gitlabSuggestions(note.Suggestions, note.Body)
		comments = append(comments, comment)
	}
	if root == nil || len(comments) == 0 {
		return ReviewThread{}, false
	}
	if root.Position == nil {
		return ReviewThread{
			ID:           discussion.ID,
			IsResolvable: resolvable,
			IsResolved:   resolved,
			Comments:     comments,
		}, true
	}
	path, line, startLine, side, startSide := normalizeGitLabPosition(root.Position)
	return ReviewThread{
		ID:           discussion.ID,
		Path:         path,
		Line:         line,
		StartLine:    startLine,
		Side:         side,
		StartSide:    startSide,
		IsResolvable: true,
		IsResolved:   resolved,
		IsOutdated:   currentHeadSHA != "" && root.Position.HeadSHA != "" && root.Position.HeadSHA != currentHeadSHA,
		Comments:     comments,
	}, true
}

func normalizeGitLabPosition(position *gitlabPositionRaw) (path string, line, startLine *int, side, startSide string) {
	path = position.NewPath
	if path == "" {
		path = position.OldPath
	}
	if strings.EqualFold(position.PositionType, "file") {
		return path, nil, nil, "file", ""
	}
	// A range is its line_range: its side and last line are its end's,
	// its first line the start's number on the start's own half (as
	// GitHub's startLine and startDiffSide), whichever numbers the
	// position itself carries.
	if lr := position.LineRange; lr != nil {
		if side, line := gitlabPointLine(lr.End, ""); line != nil {
			startSide, start := gitlabPointLine(lr.Start, side)
			if start == nil {
				startSide = ""
			}
			return path, line, start, side, startSide
		}
	}
	if position.NewLine == nil && position.OldLine != nil {
		return path, position.OldLine, nil, "left", ""
	}
	return path, position.NewLine, nil, "right", ""
}

// gitlabPointLine is a line_range point's half and its number there: a
// removed line (type old) on the left, an added one (type new) on the
// right, an unchanged one on prefer's half ("right" when empty) when it
// has that number, else on the half it has.
func gitlabPointLine(point gitlabLineRangePoint, prefer string) (string, *int) {
	switch {
	case point.Type == "old":
		return "left", point.OldLine
	case point.Type == "new":
		return "right", point.NewLine
	case prefer == "left" && point.OldLine != nil, point.NewLine == nil:
		return "left", point.OldLine
	}
	return "right", point.NewLine
}

// SubmitReview creates the review's draft notes, publishes them, and
// writes the verdict last. Past the caller's deadline it starts no request
// (writeSteps): before the publish it removes the drafts it created and
// posts nothing; after it, the verdict it did not start is reported as not
// posted.
func (f *gitlabForge) SubmitReview(ctx context.Context, ref PRReference, review SubmitReviewRequest) (SubmitReviewResult, error) {
	steps, done := writeSteps(ctx)
	defer done()
	client, server, err := f.writeServer(steps, ref)
	if err != nil {
		return SubmitReviewResult{}, err
	}
	// The draft notes are positioned on the diff the reviewer loaded, which
	// is the MR's only while its head is the one they loaded; the approval
	// names that head too, so a push after this read is refused by GitLab.
	mr, err := readGitLabMR(steps, client, ref)
	if err != nil {
		return SubmitReviewResult{}, err
	}
	if have := mr.headSHA(); !strings.EqualFold(have, review.HeadSHA) {
		return SubmitReviewResult{}, &HeadMovedError{Want: review.HeadSHA, Have: have}
	}
	// The verdict's own write comes last: the approval, or the
	// changes-requested thread. An approval GitLab cannot take from the app
	// refuses the review here, before any draft is written.
	var verdict forgeapi.Request
	step := ""
	mrPath := gitlabMRPath(ref)
	// Request changes sets the viewer's reviewer state where the publish
	// takes it, and always posts its summary as a thread to resolve.
	requestChanges := strings.EqualFold(review.Verdict, ReviewVerdictRequestChanges)
	switch {
	case strings.EqualFold(review.Verdict, ReviewVerdictApprove):
		if verdict, err = gitlabApproveRequest(steps, client, ref, review.HeadSHA); err != nil {
			return SubmitReviewResult{}, err
		}
		step = SubmitStepApproval
	case requestChanges:
		verdict, step = forgeapi.Request{Method: http.MethodPost, Path: mrPath + "/discussions", Body: map[string]string{"body": gitlabChangesThreadBody(review.Body)}}, SubmitStepChangesThread
	}
	diffRefs := mr.diffRefs()
	notes := gitlabDraftNotes(review)
	var publish any
	if requestChanges && server.has(gitlabReviewerStateSince) {
		publish = map[string]string{"reviewer_state": "requested_changes"}
	}
	// The publish runs for a draft or a reviewer state, and takes every
	// pending draft note of the viewer's, so one this submit did not create
	// (a review started on GitLab, or one an earlier submit could not
	// remove) refuses any submit that publishes, before any write.
	publishes := len(notes) > 0 || publish != nil
	if publishes {
		pending, err := gitlabPendingDraftIDs(steps, client, mrPath)
		if err != nil {
			return SubmitReviewResult{}, err
		}
		if len(pending) > 0 {
			return SubmitReviewResult{}, &PendingDraftNotesError{Count: len(pending)}
		}
	}
	var replies []gitlabHeldReply
	for _, note := range notes {
		if note.InReplyTo != nil {
			replies = append(replies, gitlabHeldReply{ordinal: len(replies), comment: note})
		}
	}
	// Held replies: every discussion they answer exists now, and each
	// draft keeps its discussion resolved or open (gitlab_replies.go).
	var before gitlabReplyThreads
	if len(replies) > 0 {
		if before, err = readGitLabReplyThreads(steps, client, ref, mr); err != nil {
			return SubmitReviewResult{}, err
		}
		for _, reply := range replies {
			if threadID := reply.comment.InReplyTo.ThreadID; before[threadID].ID == "" {
				return SubmitReviewResult{}, &ReplyThreadGoneError{ThreadID: threadID, Err: errors.New("the merge request has no such discussion")}
			}
		}
	}
	var files map[string]gitlabDiffFile
	if paths := gitlabPositionedPaths(notes); len(paths) > 0 {
		if files, err = readGitLabDiffFiles(steps, client, ref, paths); err != nil {
			return SubmitReviewResult{}, err
		}
	}
	bodies := make([]gitlabDraftNoteIn, 0, len(notes))
	for _, note := range notes {
		body, err := gitlabDraftNoteBody(note, diffRefs, files)
		if err != nil {
			return SubmitReviewResult{}, err
		}
		if body.InReplyToDiscussionID != "" {
			body.ResolveDiscussion = before.resolveFor(body.InReplyToDiscussionID)
		}
		bodies = append(bodies, body)
	}
	// A submit is several requests. One that fails before the drafts are
	// published removes the drafts this submit created, by the ids GitLab
	// returned for them, so a retry does not publish them twice. A create
	// the forge may have applied leaves one draft with no id to remove.
	created := make([]int64, 0, len(bodies))
	for _, body := range bodies {
		var draft struct {
			ID int64 `json:"id"`
		}
		if err := stopped(steps); err != nil {
			return SubmitReviewResult{}, removeGitLabDrafts(ctx, client, mrPath, created, 0, err)
		}
		if _, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: mrPath + "/draft_notes", Body: body}, &draft); err != nil {
			if body.InReplyToDiscussionID != "" && gitlabReplyRefused(steps, client, ref, body.InReplyToDiscussionID, err) {
				err = &ReplyThreadGoneError{ThreadID: body.InReplyToDiscussionID, Err: err}
			}
			unknown := 1
			if forgeapi.WriteNotApplied(err) {
				unknown = 0
			}
			return SubmitReviewResult{}, removeGitLabDrafts(ctx, client, mrPath, created, unknown, err)
		}
		if draft.ID == 0 {
			err := errors.New("GitLab created a draft note but did not return its id")
			return SubmitReviewResult{}, removeGitLabDrafts(ctx, client, mrPath, created, 1, err)
		}
		created = append(created, draft.ID)
	}
	out := SubmitReviewResult{}
	if publishes {
		if err := stopped(steps); err != nil {
			return SubmitReviewResult{}, removeGitLabDrafts(ctx, client, mrPath, created, 0, err)
		}
		if _, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: mrPath + "/draft_notes/bulk_publish", Body: publish}, nil); err != nil {
			if forgeapi.WriteNotApplied(err) {
				return SubmitReviewResult{}, removeGitLabDrafts(ctx, client, mrPath, created, 0, err)
			}
			// The publish may have run: the drafts it would publish say.
			published, failure := resolveGitLabPublish(ctx, client, mrPath, created, err)
			if !published {
				return SubmitReviewResult{}, failure
			}
		}
		out.PostedReview = true
		// The publish published every draft: each file comment is up,
		// whatever becomes of the replies and the verdict.
		out.PostedFileComments = gitlabFileComments(review.Comments)
	}
	// A published reply whose discussion went meanwhile was dropped:
	// GitLab says nothing, so the discussions say.
	var replyFailure *PartialSubmitError
	if len(replies) > 0 {
		replyFailure = f.confirmGitLabReplies(steps, client, ref, mr, before, replies, &out)
	}
	if step != "" {
		err := stopped(steps)
		if err == nil {
			_, err = client.JSON(ctx, verdict, nil)
		}
		if err != nil {
			if replyFailure == nil {
				return out, gitlabVerdictFailure(out, step, err)
			}
			// Two outcomes: the replies' and the verdict's, both reported.
			replyFailure.Verdict, replyFailure.VerdictErr = step, err
			replyFailure.Unconfirmed = replyFailure.Unconfirmed || !forgeapi.WriteNotApplied(err)
			return out, replyFailure
		}
		out.PostedReview = true
	}
	if replyFailure != nil {
		return out, replyFailure
	}
	return out, nil
}

// confirmGitLabReplies counts the published replies that are notes of
// their discussions into out, and returns the failure that names the
// first one that is not (nil when all are). A discussions read that fails
// leaves every reply unconfirmed.
func (f *gitlabForge) confirmGitLabReplies(ctx context.Context, client *forgeapi.Client, ref PRReference, mr gitlabMRRaw, before gitlabReplyThreads, replies []gitlabHeldReply, out *SubmitReviewResult) *PartialSubmitError {
	all := make([]int, 0, len(replies))
	for _, reply := range replies {
		all = append(all, reply.ordinal)
	}
	lost, err := lostGitLabReplies(ctx, client, ref, mr, before, replies, f.viewerLogin(ctx, client, ref.Host))
	if err != nil {
		return &PartialSubmitError{PostedReview: out.PostedReview, PostedFileComments: out.PostedFileComments, FailedThreadID: replies[0].comment.InReplyTo.ThreadID, UnpostedReplies: all, Unconfirmed: true,
			Err: fmt.Errorf("read the discussions to confirm the replies: %w", err)}
	}
	out.PostedReplies = len(replies) - len(lost)
	if len(lost) == 0 {
		return nil
	}
	threadID := replies[lost[0]].comment.InReplyTo.ThreadID
	return &PartialSubmitError{PostedReview: out.PostedReview, PostedFileComments: out.PostedFileComments, PostedReplies: out.PostedReplies, FailedThreadID: threadID, UnpostedReplies: lost,
		Err: &ReplyThreadGoneError{ThreadID: threadID, Err: errors.New("GitLab published the review without this reply")}}
}

// gitlabReplyRefused reports whether err, a draft reply's create failure,
// is its discussion gone: GitLab answers a 404 for a merge request it
// cannot find and a 400 for a discussion it cannot, which the discussion's
// own read tells from any other 400.
func gitlabReplyRefused(ctx context.Context, client *forgeapi.Client, ref PRReference, threadID string, err error) bool {
	if errors.Is(err, forgeapi.ErrNotFound) {
		return true
	}
	status, ok := errors.AsType[*forgeapi.StatusError](err)
	return ok && status.Status == http.StatusBadRequest && gitlabDiscussionGone(ctx, client, ref, threadID)
}

// resolveGitLabPublish settles a bulk_publish whose answer did not say
// whether it ran, by reading the viewer's pending draft notes: every one
// this submit created gone means it published; every one still pending
// means it did not, and they are removed. Anything else, or a failed
// read, is an UnconfirmedSubmitError around publishErr.
func resolveGitLabPublish(ctx context.Context, client *forgeapi.Client, mrPath string, created []int64, publishErr error) (bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), SubmitSettleTimeout)
	defer cancel()
	// A publish of the reviewer state alone leaves no draft to tell by.
	if len(created) == 0 {
		return false, &UnconfirmedSubmitError{Err: publishErr}
	}
	pending, err := gitlabPendingDraftIDs(ctx, client, mrPath)
	if err != nil {
		return false, &UnconfirmedSubmitError{Err: errors.Join(publishErr, fmt.Errorf("read the pending draft notes: %w", err))}
	}
	still := 0
	for _, id := range created {
		if pending[id] {
			still++
		}
	}
	switch still {
	case 0:
		return true, nil
	case len(created):
		return false, removeGitLabDrafts(ctx, client, mrPath, created, 0, publishErr)
	}
	return false, &UnconfirmedSubmitError{Err: fmt.Errorf("%w; %d of %d draft notes are still pending", publishErr, still, len(created))}
}

// gitlabPendingDraftIDs are the ids of the viewer's pending draft notes on
// the merge request at mrPath.
func gitlabPendingDraftIDs(ctx context.Context, client *forgeapi.Client, mrPath string) (map[int64]bool, error) {
	ids := map[int64]bool{}
	request := forgeapi.Request{Path: mrPath + "/draft_notes", Query: url.Values{"per_page": {strconv.Itoa(gitlabPageSize)}}}
	err := client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		var page []struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(resp.Body, &page); err != nil {
			return false, fmt.Errorf("GitLab draft notes: decode response: %w", err)
		}
		for _, note := range page {
			ids[note.ID] = true
		}
		return true, nil
	})
	return ids, err
}

// gitlabVerdictFailure wraps the verdict write's error (step) after the
// publish posted. A plain error would read as "nothing posted" and make
// the caller keep (and later double-post) every comment; the typed
// partial error carries the posted state instead. A verdict write alone
// is the submit's only write, so its error decides as any write's does.
func gitlabVerdictFailure(out SubmitReviewResult, step string, err error) error {
	if !out.PostedReview {
		return submitWriteError(err)
	}
	return &PartialSubmitError{PostedReview: true, PostedFileComments: out.PostedFileComments, Unconfirmed: !forgeapi.WriteNotApplied(err), Verdict: step, Err: err}
}

// gitlabChangesThreadBody is the changes-requested thread's text.
func gitlabChangesThreadBody(summary string) string {
	return "Changes requested:\n\n" + strings.TrimSpace(summary)
}

// gitlabFileComments counts the file comments among comments.
func gitlabFileComments(comments []ReviewLineComment) int {
	n := 0
	for _, comment := range comments {
		if comment.InReplyTo == nil && strings.EqualFold(comment.Side, "file") {
			n++
		}
	}
	return n
}

// gitlabDraftNotes are the review's draft notes: a summary (but Request
// changes', which is its own thread), then the comments.
func gitlabDraftNotes(review SubmitReviewRequest) []ReviewLineComment {
	notes := make([]ReviewLineComment, 0, len(review.Comments)+1)
	if body := review.Body; strings.TrimSpace(body) != "" && !strings.EqualFold(review.Verdict, ReviewVerdictRequestChanges) {
		notes = append(notes, ReviewLineComment{Body: body, Side: "summary"})
	}
	notes = append(notes, review.Comments...)
	return notes
}

// gitlabDraftNoteIn is the REST body of one draft note: a summary, a
// positioned comment, or a reply to a discussion.
type gitlabDraftNoteIn struct {
	Note                  string             `json:"note"`
	Position              *gitlabPositionRaw `json:"position,omitempty"`
	InReplyToDiscussionID string             `json:"in_reply_to_discussion_id,omitempty"`
	// ResolveDiscussion is a reply's discussion state to keep: GitLab
	// reopens a resolved discussion a reply is published into unless it
	// says resolve.
	ResolveDiscussion *bool `json:"resolve_discussion,omitempty"`
}

// removeGitLabDrafts deletes the drafts with ids, the ones a failed submit
// created, and no other, then returns failure. unknown counts drafts the
// submit created without learning their ids. A draft it could not remove
// is reported as a *DraftsLeftError around failure. A draft already gone
// (404) was not removed by this submit: another publish of the viewer's
// drafts may have posted it, so failure becomes an UnconfirmedSubmitError.
func removeGitLabDrafts(ctx context.Context, client *forgeapi.Client, mrPath string, ids []int64, unknown int, failure error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), SubmitSettleTimeout)
	defer cancel()
	left, gone := unknown, 0
	var errs []error
	for _, id := range ids {
		request := forgeapi.Request{Method: http.MethodDelete, Path: mrPath + "/draft_notes/" + strconv.FormatInt(id, 10)}
		_, err := client.JSON(ctx, request, nil)
		switch {
		case errors.Is(err, forgeapi.ErrNotFound):
			gone++
		case err != nil:
			left++
			errs = append(errs, err)
		}
	}
	if gone > 0 {
		failure = &UnconfirmedSubmitError{Err: fmt.Errorf("%w; %d draft notes were gone before they were removed", failure, gone)}
	}
	if left == 0 {
		return failure
	}
	if unknown > 0 {
		errs = append(errs, fmt.Errorf("%d draft notes have no id to delete", unknown))
	}
	return &DraftsLeftError{Err: failure, Count: left, Cleanup: errors.Join(errs...)}
}

// PendingDraftNotesError is a GitLab submit refused before any write
// because the viewer already has Count pending draft notes on the merge
// request, which its publish would post with this review.
type PendingDraftNotesError struct {
	Count int
}

func (e *PendingDraftNotesError) Error() string {
	return fmt.Sprintf("the viewer has %d pending draft notes this review did not create", e.Count)
}

// DraftsLeftError is a submit that failed after creating draft notes it
// then could not remove: Count of them may still be pending on the MR.
type DraftsLeftError struct {
	Err     error
	Count   int
	Cleanup error
}

func (e *DraftsLeftError) Error() string {
	return fmt.Sprintf("%v; %d draft notes may remain pending: %v", e.Err, e.Count, e.Cleanup)
}

func (e *DraftsLeftError) Unwrap() error { return e.Err }

func gitlabDraftNoteBody(comment ReviewLineComment, refs *PRDiffRefs, files map[string]gitlabDiffFile) (gitlabDraftNoteIn, error) {
	payload := gitlabDraftNoteIn{Note: comment.Body}
	if strings.TrimSpace(payload.Note) == "" {
		return gitlabDraftNoteIn{}, errors.New("draft note body is required")
	}
	if strings.EqualFold(comment.Side, "summary") {
		return payload, nil
	}
	if reply := comment.InReplyTo; reply != nil {
		if strings.TrimSpace(reply.ThreadID) == "" {
			return gitlabDraftNoteIn{}, errors.New("a reply names its discussion")
		}
		payload.InReplyToDiscussionID = reply.ThreadID
		return payload, nil
	}
	position, err := gitlabPositionForComment(comment, refs, files)
	if err != nil {
		return gitlabDraftNoteIn{}, err
	}
	payload.Position = position
	return payload, nil
}

// gitlabPositionForComment is comment's position on the merge request's
// diff (refs), its file and lines resolved in files (readGitLabDiffFiles).
func gitlabPositionForComment(comment ReviewLineComment, refs *PRDiffRefs, files map[string]gitlabDiffFile) (*gitlabPositionRaw, error) {
	if strings.TrimSpace(comment.Path) == "" {
		return nil, errors.New("draft note path is required")
	}
	file, ok := files[comment.Path]
	if !ok {
		return nil, fmt.Errorf("%s is not in the merge request's diff", comment.Path)
	}
	position := &gitlabPositionRaw{
		BaseSHA:  refs.BaseSHA,
		HeadSHA:  refs.HeadSHA,
		StartSHA: refs.StartSHA,
		OldPath:  file.oldPath,
		NewPath:  file.newPath,
	}
	if strings.EqualFold(comment.Side, "file") {
		position.PositionType = "file"
		return position, nil
	}
	position.PositionType = "text"
	if comment.Line == nil {
		return nil, fmt.Errorf("draft note for %s is missing a line", comment.Path)
	}
	end, err := file.point(*comment.Line, derefInt(comment.OldLine), comment.Side)
	if err != nil {
		return nil, err
	}
	position.OldLine, position.NewLine = end.OldLine, end.NewLine
	if start := comment.Start; start != nil {
		first, err := file.point(start.Line, start.OldLine, start.Side)
		if err != nil {
			return nil, err
		}
		position.LineRange = &gitlabLineRangeRaw{Start: first, End: end}
	}
	return position, nil
}

func derefInt(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}

func gitlabLineCode(path string, oldLine, newLine int) string {
	sum := sha1.Sum([]byte(path))
	return fmt.Sprintf("%x_%d_%d", sum, oldLine, newLine)
}

func (f *gitlabForge) ReplyToThread(ctx context.Context, ref PRReference, threadID string, _ int64, body string) error {
	if strings.TrimSpace(threadID) == "" {
		return errors.New("GitLab review reply requires a discussion id")
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("reply body is required")
	}
	client, err := f.writeClient(ctx, ref)
	if err != nil {
		return err
	}
	request := forgeapi.Request{Method: http.MethodPost, Path: gitlabDiscussionPath(ref, threadID) + "/notes", Body: map[string]string{"body": body}}
	_, err = client.JSON(ctx, request, nil)
	if errors.Is(err, forgeapi.ErrNotFound) {
		return &ReplyThreadGoneError{ThreadID: threadID, Err: err}
	}
	return commentWriteError(err)
}

// SetThreadResolved resolves (or reopens) one MR discussion, addressed
// under the merge request, so a discussion of another is a 404. A
// discussion with no resolvable notes answers 400, which surfaces as the
// forge's error rather than a silent no-op; a failure the forge may have
// applied is an *UnconfirmedWriteError.
func (f *gitlabForge) SetThreadResolved(ctx context.Context, ref PRReference, threadID string, resolved bool) error {
	if strings.TrimSpace(threadID) == "" {
		return errors.New("GitLab thread resolution requires a discussion id")
	}
	client, err := f.writeClient(ctx, ref)
	if err != nil {
		return err
	}
	request := forgeapi.Request{Method: http.MethodPut, Path: gitlabDiscussionPath(ref, threadID), Body: map[string]bool{"resolved": resolved}}
	_, err = client.JSON(ctx, request, nil)
	return writeOutcome(err)
}

// extractMRCreateURL pulls the WebURL from glab's `mr create` stdout.
// The non-TTY path documented in glab source emits just the WebURL,
// but defensively pick the last URL-looking line so a minor TTY/state
// banner before it doesn't break parsing. Returns "" for stdout that
// contains no URL-like content so the caller surfaces an empty-URL
// error rather than handing a banner string to the user.
func extractMRCreateURL(stdout string) string {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return ""
	}
	if !strings.Contains(trimmed, "\n") {
		if isURLLike(trimmed) {
			return trimmed
		}
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if isURLLike(line) {
			return line
		}
	}
	return ""
}

func isURLLike(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}
