package git

import (
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
)

// gitlabForge implements Forge using the glab CLI. All operations
// route through the owning Core's runBinary for timeout + size-cap
// discipline.
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
func (f *gitlabForge) CreatePR(cwd, title, body, base string, draft bool) (string, error) {
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
	result, err := f.core.runBinaryInteractive("glab", cwd, args...)
	if err != nil {
		return "", normalizeGitLabCLIError(err)
	}
	if result.exitCode != 0 {
		return "", fmt.Errorf("glab mr create failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}

	url := extractMRCreateURL(result.stdout)
	if url == "" {
		return "", errors.New("glab mr create returned empty URL")
	}
	return url, nil
}

// ListOpenPRs returns open merge requests for the given source branch.
// The implementation uses `glab api` instead of `glab mr list --output json`:
// older glab builds support `api` but do not expose JSON formatting on
// `mr list`, and the header badge needs this lookup to work across both.
func (f *gitlabForge) ListOpenPRs(cwd, head string) ([]GitPR, error) {
	sourceBranch := strings.TrimSpace(head)
	if sourceBranch == "" {
		return nil, errors.New("merge request source branch is required")
	}
	result, err := f.core.runBinary("glab", cwd, "api", gitLabOpenMRsEndpoint(sourceBranch))
	if err != nil {
		return nil, normalizeGitLabCLIError(err)
	}
	if result.exitCode != 0 {
		return nil, fmt.Errorf("glab api merge request list failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}
	stdout := strings.TrimSpace(result.stdout)
	if stdout == "" || stdout == "[]" || stdout == "null" {
		return nil, nil
	}

	// The GitLab REST API exposes web_url and iid (project-internal MR number),
	// which map onto the forge-agnostic GitPR shape. Accept webUrl as a
	// defensive compatibility alias for CLI-shaped JSON.
	var raw []struct {
		WebURL      string `json:"web_url"`
		WebURLCamel string `json:"webUrl"`
		IID         int    `json:"iid"`
		Title       string `json:"title"`
		State       string `json:"state"`
	}
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		return nil, fmt.Errorf("decode glab api merge request list output: %w", err)
	}
	pulls := make([]GitPR, 0, len(raw))
	for _, r := range raw {
		webURL := r.WebURL
		if webURL == "" {
			webURL = r.WebURLCamel
		}
		pulls = append(pulls, GitPR{
			URL:    webURL,
			Number: r.IID,
			Title:  r.Title,
			State:  NormalizePRState(r.State),
		})
	}
	return pulls, nil
}

// ListMergedPRHeads fetches recently merged MRs' source-branch heads.
// `sha` is the source branch's last commit before merge — exactly the
// pre-squash tip prune needs. GitLab caps per_page at 100, so the limit
// is honored by paging; a short page means the history is exhausted.
func (f *gitlabForge) ListMergedPRHeads(cwd string, limit int) ([]MergedPRHead, error) {
	if limit <= 0 {
		return nil, errors.New("merged MR list limit must be positive")
	}
	var heads []MergedPRHead
	for page := 1; len(heads) < limit; page++ {
		perPage := min(limit-len(heads), 100)
		endpoint := fmt.Sprintf(
			"projects/:fullpath/merge_requests?state=merged&per_page=%d&page=%d&order_by=updated_at&sort=desc",
			perPage, page,
		)
		result, err := f.core.runBinary("glab", cwd, "api", endpoint)
		if err != nil {
			return nil, normalizeGitLabCLIError(err)
		}
		if result.exitCode != 0 {
			return nil, fmt.Errorf("glab api merged MR list failed: %s", commandOutputMessage(result.stdout, result.stderr))
		}
		stdout := strings.TrimSpace(result.stdout)
		if stdout == "" || stdout == "[]" || stdout == "null" {
			break
		}

		var raw []struct {
			SourceBranch string `json:"source_branch"`
			SHA          string `json:"sha"`
			WebURL       string `json:"web_url"`
		}
		if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
			return nil, fmt.Errorf("decode glab api merged MR list output: %w", err)
		}
		for _, r := range raw {
			heads = append(heads, MergedPRHead{
				HeadRefName: r.SourceBranch,
				HeadOid:     r.SHA,
				URL:         r.WebURL,
			})
		}
		if len(raw) < perPage {
			break
		}
	}
	return heads, nil
}

func gitLabOpenMRsEndpoint(sourceBranch string) string {
	return "projects/:fullpath/merge_requests?state=opened&source_branch=" +
		url.QueryEscape(sourceBranch) +
		"&per_page=1&view=simple"
}

func gitLabMREndpoint(project string, number int) string {
	return "projects/" + url.PathEscape(project) + "/merge_requests/" + strconv.Itoa(number)
}

func gitLabApprovalsEndpoint(project string, number int) string {
	return gitLabMREndpoint(project, number) + "/approvals"
}

func gitLabDiscussionsEndpoint(project string, number, page int) string {
	return gitLabMREndpoint(project, number) + "/discussions?per_page=50&page=" + strconv.Itoa(page)
}

func gitLabDraftNotesEndpoint(project string, number int) string {
	return gitLabMREndpoint(project, number) + "/draft_notes"
}

func gitLabBulkPublishEndpoint(project string, number int) string {
	return gitLabDraftNotesEndpoint(project, number) + "/bulk_publish"
}

func gitLabApproveEndpoint(project string, number int) string {
	return gitLabMREndpoint(project, number) + "/approve"
}

func gitLabDiscussionEndpoint(project string, number int, discussionID string) string {
	return gitLabMREndpoint(project, number) + "/discussions/" + url.PathEscape(discussionID)
}

func gitLabDiscussionNotesEndpoint(project string, number int, discussionID string) string {
	return gitLabDiscussionEndpoint(project, number, discussionID) + "/notes"
}

// ViewPR fetches MR metadata via raw REST. glab 1.36.0 has no JSON
// output mode for `mr view`, but `glab api` exists across the supported
// range and returns the full MR shape.
func (f *gitlabForge) ViewPR(cwd, project string, number int) (PRMetadata, error) {
	if strings.TrimSpace(project) == "" {
		return PRMetadata{}, errors.New("project (namespace/repo) is required")
	}
	if number <= 0 {
		return PRMetadata{}, fmt.Errorf("MR number must be positive, got %d", number)
	}
	result, err := f.core.runBinary("glab", cwd, "api", gitLabMREndpoint(project, number))
	if err != nil {
		return PRMetadata{}, normalizeGitLabCLIError(err)
	}
	if result.exitCode != 0 {
		return PRMetadata{}, gitlabCommandFailure("glab api merge request view failed", result)
	}
	detail, err := parseGitLabPRDetail(result.stdout, nil)
	if err != nil {
		return PRMetadata{}, fmt.Errorf("glab mr view returned malformed JSON: %w", err)
	}
	return PRMetadata{
		Title:       detail.Title,
		Body:        detail.Body,
		HeadRefName: detail.HeadRefName,
		BaseRefName: detail.BaseRefName,
		URL:         detail.URL,
		AuthorLogin: detail.AuthorLogin,
		State:       detail.State,
	}, nil
}

func (f *gitlabForge) GetPRDetail(cwd, project string, number int) (PRDetail, error) {
	if strings.TrimSpace(project) == "" {
		return PRDetail{}, errors.New("project (namespace/repo) is required")
	}
	if number <= 0 {
		return PRDetail{}, fmt.Errorf("MR number must be positive, got %d", number)
	}
	result, err := f.core.runBinary("glab", cwd, "api", gitLabMREndpoint(project, number))
	if err != nil {
		return PRDetail{}, normalizeGitLabCLIError(err)
	}
	if result.exitCode != 0 {
		return PRDetail{}, gitlabCommandFailure("glab api merge request view failed", result)
	}
	approvals, err := f.gitlabApprovals(cwd, project, number)
	if err != nil {
		return PRDetail{}, err
	}
	detail, err := parseGitLabPRDetail(result.stdout, approvals)
	if err != nil {
		return PRDetail{}, fmt.Errorf("glab api merge request view returned malformed JSON: %w", err)
	}
	return detail, nil
}

func (f *gitlabForge) gitlabApprovals(cwd, project string, number int) ([]ReviewVerdict, error) {
	result, err := f.core.runBinary("glab", cwd, "api", gitLabApprovalsEndpoint(project, number))
	if err != nil {
		return nil, normalizeGitLabCLIError(err)
	}
	if result.exitCode != 0 {
		return nil, gitlabCommandFailure("glab api merge request approvals failed", result)
	}
	reviews, err := parseGitLabApprovals(result.stdout)
	if err != nil {
		return nil, fmt.Errorf("glab api merge request approvals returned malformed JSON: %w", err)
	}
	return reviews, nil
}

func parseGitLabPRDetail(stdout string, approvals []ReviewVerdict) (PRDetail, error) {
	var raw struct {
		IID                 int    `json:"iid"`
		Title               string `json:"title"`
		Description         string `json:"description"`
		SourceBranch        string `json:"source_branch"`
		TargetBranch        string `json:"target_branch"`
		SHA                 string `json:"sha"`
		WebURL              string `json:"web_url"`
		State               string `json:"state"`
		Draft               bool   `json:"draft"`
		WorkInProgress      bool   `json:"work_in_progress"`
		ChangesCount        string `json:"changes_count"`
		HasConflicts        bool   `json:"has_conflicts"`
		DetailedMergeStatus string `json:"detailed_merge_status"`
		Author              struct {
			Username string `json:"username"`
		} `json:"author"`
		DiffRefs struct {
			BaseSHA  string `json:"base_sha"`
			HeadSHA  string `json:"head_sha"`
			StartSHA string `json:"start_sha"`
		} `json:"diff_refs"`
		HeadPipeline *struct {
			Status string `json:"status"`
			WebURL string `json:"web_url"`
		} `json:"head_pipeline"`
	}
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		return PRDetail{}, err
	}
	headSHA := raw.DiffRefs.HeadSHA
	if headSHA == "" {
		headSHA = raw.SHA
	}
	return PRDetail{
		Number:         raw.IID,
		Title:          raw.Title,
		Body:           raw.Description,
		AuthorLogin:    raw.Author.Username,
		State:          NormalizePRState(raw.State),
		Draft:          raw.Draft || raw.WorkInProgress,
		HeadRefName:    raw.SourceBranch,
		BaseRefName:    raw.TargetBranch,
		HeadSHA:        headSHA,
		URL:            raw.WebURL,
		ChangedFiles:   parseGitLabChangesCount(raw.ChangesCount),
		ReviewDecision: gitlabReviewDecision(approvals),
		LatestReviews:  approvals,
		Checks:         gitlabCheckSummary(raw.HeadPipeline),
		Mergeability:   normalizeGitLabMergeability(raw.HasConflicts, raw.DetailedMergeStatus),
		DiffRefs: &PRDiffRefs{
			BaseSHA:  raw.DiffRefs.BaseSHA,
			HeadSHA:  raw.DiffRefs.HeadSHA,
			StartSHA: raw.DiffRefs.StartSHA,
		},
	}, nil
}

func parseGitLabChangesCount(value string) int {
	value = strings.TrimSuffix(strings.TrimSpace(value), "+")
	n, _ := strconv.Atoi(value)
	return n
}

func gitlabCheckSummary(pipeline *struct {
	Status string `json:"status"`
	WebURL string `json:"web_url"`
}) CheckSummary {
	if pipeline == nil || pipeline.Status == "" {
		return CheckSummary{}
	}
	check := CheckStatus{Kind: "Pipeline", Name: "Pipeline", Status: pipeline.Status, DetailsURL: pipeline.WebURL}
	summary := CheckSummary{Total: 1, Checks: []CheckStatus{check}}
	addCheckBucket(&summary, check)
	return summary
}

func parseGitLabApprovals(stdout string) ([]ReviewVerdict, error) {
	var raw struct {
		ApprovedBy []struct {
			ApprovedAt string `json:"approved_at"`
			User       struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"approved_by"`
	}
	if strings.TrimSpace(stdout) == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		return nil, err
	}
	out := make([]ReviewVerdict, 0, len(raw.ApprovedBy))
	for _, approval := range raw.ApprovedBy {
		if approval.User.Username == "" {
			continue
		}
		out = append(out, ReviewVerdict{
			AuthorLogin: approval.User.Username,
			State:       "APPROVED",
			SubmittedAt: approval.ApprovedAt,
		})
	}
	return out, nil
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

// Diff returns the unified diff for an MR via `glab mr diff -R <project>`.
func (f *gitlabForge) Diff(cwd, project string, number int) (string, error) {
	if strings.TrimSpace(project) == "" {
		return "", errors.New("project (namespace/repo) is required")
	}
	if number <= 0 {
		return "", fmt.Errorf("MR number must be positive, got %d", number)
	}
	result, err := f.core.runBinaryWithLimit(
		"glab",
		cwd,
		maxPRDiffBytes,
		"mr", "diff",
		strconv.Itoa(number),
		"-R", project,
	)
	if err != nil {
		return "", normalizeGitLabCLIError(err)
	}
	if result.exitCode != 0 {
		return "", fmt.Errorf("glab mr diff failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}
	return result.stdout, nil
}

func (f *gitlabForge) ListReviewThreads(cwd, project string, number int) ([]ReviewThread, error) {
	detail, err := f.GetPRDetail(cwd, project, number)
	if err != nil {
		return nil, err
	}
	headSHA := detail.HeadSHA
	var all []ReviewThread
	page := 1
	for {
		endpoint := gitLabDiscussionsEndpoint(project, number, page)
		result, err := f.core.runBinary("glab", cwd, "api", "--include", endpoint)
		if err != nil {
			return nil, normalizeGitLabCLIError(err)
		}
		if result.exitCode != 0 {
			return nil, gitlabCommandFailure("glab api merge request discussions failed", result)
		}
		headers, body := splitGitLabIncludedResponse(result.stdout)
		threads, err := parseGitLabReviewThreads(body, headSHA)
		if err != nil {
			return nil, fmt.Errorf("glab api merge request discussions returned malformed JSON: %w", err)
		}
		all = append(all, threads...)
		next := gitLabHeader(headers, "X-Next-Page")
		if next == "" {
			return all, nil
		}
		nextPage, err := strconv.Atoi(next)
		if err != nil || nextPage <= page {
			return all, nil
		}
		page = nextPage
	}
}

func parseGitLabReviewThreads(stdout, currentHeadSHA string) ([]ReviewThread, error) {
	var discussions []gitlabDiscussionRaw
	if err := json.Unmarshal([]byte(stdout), &discussions); err != nil {
		return nil, err
	}
	threads := make([]ReviewThread, 0, len(discussions))
	for _, discussion := range discussions {
		thread, ok := normalizeGitLabDiscussion(discussion, currentHeadSHA)
		if !ok {
			continue
		}
		threads = append(threads, thread)
	}
	return threads, nil
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

type gitlabAuthorRaw struct {
	Username string `json:"username"`
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

func (f *gitlabForge) SubmitReview(cwd, project string, number int, review SubmitReviewRequest) (SubmitReviewResult, error) {
	detail, err := f.GetPRDetail(cwd, project, number)
	if err != nil {
		return SubmitReviewResult{}, err
	}
	diffRefs := detail.DiffRefs
	if diffRefs == nil {
		return SubmitReviewResult{}, errors.New("GitLab review submission requires MR diff_refs")
	}
	notes := gitlabDraftNotes(review)
	endpoint := gitLabDraftNotesEndpoint(project, number)
	for _, note := range notes {
		body, err := gitlabDraftNoteBody(note, diffRefs)
		if err != nil {
			return SubmitReviewResult{}, err
		}
		result, err := f.core.runBinaryInput("glab", cwd, string(body), "api", endpoint, "-X", "POST", "-H", "Content-Type: application/json", "--input", "-")
		if err != nil {
			return SubmitReviewResult{}, normalizeGitLabCLIError(err)
		}
		if result.exitCode != 0 {
			return SubmitReviewResult{}, gitlabCommandFailure("glab api create draft note failed", result)
		}
	}
	out := SubmitReviewResult{}
	if len(notes) > 0 {
		result, err := f.core.runBinary("glab", cwd, "api", gitLabBulkPublishEndpoint(project, number), "-X", "POST")
		if err != nil {
			return SubmitReviewResult{}, normalizeGitLabCLIError(err)
		}
		if result.exitCode != 0 {
			return SubmitReviewResult{}, gitlabCommandFailure("glab api publish draft notes failed", result)
		}
		out.PostedReview = true
	}
	if strings.EqualFold(review.Verdict, ReviewVerdictApprove) {
		result, err := f.core.runBinary("glab", cwd, "api", gitLabApproveEndpoint(project, number), "-X", "POST", "-f", "sha="+detail.HeadSHA)
		if err != nil {
			return out, gitlabApproveFailure(out, normalizeGitLabCLIError(err))
		}
		if result.exitCode != 0 {
			return out, gitlabApproveFailure(out, gitlabCommandFailure("glab api approve merge request failed", result))
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

func gitlabDraftNoteBody(comment ReviewLineComment, refs *PRDiffRefs) ([]byte, error) {
	payload := struct {
		Note     string             `json:"note"`
		Position *gitlabPositionRaw `json:"position,omitempty"`
	}{
		Note: comment.Body,
	}
	if strings.TrimSpace(payload.Note) == "" {
		return nil, errors.New("draft note body is required")
	}
	if strings.EqualFold(comment.Side, "summary") {
		return json.Marshal(payload)
	}
	position, err := gitlabPositionForComment(comment, refs)
	if err != nil {
		return nil, err
	}
	payload.Position = position
	return json.Marshal(payload)
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

func (f *gitlabForge) ReplyToThread(cwd, project string, number int, threadID string, _ int64, body string) error {
	if strings.TrimSpace(threadID) == "" {
		return errors.New("GitLab review reply requires a discussion id")
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("reply body is required")
	}
	result, err := f.core.runBinary(
		"glab",
		cwd,
		"api",
		gitLabDiscussionNotesEndpoint(project, number, threadID),
		"-X", "POST",
		"-f", "body="+body,
	)
	if err != nil {
		return normalizeGitLabCLIError(err)
	}
	if result.exitCode != 0 {
		return gitlabCommandFailure("glab api reply failed", result)
	}
	return nil
}

// SetThreadResolved resolves (or reopens) one MR discussion. GitLab takes
// the new state as a query parameter on the discussion itself; a
// discussion with no resolvable notes answers 400, which surfaces as the
// CLI failure it is rather than a silent no-op.
func (f *gitlabForge) SetThreadResolved(cwd, project string, number int, threadID string, resolved bool) error {
	if strings.TrimSpace(project) == "" {
		return errors.New("project (namespace/repo) is required")
	}
	if number <= 0 {
		return fmt.Errorf("MR number must be positive, got %d", number)
	}
	if strings.TrimSpace(threadID) == "" {
		return errors.New("GitLab thread resolution requires a discussion id")
	}
	endpoint := gitLabDiscussionEndpoint(project, number, threadID) + "?resolved=" + strconv.FormatBool(resolved)
	result, err := f.core.runBinary("glab", cwd, "api", endpoint, "-X", "PUT")
	if err != nil {
		return normalizeGitLabCLIError(err)
	}
	if result.exitCode != 0 {
		return gitlabCommandFailure("glab api resolve discussion failed", result)
	}
	return nil
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

func normalizeGitLabCLIError(err error) error {
	if _, ok := errors.AsType[*exec.Error](err); ok || errors.Is(err, exec.ErrNotFound) {
		return &ForgeSetupError{
			Forge:   "gitlab",
			Binary:  "glab",
			Kind:    "missing",
			Message: "GitLab CLI (`glab`) is not installed or not on PATH. Install from https://gitlab.com/gitlab-org/cli and run 'glab auth login' to continue",
			Err:     err,
		}
	}
	return err
}

func gitlabCommandFailure(prefix string, result commandResult) error {
	message := commandOutputMessage(result.stdout, result.stderr)
	if isGitLabAuthMessage(message) {
		return &ForgeSetupError{
			Forge:   "gitlab",
			Binary:  "glab",
			Kind:    "unauthenticated",
			Message: "GitLab CLI (`glab`) is not authenticated. Run 'glab auth login' to continue",
		}
	}
	return fmt.Errorf("%s: %s", prefix, message)
}

func isGitLabAuthMessage(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "glab auth login") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "authentication required") ||
		strings.Contains(lower, "requires authentication")
}

func splitGitLabIncludedResponse(stdout string) (headers, body string) {
	if idx := strings.LastIndex(stdout, "\r\n\r\n"); idx >= 0 {
		return stdout[:idx], stdout[idx+4:]
	}
	if idx := strings.LastIndex(stdout, "\n\n"); idx >= 0 {
		return stdout[:idx], stdout[idx+2:]
	}
	return "", stdout
}

func gitLabHeader(headers, name string) string {
	name = strings.ToLower(name)
	for _, line := range strings.Split(headers, "\n") {
		line = strings.TrimSpace(line)
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.ToLower(strings.TrimSpace(key)) == name {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
