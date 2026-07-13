package git

import (
	"errors"
	"fmt"
	"strings"
)

// Forge wraps the host-specific operations against a code-hosting CLI
// (gh for GitHub, glab for GitLab). All operations route through the
// owning Core's runBinary so timeouts, size caps, and subprocess
// discipline stay consistent across forges — the Forge implementation
// must not call exec.Command directly.
type Forge interface {
	// ID returns "github" or "gitlab" — the canonical short id for this forge.
	ID() string
	// BinaryName returns the OS binary the forge shells out to (e.g. "gh").
	// Used for "<binary> is not installed" messaging.
	BinaryName() string

	// ListOpenPRs returns open PRs/MRs for the given head/source branch.
	ListOpenPRs(cwd, head string) ([]GitPR, error)
	// CreatePR opens a PR/MR for the current branch in cwd. Returns the URL.
	CreatePR(cwd, title, body, base string, draft bool) (string, error)
	// ViewPR fetches metadata for a PR/MR identified by project + number.
	// project is "owner/repo" (GitHub) or "namespace/.../repo" (GitLab).
	// cwd may be empty when there is no local clone — gh --repo and
	// glab -R both query authenticated state without needing one.
	ViewPR(cwd, project string, number int) (PRMetadata, error)
	// Diff returns the unified-patch diff for the given PR/MR.
	Diff(cwd, project string, number int) (string, error)
	// GetPRDetail fetches the review-pane detail shape for a PR/MR.
	GetPRDetail(cwd, project string, number int) (PRDetail, error)
	// ListReviewThreads fetches normalized inline review threads.
	ListReviewThreads(cwd, project string, number int) ([]ReviewThread, error)
	// SubmitReview publishes a PR/MR review verdict plus draft comments.
	SubmitReview(cwd, project string, number int, review SubmitReviewRequest) (SubmitReviewResult, error)
	// ReplyToThread posts an immediate reply to an existing review thread.
	ReplyToThread(cwd, project string, number int, threadID string, databaseID int64, body string) error
	// ListPRCIJobs fetches the PR/MR head pipeline grouped into stages
	// (GitLab stages, GitHub workflows) with per-job status.
	ListPRCIJobs(cwd, project string, number int) (CIPipeline, error)
	// GetCIJobLog fetches the raw log/trace for one CI job.
	GetCIJobLog(cwd, project, jobID string) (string, error)
}

// PRMetadata is the forge-agnostic view of a PR/MR fetched via ViewPR.
type PRMetadata struct {
	Title       string
	Body        string
	HeadRefName string
	BaseRefName string
	URL         string
	AuthorLogin string
	State       string
	Files       []PRFile
}

// PRFile describes one file's per-PR change stats.
type PRFile struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

const (
	ReviewVerdictApprove        = "approve"
	ReviewVerdictRequestChanges = "request-changes"
	ReviewVerdictComment        = "comment"

	MergeabilityConflicts = "conflicts"
	MergeabilityClean     = "clean"
	MergeabilityChecking  = "checking"
)

// PRDetail is the normalized PR/MR detail shape consumed by the review pane.
type PRDetail struct {
	Number         int             `json:"number"`
	Title          string          `json:"title"`
	Body           string          `json:"body"`
	AuthorLogin    string          `json:"authorLogin"`
	State          string          `json:"state"`
	Draft          bool            `json:"draft"`
	HeadRefName    string          `json:"headRefName"`
	BaseRefName    string          `json:"baseRefName"`
	HeadSHA        string          `json:"headSHA"`
	URL            string          `json:"url"`
	Additions      int             `json:"additions"`
	Deletions      int             `json:"deletions"`
	ChangedFiles   int             `json:"changedFiles"`
	ViewerIsAuthor bool            `json:"viewerIsAuthor"`
	ReviewDecision string          `json:"reviewDecision"`
	LatestReviews  []ReviewVerdict `json:"latestReviews"`
	Checks         CheckSummary    `json:"checks"`
	Mergeability   string          `json:"mergeability"`
	DiffRefs       *PRDiffRefs     `json:"diffRefs,omitempty"`
}

type PRDiffRefs struct {
	BaseSHA  string `json:"baseSHA"`
	HeadSHA  string `json:"headSHA"`
	StartSHA string `json:"startSHA"`
}

type ReviewVerdict struct {
	AuthorLogin string `json:"authorLogin"`
	State       string `json:"state"`
	SubmittedAt string `json:"submittedAt"`
	Body        string `json:"body"`
	CommitSHA   string `json:"commitSHA"`
}

type CheckSummary struct {
	Total    int           `json:"total"`
	Success  int           `json:"success"`
	Pending  int           `json:"pending"`
	Failure  int           `json:"failure"`
	Skipped  int           `json:"skipped"`
	Canceled int           `json:"canceled"`
	Checks   []CheckStatus `json:"checks"`
}

type CheckStatus struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Workflow    string `json:"workflow,omitempty"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion,omitempty"`
	DetailsURL  string `json:"detailsURL,omitempty"`
	StartedAt   string `json:"startedAt,omitempty"`
	CompletedAt string `json:"completedAt,omitempty"`
}

