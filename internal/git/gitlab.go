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
	"time"

	"agent-overflow/internal/forgeapi"
)

// gitlabForge implements Forge over the GitLab REST API through the
// Core's forge API transport. Only CreatePR runs glab.
type gitlabForge struct {
	core *Core
	// projects caches each login's merge settings of a project and
	// viewers each login's account on a host (gitlab_merge_read.go).
	projects gitlabCache[gitlabProjectSettings]
	viewers  gitlabCache[gitlabViewer]
	// noApprovalState marks a host whose GraphQL has no
	// MergeRequest.approvalState (GitLab CE), for gitlabCacheTTL
	// (pr_list_gitlab.go).
	noApprovalState gitlabCache[bool]
	// activity is each host's viewer activity, read once per list round
	// (pr_list_gitlab.go).
	activity gitlabActivityRounds
	// routes remembers, per host, a server that answered the older cancel
	// route (gitlab_merge.go).
	routes gitlabRouteMemory
	// pipelineTags remembers which listed pipelines ran on a tag
	// (ci_gitlab.go).
	pipelineTags gitlabPipelineTags
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

// gitlabOriginProject is the GitLab project cwd's origin names, as an
// API path, and the API client of its host.
func (f *gitlabForge) gitlabOriginProject(cwd string) (*forgeapi.Client, string, error) {
	forge, host, project, err := f.core.originCoordinates(cwd)
	if err != nil {
		return nil, "", err
	}
	if forge != "gitlab" {
		return nil, "", &OriginUnknownError{Cwd: cwd, Reason: "the origin is not a GitLab repository"}
	}
	client, err := f.core.gitlabAPI(host)
	if err != nil {
		return nil, "", err
	}
	return client, gitlabProjectPath(project), nil
}

// gitlabOpenMRsMaxPages bounds the pages one open merge request lookup
// reads past forks' merge requests from a branch of the same name.
const gitlabOpenMRsMaxPages = 5

// ListOpenPRs returns the open merge request whose source is the given
// branch of cwd's origin project itself. The source_branch filter also
// matches a fork's branch of the same name, which is someone else's work:
// a merge request counts only when its source project is the target
// project. GitLab keeps one open merge request per source branch, so the
// read stops at it. The full view is read because the simple one carries
// no source_project_id.
func (f *gitlabForge) ListOpenPRs(ctx context.Context, cwd, head string) ([]GitPR, error) {
	sourceBranch := strings.TrimSpace(head)
	if sourceBranch == "" {
		return nil, errors.New("merge request source branch is required")
	}
	client, project, err := f.gitlabOriginProject(cwd)
	if err != nil {
		return nil, err
	}
	request := forgeapi.Request{
		Path:         project + "/merge_requests",
		Query:        url.Values{"state": {"opened"}, "source_branch": {sourceBranch}, "per_page": {strconv.Itoa(gitlabPageSize)}},
		MaxBodyBytes: gitlabMRListMaxBody,
	}
	pulls := []GitPR{}
	page, pages := 1, 0
	err = client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		var raw []struct {
			WebURL          string `json:"web_url"`
			IID             int    `json:"iid"`
			Title           string `json:"title"`
			State           string `json:"state"`
			ProjectID       int64  `json:"project_id"`
			SourceProjectID int64  `json:"source_project_id"`
			TargetBranch    string `json:"target_branch"`
		}
		if err := json.Unmarshal(resp.Body, &raw); err != nil {
			return false, fmt.Errorf("GitLab open merge requests: decode response: %w", err)
		}
		for _, r := range raw {
			if r.SourceProjectID != r.ProjectID {
				continue
			}
			pulls = append(pulls, GitPR{URL: r.WebURL, Number: r.IID, Title: r.Title, State: NormalizePRState(r.State), BaseBranch: r.TargetBranch})
		}
		pages++
		return len(pulls) == 0 && pages < gitlabOpenMRsMaxPages && len(raw) == gitlabPageSize && advanceGitLabPage(resp, &page), nil
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
// heads, most recently updated first. `sha` is the source branch's last
// commit before merge, exactly the pre-squash tip prune needs.
func (f *gitlabForge) ListMergedPRHeads(ctx context.Context, cwd string, limit int) ([]MergedPRHead, error) {
	if limit <= 0 {
		return nil, errors.New("merged MR list limit must be positive")
	}
	client, project, err := f.gitlabOriginProject(cwd)
	if err != nil {
		return nil, err
	}
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
	page := 1
	err = client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		var raw []struct {
			SourceBranch string `json:"source_branch"`
			SHA          string `json:"sha"`
			WebURL       string `json:"web_url"`
		}
		if err := json.Unmarshal(resp.Body, &raw); err != nil {
			return false, fmt.Errorf("GitLab merged merge requests: decode response: %w", err)
		}
		for _, r := range raw {
			if len(heads) == limit {
				break
			}
			heads = append(heads, MergedPRHead{HeadRefName: r.SourceBranch, HeadOid: r.SHA, URL: r.WebURL})
		}
		return len(heads) < limit && len(raw) == perPage && advanceGitLabPage(resp, &page), nil
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
// it: the detail adds the approvals, the project's merge settings (read
// once per login per project per gitlabCacheTTL) and the viewer's login
// (once per login per host per gitlabCacheTTL), the threads and the
// events the discussions (paged; the system notes are events, the rest
// threads), CI the head pipeline's rows (paged, readGitLabPipeline). A
// part not wanted costs no request. prev and stepsFor are unused: the
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
	var threads []ReviewThread
	if want.Threads || want.Events {
		var system []gitlabNoteRaw
		threads, system, err = readGitLabDiscussions(ctx, client, ref, gitlabThreadContext{headSHA: mr.headSHA(), webURL: mr.WebURL})
		if err != nil {
			return PRRead{}, err
		}
		notes = system
		if want.Threads {
			out.Threads = threads
		}
	}
	// The detail's armed auto-merge takes its time and wording from the
	// latest auto-merge event when the same read has the notes.
	events := gitlabEvents(mr, notes)
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
		viewer := f.viewerLogin(ctx, client, ref.Host)
		out.Detail = gitlabPRDetail(mr, gitlabApprovalVerdicts(approvals))
		gitlabFillMerge(&out.Detail, mr, approvals, settings, events, viewer)
	}
	if want.Threads {
		// Who may edit and delete a note is its author and, for every note,
		// a maintainer or owner (GitLab's notes carry no permission). The
		// login and the role are the detail's cached reads, so a tick that
		// reads both costs no request of its own for them.
		settings, err := f.projectSettings(ctx, client, ref.Repository())
		if err != nil {
			return PRRead{}, err
		}
		markGitLabViewerNotes(out.Threads, f.viewerLogin(ctx, client, ref.Host), settings.accessLevel())
	}
	if want.CI {
		if out.CI, err = readGitLabPipeline(ctx, client, ref.Repository(), mr.HeadPipeline); err != nil {
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
	User                        struct {
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
	ApprovedBy        []struct {
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

// readGitLabDiscussions reads every page of the merge request's
// discussions: the human notes as threads normalized against the MR's
// current head commit, and the system notes in GitLab's order, which are
// the events' source (gitlabEvents).
func readGitLabDiscussions(ctx context.Context, client *forgeapi.Client, ref PRReference, at gitlabThreadContext) ([]ReviewThread, []gitlabNoteRaw, error) {
	request := forgeapi.Request{
		Path:         gitlabMRPath(ref) + "/discussions",
		Query:        url.Values{"per_page": {strconv.Itoa(gitlabPageSize)}},
		MaxBodyBytes: gitlabDiscussionsMaxBody,
	}
	var threads []ReviewThread
	var system []gitlabNoteRaw
	page := 1
	err := client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
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
		return advanceGitLabPage(resp, &page), nil
	})
	if err != nil {
		return nil, nil, err
	}
	return threads, system, nil
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
	path, line, startLine, side := normalizeGitLabPosition(root.Position)
	return ReviewThread{
		ID:           discussion.ID,
		Path:         path,
		Line:         line,
		StartLine:    startLine,
		Side:         side,
		IsResolvable: true,
		IsResolved:   resolved,
		IsOutdated:   currentHeadSHA != "" && root.Position.HeadSHA != "" && root.Position.HeadSHA != currentHeadSHA,
		Comments:     comments,
	}, true
}

func normalizeGitLabPosition(position *gitlabPositionRaw) (string, *int, *int, string) {
	path := position.NewPath
	if path == "" {
		path = position.OldPath
	}
	if strings.EqualFold(position.PositionType, "file") {
		return path, nil, nil, "file"
	}
	side := "right"
	line := position.NewLine
	if position.NewLine == nil && position.OldLine != nil {
		side = "left"
		line = position.OldLine
	}
	var startLine *int
	if position.LineRange != nil {
		if side == "left" {
			startLine = position.LineRange.Start.OldLine
		} else {
			startLine = position.LineRange.Start.NewLine
		}
	}
	return path, line, startLine, side
}

func (f *gitlabForge) SubmitReview(ctx context.Context, ref PRReference, review SubmitReviewRequest) (SubmitReviewResult, error) {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return SubmitReviewResult{}, err
	}
	// The draft notes are positioned on the MR's current diff, and an
	// approval names the head it approves; the MR alone carries both.
	mr, err := readGitLabMR(ctx, client, ref)
	if err != nil {
		return SubmitReviewResult{}, err
	}
	diffRefs := mr.diffRefs()
	notes := gitlabDraftNotes(review)
	mrPath := gitlabMRPath(ref)
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
		if before, err = readGitLabReplyThreads(ctx, client, ref, mr); err != nil {
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
		if files, err = readGitLabDiffFiles(ctx, client, ref, paths); err != nil {
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
		if _, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: mrPath + "/draft_notes", Body: body}, &draft); err != nil {
			if body.InReplyToDiscussionID != "" && gitlabReplyRefused(ctx, client, ref, body.InReplyToDiscussionID, err) {
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
	if len(notes) > 0 {
		if _, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: mrPath + "/draft_notes/bulk_publish"}, nil); err != nil {
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
	}
	// A published reply whose discussion went meanwhile was dropped:
	// GitLab says nothing, so the discussions say.
	var replyFailure *PartialSubmitError
	if len(replies) > 0 {
		replyFailure = f.confirmGitLabReplies(ctx, client, ref, mr, before, replies, &out)
	}
	if strings.EqualFold(review.Verdict, ReviewVerdictApprove) {
		approve := forgeapi.Request{Method: http.MethodPost, Path: mrPath + "/approve", Body: map[string]string{"sha": mr.headSHA()}}
		if _, err := client.JSON(ctx, approve, nil); err != nil {
			failure := gitlabApproveFailure(out, err)
			if partial, ok := failure.(*PartialSubmitError); ok && replyFailure != nil {
				partial.UnpostedReplies = replyFailure.UnpostedReplies
			}
			return out, failure
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
		return &PartialSubmitError{PostedReview: out.PostedReview, FailedThreadID: replies[0].comment.InReplyTo.ThreadID, UnpostedReplies: all, Unconfirmed: true,
			Err: fmt.Errorf("read the discussions to confirm the replies: %w", err)}
	}
	out.PostedReplies = len(replies) - len(lost)
	if len(lost) == 0 {
		return nil
	}
	threadID := replies[lost[0]].comment.InReplyTo.ThreadID
	return &PartialSubmitError{PostedReview: out.PostedReview, PostedReplies: out.PostedReplies, FailedThreadID: threadID, UnpostedReplies: lost,
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
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitlabDraftCleanupTimeout)
	defer cancel()
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

// gitlabApproveFailure wraps an approve error after notes were already
// published. A plain error would read as "nothing posted" and make the
// caller keep (and later double-post) every comment; the typed partial
// error carries the posted state instead. An approve alone is the
// submit's only write, so its error decides as any write's does.
func gitlabApproveFailure(out SubmitReviewResult, err error) error {
	if !out.PostedReview {
		return submitWriteError(err)
	}
	return &PartialSubmitError{PostedReview: true, PostedFileComments: out.PostedFileComments, Unconfirmed: !forgeapi.WriteNotApplied(err), Err: err}
}

func gitlabDraftNotes(review SubmitReviewRequest) []ReviewLineComment {
	notes := make([]ReviewLineComment, 0, len(review.Comments)+1)
	body := review.Body
	if strings.EqualFold(review.Verdict, ReviewVerdictRequestChanges) {
		if strings.TrimSpace(body) == "" {
			body = "Changes requested."
		} else {
			body = "Changes requested:\n\n" + body
		}
	}
	if strings.TrimSpace(body) != "" {
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

// gitlabDraftCleanupTimeout bounds removing a failed submit's drafts.
const gitlabDraftCleanupTimeout = 30 * time.Second

// removeGitLabDrafts deletes the drafts with ids, the ones a failed submit
// created, and no other, then returns failure. unknown counts drafts the
// submit created without learning their ids. A draft it could not remove
// is reported as a *DraftsLeftError around failure. A draft already gone
// (404) was not removed by this submit: another publish of the viewer's
// drafts may have posted it, so failure becomes an UnconfirmedSubmitError.
func removeGitLabDrafts(ctx context.Context, client *forgeapi.Client, mrPath string, ids []int64, unknown int, failure error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitlabDraftCleanupTimeout)
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
	client, err := f.core.gitlabAPI(ref.Host)
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

// SetThreadResolved resolves (or reopens) one MR discussion. A discussion
// with no resolvable notes answers 400, which surfaces as the forge's
// error rather than a silent no-op.
func (f *gitlabForge) SetThreadResolved(ctx context.Context, ref PRReference, threadID string, resolved bool) error {
	if strings.TrimSpace(threadID) == "" {
		return errors.New("GitLab thread resolution requires a discussion id")
	}
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	request := forgeapi.Request{Method: http.MethodPut, Path: gitlabDiscussionPath(ref, threadID), Body: map[string]bool{"resolved": resolved}}
	_, err = client.JSON(ctx, request, nil)
	return err
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
