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

// ListOpenPRs returns the open merge request whose source is the given
// branch of cwd's origin project.
func (f *gitlabForge) ListOpenPRs(ctx context.Context, cwd, head string) ([]GitPR, error) {
	sourceBranch := strings.TrimSpace(head)
	if sourceBranch == "" {
		return nil, errors.New("merge request source branch is required")
	}
	client, project, err := f.gitlabOriginProject(cwd)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		WebURL string `json:"web_url"`
		IID    int    `json:"iid"`
		Title  string `json:"title"`
		State  string `json:"state"`
	}
	request := forgeapi.Request{
		Path:  project + "/merge_requests",
		Query: url.Values{"state": {"opened"}, "source_branch": {sourceBranch}, "per_page": {"1"}, "view": {"simple"}},
	}
	if _, err := client.JSON(ctx, request, &raw); err != nil {
		return nil, err
	}
	pulls := make([]GitPR, 0, len(raw))
	for _, r := range raw {
		pulls = append(pulls, GitPR{URL: r.WebURL, Number: r.IID, Title: r.Title, State: NormalizePRState(r.State)})
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
// it: the detail adds the approvals, the threads the discussions (paged),
// CI the head pipeline's jobs (paged). A part not wanted costs no request.
// prev and stepsFor are unused: the pipeline's jobs list is one read the
// previous observation cannot stand in for, and GitLab jobs have no steps.
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
	if want.Detail {
		var approvals gitlabApprovalsRaw
		if _, err := client.JSON(ctx, forgeapi.Request{Path: gitlabMRPath(ref) + "/approvals"}, &approvals); err != nil {
			return PRRead{}, err
		}
		out.Detail = gitlabPRDetail(mr, gitlabApprovalVerdicts(approvals))
	}
	if want.Threads {
		if out.Threads, err = readGitLabThreads(ctx, client, ref, mr.headSHA()); err != nil {
			return PRRead{}, err
		}
	}
	if want.CI {
		if out.CI, err = readGitLabPipeline(ctx, client, ref, mr.HeadPipeline); err != nil {
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
}

// gitlabPipelineRaw is a merge request's head pipeline.
type gitlabPipelineRaw struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	WebURL string `json:"web_url"`
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
	ApprovedBy []struct {
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
	addCheckBucket(&summary, check)
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

// readGitLabThreads reads every page of the merge request's discussions,
// normalized against the MR's current head commit.
func readGitLabThreads(ctx context.Context, client *forgeapi.Client, ref PRReference, headSHA string) ([]ReviewThread, error) {
	request := forgeapi.Request{
		Path:  gitlabMRPath(ref) + "/discussions",
		Query: url.Values{"per_page": {strconv.Itoa(gitlabPageSize)}},
	}
	var threads []ReviewThread
	page := 1
	err := client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		var discussions []gitlabDiscussionRaw
		if err := json.Unmarshal(resp.Body, &discussions); err != nil {
			return false, fmt.Errorf("GitLab merge request discussions: decode response: %w", err)
		}
		threads = append(threads, gitlabReviewThreads(discussions, headSHA)...)
		return advanceGitLabPage(resp, &page), nil
	})
	if err != nil {
		return nil, err
	}
	return threads, nil
}

func gitlabReviewThreads(discussions []gitlabDiscussionRaw, currentHeadSHA string) []ReviewThread {
	threads := make([]ReviewThread, 0, len(discussions))
	for _, discussion := range discussions {
		thread, ok := normalizeGitLabDiscussion(discussion, currentHeadSHA)
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

func normalizeGitLabDiscussion(discussion gitlabDiscussionRaw, currentHeadSHA string) (ReviewThread, bool) {
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
		comments = append(comments, ReviewComment{
			AuthorLogin: note.Author.Username,
			AuthorName:  note.Author.Name,
			Body:        note.Body,
			CreatedAt:   note.CreatedAt,
			DatabaseID:  note.ID,
		})
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
	for _, note := range notes {
		body, err := gitlabDraftNoteBody(note, diffRefs)
		if err != nil {
			return SubmitReviewResult{}, err
		}
		if _, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: mrPath + "/draft_notes", Body: body}, nil); err != nil {
			return SubmitReviewResult{}, err
		}
	}
	out := SubmitReviewResult{}
	if len(notes) > 0 {
		if _, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: mrPath + "/draft_notes/bulk_publish"}, nil); err != nil {
			return SubmitReviewResult{}, err
		}
		out.PostedReview = true
	}
	if strings.EqualFold(review.Verdict, ReviewVerdictApprove) {
		approve := forgeapi.Request{Method: http.MethodPost, Path: mrPath + "/approve", Body: map[string]string{"sha": mr.headSHA()}}
		if _, err := client.JSON(ctx, approve, nil); err != nil {
			return out, gitlabApproveFailure(out, err)
		}
		out.PostedReview = true
	}
	return out, nil
}