// ReviewThread is one PR discussion: a file-anchored review thread
// (Path set) or a PR-level conversation thread (Path empty — GitLab
// position-less discussions, GitHub PR conversation comments).
type ReviewThread struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Line      *int   `json:"line"`
	StartLine *int   `json:"startLine"`
	Side      string `json:"side"`
	// IsResolvable distinguishes threads with a real resolve state from
	// flat comments (GitHub conversation comments, non-resolvable GitLab
	// notes) where IsResolved=false would misread as "needs attention".
	IsResolvable bool            `json:"isResolvable"`
	IsResolved   bool            `json:"isResolved"`
	IsOutdated   bool            `json:"isOutdated"`
	Comments     []ReviewComment `json:"comments"`
}

type ReviewComment struct {
	AuthorLogin string         `json:"authorLogin"`
	Body        string         `json:"body"`
	CreatedAt   string         `json:"createdAt"`
	DatabaseID  int64          `json:"databaseID"`
	ReplyTo     *ReviewReplyTo `json:"replyTo,omitempty"`
}

type ReviewReplyTo struct {
	ID         string `json:"id"`
	DatabaseID int64  `json:"databaseID"`
}

type SubmitReviewRequest struct {
	Verdict  string              `json:"verdict"`
	Body     string              `json:"body"`
	Comments []ReviewLineComment `json:"comments"`
}

type ReviewLineComment struct {
	Path      string `json:"path"`
	Body      string `json:"body"`
	Line      *int   `json:"line,omitempty"`
	Side      string `json:"side"`
	StartLine *int   `json:"startLine,omitempty"`
}

type SubmitReviewResult struct {
	PostedReview       bool `json:"postedReview"`
	PostedFileComments int  `json:"postedFileComments"`
}

// PartialSubmitError reports that the primary review landed but a later
// provider-specific follow-up call failed.
type PartialSubmitError struct {
	PostedReview       bool
	PostedFileComments int
	FailedPath         string
	Err                error
}

func (e *PartialSubmitError) Error() string {
	if e == nil {
		return ""
	}
	if e.FailedPath != "" {
		return fmt.Sprintf("review submitted, but posting file-level comment for %s failed: %v", e.FailedPath, e.Err)
	}
	return fmt.Sprintf("review submitted, but a follow-up step failed: %v", e.Err)
}

func (e *PartialSubmitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ForgeSetupError is a typed, user-facing setup problem for a forge CLI.
type ForgeSetupError struct {
	Forge   string
	Binary  string
	Kind    string
	Message string
	Err     error
}

func (e *ForgeSetupError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *ForgeSetupError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// PRReference identifies a PR/MR by host, namespace, repo, and number.
// Namespace carries the full path-segment chain before the repo
// (a single "owner" for GitHub, possibly a "group/sub/sub" chain for
// GitLab subgroups).
type PRReference struct {
	Forge     string // "github" | "gitlab"
	Namespace string // "owner" or "group/sub/..."
	Repo      string
	Number    int
}

// Project returns "namespace/repo" suitable for passing as gh --repo or
// glab -R.
func (r PRReference) Project() string {
	if r.Namespace == "" {
		return r.Repo
	}
	return r.Namespace + "/" + r.Repo
}

// ErrUnsupportedForge is returned by every nullForge operation. Callers
// should surface it to the user as "this remote isn't a supported
// forge" rather than dispatching to a binary we don't have.
var ErrUnsupportedForge = errors.New("forge integration is not available for this remote")

// nullForge is the sentinel returned by Core.forgeFor when origin URL
// classification yields an unsupported host. Every operation returns
// ErrUnsupportedForge so callers can branch-free dispatch.
type nullForge struct{}

func (nullForge) ID() string         { return "" }
func (nullForge) BinaryName() string { return "" }

func (nullForge) ListOpenPRs(string, string) ([]GitPR, error) {
	return nil, ErrUnsupportedForge
}

func (nullForge) CreatePR(string, string, string, string, bool) (string, error) {
	return "", ErrUnsupportedForge
}

func (nullForge) ViewPR(string, string, int) (PRMetadata, error) {
	return PRMetadata{}, ErrUnsupportedForge
}

func (nullForge) Diff(string, string, int) (string, error) {
	return "", ErrUnsupportedForge
}

func (nullForge) GetPRDetail(string, string, int) (PRDetail, error) {
	return PRDetail{}, ErrUnsupportedForge
}

func (nullForge) ListReviewThreads(string, string, int) ([]ReviewThread, error) {
	return nil, ErrUnsupportedForge
}

func (nullForge) SubmitReview(string, string, int, SubmitReviewRequest) (SubmitReviewResult, error) {
	return SubmitReviewResult{}, ErrUnsupportedForge
}

func (nullForge) ReplyToThread(string, string, int, string, int64, string) error {
	return ErrUnsupportedForge
}

func (nullForge) ListPRCIJobs(string, string, int) (CIPipeline, error) {
	return CIPipeline{}, ErrUnsupportedForge
}

func (nullForge) GetCIJobLog(string, string, string) (string, error) {
	return "", ErrUnsupportedForge
}

// PRAnchorScheme is the URI scheme used for the project-row anchor we
// generate when a PR/MR thread has no local clone matching its repo.
// The anchor is opaque — it is stored as Project.Path and used as a
// uniqueness key, never re-parsed. Use BuildPRAnchor to construct one.
const PRAnchorScheme = "pr://"

// BuildPRAnchor constructs a "pr://forge/namespace/repo" pseudo-URI
// for the project-row of a PR/MR thread that has no matching local
// clone. The forge prefix makes the anchor self-describing without
// requiring callers to re-classify the namespace later.
func BuildPRAnchor(forge, namespace, repo string) string {
	return fmt.Sprintf("%s%s/%s/%s", PRAnchorScheme, forge, namespace, repo)
}

// SplitProjectForForge separates "namespace/repo" with per-forge
// segment rules: github requires exactly two segments (owner/repo),
// gitlab accepts any N≥2 segments where everything before the last is
// the namespace (group/sub/.../repo).
//
// Each segment is also validated against safe-name rules — no leading
// dashes (would be misread as flags by shell-out targets), no `.` or
// `..` (path traversal in the pseudo-anchor), no control characters
// or whitespace. The CLI argv path itself is shell-safe (we never
// interpolate via a shell), but defense-in-depth keeps the values
// out of DB rows and logs in pathological shapes.
func SplitProjectForForge(forgeID, project string) (namespace, repo string, err error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return "", "", errors.New("project is required")
	}
	parts := strings.Split(project, "/")
	for _, p := range parts {
		if err := ValidateProjectSegment(p); err != nil {
			return "", "", fmt.Errorf("project %q: %w", project, err)
		}
	}

	switch forgeID {
	case "github":
		if len(parts) != 2 {
			return "", "", fmt.Errorf("github project must be in the form OWNER/REPO, got %q", project)
		}
		return parts[0], parts[1], nil
	case "gitlab":
		if len(parts) < 2 {
			return "", "", fmt.Errorf("gitlab project must be NAMESPACE/REPO (or longer for subgroups), got %q", project)
		}
		return strings.Join(parts[:len(parts)-1], "/"), parts[len(parts)-1], nil
	default:
		return "", "", fmt.Errorf("unsupported forge %q", forgeID)
	}
}

// ValidateProjectSegment enforces a conservative char class on a
// single namespace/repo path segment. Rejects empty, `.`/`..`, leading
// dash, whitespace, and control characters. The accepted set covers
// all real github / gitlab owner / namespace / repo names.
func ValidateProjectSegment(seg string) error {
	if seg == "" {
		return errors.New("segment is empty")
	}
	if seg == "." || seg == ".." {
		return fmt.Errorf("segment %q is not allowed", seg)
	}
	if seg[0] == '-' {
		return fmt.Errorf("segment %q must not start with '-'", seg)
	}
	for _, r := range seg {
		if r <= 0x20 || r == 0x7f {
			return fmt.Errorf("segment %q contains a control or whitespace character", seg)
		}
	}
	return nil
}

// NormalizePRState maps a forge-native PR/MR state to a canonical
// lowercase vocabulary: "open", "closed", "merged", "locked", or "".
// Both gh ("OPEN") and glab ("opened") map onto "open"; the rest
// already align after lowercasing.
func NormalizePRState(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "open", "opened":
		return "open"
	case "closed":
		return "closed"
	case "merged":
		return "merged"
	case "locked":
		return "locked"
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

func (c *Core) GetPRDetail(cwd string, ref PRReference) (PRDetail, error) {
	return c.ForgeByID(ref.Forge).GetPRDetail(cwd, ref.Project(), ref.Number)
}

func (c *Core) GetPRDiff(cwd string, ref PRReference) (string, error) {
	return c.ForgeByID(ref.Forge).Diff(cwd, ref.Project(), ref.Number)
}

func (c *Core) ListReviewThreads(cwd string, ref PRReference) ([]ReviewThread, error) {
	return c.ForgeByID(ref.Forge).ListReviewThreads(cwd, ref.Project(), ref.Number)
}

func (c *Core) SubmitReview(cwd string, ref PRReference, review SubmitReviewRequest) (SubmitReviewResult, error) {
	return c.ForgeByID(ref.Forge).SubmitReview(cwd, ref.Project(), ref.Number, review)
}

func (c *Core) ReplyToThread(cwd string, ref PRReference, threadID string, databaseID int64, body string) error {
	return c.ForgeByID(ref.Forge).ReplyToThread(cwd, ref.Project(), ref.Number, threadID, databaseID, body)
}

func (c *Core) ListPRCIJobs(cwd string, ref PRReference) (CIPipeline, error) {
	return c.ForgeByID(ref.Forge).ListPRCIJobs(cwd, ref.Project(), ref.Number)
}

func (c *Core) GetCIJobLog(cwd string, ref PRReference, jobID string) (string, error) {
	return c.ForgeByID(ref.Forge).GetCIJobLog(cwd, ref.Project(), jobID)
}