// gitlabApproveFailure wraps an approve error after notes were already
// published. A plain error would read as "nothing posted" and make the
// caller keep (and later double-post) every comment; the typed partial
// error carries the posted state instead.
func gitlabApproveFailure(out SubmitReviewResult, err error) error {
	if !out.PostedReview {
		return err
	}
	return &PartialSubmitError{PostedReview: true, PostedFileComments: out.PostedFileComments, Err: err}
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

// gitlabDraftNoteIn is the REST body of one draft note.
type gitlabDraftNoteIn struct {
	Note     string             `json:"note"`
	Position *gitlabPositionRaw `json:"position,omitempty"`
}

func gitlabDraftNoteBody(comment ReviewLineComment, refs *PRDiffRefs) (gitlabDraftNoteIn, error) {
	payload := gitlabDraftNoteIn{Note: comment.Body}
	if strings.TrimSpace(payload.Note) == "" {
		return gitlabDraftNoteIn{}, errors.New("draft note body is required")
	}
	if strings.EqualFold(comment.Side, "summary") {
		return payload, nil
	}
	position, err := gitlabPositionForComment(comment, refs)
	if err != nil {
		return gitlabDraftNoteIn{}, err
	}
	payload.Position = position
	return payload, nil
}

func gitlabPositionForComment(comment ReviewLineComment, refs *PRDiffRefs) (*gitlabPositionRaw, error) {
	if strings.TrimSpace(comment.Path) == "" {
		return nil, errors.New("draft note path is required")
	}
	position := &gitlabPositionRaw{
		BaseSHA:  refs.BaseSHA,
		HeadSHA:  refs.HeadSHA,
		StartSHA: refs.StartSHA,
		OldPath:  comment.Path,
		NewPath:  comment.Path,
	}
	if strings.EqualFold(comment.Side, "file") {
		position.PositionType = "file"
		return position, nil
	}
	position.PositionType = "text"
	if comment.Line == nil {
		return nil, fmt.Errorf("draft note for %s is missing a line", comment.Path)
	}
	side := strings.ToLower(strings.TrimSpace(comment.Side))
	switch side {
	case "left", "old":
		position.OldLine = comment.Line
	case "right", "new", "":
		position.NewLine = comment.Line
	default:
		return nil, fmt.Errorf("draft note for %s has invalid side %q", comment.Path, comment.Side)
	}
	if comment.StartLine != nil {
		position.LineRange = gitlabLineRange(comment.Path, side, *comment.StartLine, *comment.Line)
	}
	return position, nil
}

func gitlabLineRange(path, side string, start, end int) *gitlabLineRangeRaw {
	if side == "left" || side == "old" {
		return &gitlabLineRangeRaw{
			Start: gitlabLineRangePoint{LineCode: gitlabLineCode(path, start, 0), Type: "old", OldLine: &start},
			End:   gitlabLineRangePoint{LineCode: gitlabLineCode(path, end, 0), Type: "old", OldLine: &end},
		}
	}
	return &gitlabLineRangeRaw{
		Start: gitlabLineRangePoint{LineCode: gitlabLineCode(path, 0, start), Type: "new", NewLine: &start},
		End:   gitlabLineRangePoint{LineCode: gitlabLineCode(path, 0, end), Type: "new", NewLine: &end},
	}
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
	return err
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
