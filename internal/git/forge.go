package git

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/forgeattach"
)

// Forge wraps the host-specific operations against a code-hosting forge.
// Requests go through the owning Core's forge API transport (see
// docs/architecture/forge-transport.md); the operations that still run a
// CLI (`gh pr create`, `glab mr create`) route through the Core's runSpec
// so timeouts and subprocess discipline stay consistent. A Forge
// implementation must not call exec.Command directly.
//
// Every operation takes the context of its owner: a bound method's call,
// or the PR pump's lifetime. Cancelling it ends the request. A context
// marked with forgeapi.WithInteractive is a user's own action.
//
// Repository-scoped operations (ListOpenPRs, ListMergedPRHeads, CreatePR)
// resolve the repository from cwd. PR-scoped operations take the
// PRReference, which carries the forge host, and no cwd.
type Forge interface {
	// ID returns "github" or "gitlab", the canonical short id for this forge.
	ID() string
	// BinaryName returns the forge's CLI (e.g. "gh"), whose login the
	// transport borrows. Used for "<binary> is not installed" messaging.
	BinaryName() string

	// ListOpenPRs returns open PRs/MRs for the given head/source branch.
	ListOpenPRs(ctx context.Context, cwd, head string) ([]GitPR, error)
	// ListMergedPRHeads returns the head branch name + head SHA of up to
	// `limit` recently merged PRs/MRs. One bulk read backing the prune
	// preview's squash-merge detection: a gone local branch whose tip
	// matches a merged PR head was fully pushed before the merge.
	ListMergedPRHeads(ctx context.Context, cwd string, limit int) ([]MergedPRHead, error)
	// CreatePR opens a PR/MR for the current branch in cwd. Returns the URL.
	CreatePR(ctx context.Context, cwd, title, body, base string, draft bool) (string, error)
	// ReadPR reads the parts of a PR/MR want names, the PR pump's one
	// read per tick (docs/architecture/forge-transport.md#reads-per-tick).
	// Detail is the review-pane detail shape, Threads the normalized
	// review threads and conversation comments, CI the head pipeline
	// grouped into stages (GitLab stages, GitHub workflows) with per-job
	// status. For CI, prev is the pipeline the caller last observed, or
	// nil: a forge may serve the parts of it the forge reports unchanged
	// instead of refetching them; stepsFor names the jobs whose steps the
	// caller shows, and a forge with steps fills them on those jobs only.
	// A part not wanted is left zero.
	ReadPR(ctx context.Context, ref PRReference, want PRReadParts, prev *CIPipeline, stepsFor []string) (PRRead, error)
	// SubmitReview publishes a PR/MR review verdict plus draft comments.
	SubmitReview(ctx context.Context, ref PRReference, review SubmitReviewRequest) (SubmitReviewResult, error)
	// ReplyToThread posts an immediate reply to an existing review thread.
	ReplyToThread(ctx context.Context, ref PRReference, threadID string, databaseID int64, body string) error
	// SetThreadResolved marks one review thread resolved (or reopens it).
	// threadID is the same id ReadPR reported: a GitHub review thread node
	// id, a GitLab discussion id.
	SetThreadResolved(ctx context.Context, ref PRReference, threadID string, resolved bool) error
	// GetCIJobLog fetches the log/trace of one CI job of ref's repository,
	// conditional on the request's ETag (see CIJobLog).
	GetCIJobLog(ctx context.Context, ref PRReference, req CIJobLogRequest) (CIJobLog, error)
	// CILogStreams reports whether GetCIJobLog serves a running job's log
	// as it grows (GitLab's trace), so following it at a quick cadence
	// shows progress. GitHub answers 404 until the job's log blob exists,
	// in practice once the job completed (ErrCIJobLogNotFound).
	CILogStreams() bool
	// FetchAttachment downloads one forge-hosted attachment referenced by
	// ref's body or review comments, through the user's own forge login.
	// A body larger than maxBytes is an error, not a truncation. See
	// forge_attachment.go.
	FetchAttachment(ctx context.Context, ref PRReference, target forgeattach.Target, maxBytes int64) ([]byte, error)
}

// PRReadParts names the parts of a PR one ReadPR decodes.
type PRReadParts struct {
	Detail, Threads, CI bool
}

func (p PRReadParts) none() bool { return !p.Detail && !p.Threads && !p.CI }

// PRRead is one ReadPR answer. A part ReadPR was not asked for is zero.
// HasCI reports that the CI part was read, so a caller can tell a PR
// with no pipeline (HasCI, empty CI) from a read that did not ask.
type PRRead struct {
	Detail  PRDetail
	Threads []ReviewThread
	CI      CIPipeline
	HasCI   bool
}

// MergedPRHead is one merged PR/MR's head coordinates as returned by
// ListMergedPRHeads.
type MergedPRHead struct {
	// HeadRefName is the PR's head / MR's source branch name.
	HeadRefName string
	// HeadOid is the full SHA of that branch when it was merged.
	HeadOid string
	// URL links the PR/MR for display.
	URL string
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
//
// AuthorName, here and on ReviewVerdict and ReviewComment, is the forge
// display name beside the AuthorLogin username. It is empty when the forge
// does not report one (GitHub review verdicts, bots, users without a
// display name); clients then show the login.
type PRDetail struct {
	Number         int             `json:"number"`
	Title          string          `json:"title"`
	Body           string          `json:"body"`
	AuthorLogin    string          `json:"authorLogin"`
	AuthorName     string          `json:"authorName,omitempty"`
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
	AuthorName  string `json:"authorName,omitempty"`
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
	AuthorName  string         `json:"authorName,omitempty"`
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

// PRReference identifies a PR/MR by host, namespace, repo, and number.
// Namespace carries the full path-segment chain before the repo
// (a single "owner" for GitHub, possibly a "group/sub/sub" chain for
// GitLab subgroups).
type PRReference struct {
	Forge string // "github" | "gitlab"
	// Host is the forge host the PR lives on, spelled as its URL's
	// URL.host (lowercase, a non-default port kept; see ParsePRURL).
	// Required.
	Host      string
	Namespace string // "owner" or "group/sub/..."
	Repo      string
	Number    int
}

// Project returns "namespace/repo", the repository path within Host.
func (r PRReference) Project() string {
	if r.Namespace == "" {
		return r.Repo
	}
	return r.Namespace + "/" + r.Repo
}

// publicForgeHosts maps a forge id to its public host, the one a PR key
// leaves implicit.
var publicForgeHosts = map[string]string{
	"github": "github.com",
	"gitlab": "gitlab.com",
}

// Key is the entity key for the PR: "forge:namespace/repo:number" on the
// forge's public host and "forge@host:namespace/repo:number" on any other.
// The frontend's prKey (frontend/src/lib/utils/prReference.ts) builds the
// identical string. Segments never contain ':' and a valid host never
// contains '@' (Validate), so two references share a key only when they
// name the same PR. Keys are compared, never parsed.
func (r PRReference) Key() string {
	if publicForgeHosts[r.Forge] == r.Host {
		return fmt.Sprintf("%s:%s:%d", r.Forge, r.Project(), r.Number)
	}
	return fmt.Sprintf("%s@%s:%s:%d", r.Forge, r.Host, r.Project(), r.Number)
}

// Validate reports whether r names a PR a forge operation can address: a
// supported forge, a lowercase host, a project path that satisfies
// SplitProjectForForge, and a positive number.
func (r PRReference) Validate() error {
	if _, ok := publicForgeHosts[r.Forge]; !ok {
		return fmt.Errorf("unsupported forge %q: %w", r.Forge, ErrUnsupportedForge)
	}
	if err := validatePRHost(r.Host); err != nil {
		return err
	}
	if r.Number <= 0 {
		return fmt.Errorf("PR number must be positive, got %d", r.Number)
	}
	_, _, err := SplitProjectForForge(r.Forge, r.Project())
	return err
}

// validatePRHost accepts a URL host as ParsePRURL records it: lowercase,
// optionally with a port or as a bracketed IPv6 literal. Userinfo, paths,
// whitespace and control characters are refused so the host cannot alias
// another key or reach a request line.
func validatePRHost(host string) error {
	if host == "" {
		return errors.New("PR host is required")
	}
	if host != strings.ToLower(host) {
		return fmt.Errorf("PR host %q must be lowercase", host)
	}
	for _, r := range host {
		if r <= 0x20 || r == 0x7f || strings.ContainsRune("/\\@?#%", r) {
			return fmt.Errorf("PR host %q contains an invalid character", host)
		}
	}
	return nil
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

func (nullForge) ListOpenPRs(context.Context, string, string) ([]GitPR, error) {
	return nil, ErrUnsupportedForge
}

func (nullForge) ListMergedPRHeads(context.Context, string, int) ([]MergedPRHead, error) {
	return nil, ErrUnsupportedForge
}

func (nullForge) CreatePR(context.Context, string, string, string, string, bool) (string, error) {
	return "", ErrUnsupportedForge
}

func (nullForge) ReadPR(context.Context, PRReference, PRReadParts, *CIPipeline, []string) (PRRead, error) {
	return PRRead{}, ErrUnsupportedForge
}

func (nullForge) SubmitReview(context.Context, PRReference, SubmitReviewRequest) (SubmitReviewResult, error) {
	return SubmitReviewResult{}, ErrUnsupportedForge
}

func (nullForge) ReplyToThread(context.Context, PRReference, string, int64, string) error {
	return ErrUnsupportedForge
}

func (nullForge) SetThreadResolved(context.Context, PRReference, string, bool) error {
	return ErrUnsupportedForge
}

func (nullForge) CILogStreams() bool { return false }

func (nullForge) GetCIJobLog(context.Context, PRReference, CIJobLogRequest) (CIJobLog, error) {
	return CIJobLog{}, ErrUnsupportedForge
}

// SplitProjectForForge separates "namespace/repo" with per-forge
// segment rules: github requires exactly two segments (owner/repo),
// gitlab accepts any N≥2 segments where everything before the last is
// the namespace (group/sub/.../repo).
//
// Each segment is also validated against safe-name rules — no leading
// dashes (would be misread as flags by shell-out targets), no `.` or
// `..` (path traversal in forge API paths), no control characters
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
// dash, whitespace, control characters, and `:`. The accepted set covers
// all real github / gitlab owner / namespace / repo names.
//
// `:` is rejected because the PR entity key is `<forge>:<project>:<number>`
// (PRReference.Key / the frontend's prKey). A segment carrying a colon would
// let two different pull requests spell the same key, and the key is what
// every `pr:updated` frame is addressed by — one PR's poll results would
// land on another PR's panes. GitHub and GitLab both refuse `:` in a path
// segment, so nothing legitimate is lost.
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
		if r == ':' {
			return fmt.Errorf("segment %q must not contain ':'", seg)
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

// The PR-scoped Core operations validate ref before dispatching, so no
// caller's precondition decides what reaches a forge.

// ReadPR reads the parts of ref want names in one forge read (see
// Forge.ReadPR). Asking for no part is an error.
func (c *Core) ReadPR(ctx context.Context, ref PRReference, want PRReadParts, prev *CIPipeline, stepsFor []string) (PRRead, error) {
	if err := ref.Validate(); err != nil {
		return PRRead{}, err
	}
	if want.none() {
		return PRRead{}, errors.New("PR read names no part")
	}
	return c.ForgeByID(ref.Forge).ReadPR(ctx, ref, want, prev, stepsFor)
}

// GetPRDetail is ReadPR for the detail alone.
func (c *Core) GetPRDetail(ctx context.Context, ref PRReference) (PRDetail, error) {
	read, err := c.ReadPR(ctx, ref, PRReadParts{Detail: true}, nil, nil)
	return read.Detail, err
}

// ListReviewThreads is ReadPR for the threads alone.
func (c *Core) ListReviewThreads(ctx context.Context, ref PRReference) ([]ReviewThread, error) {
	read, err := c.ReadPR(ctx, ref, PRReadParts{Threads: true}, nil, nil)
	return read.Threads, err
}

func (c *Core) SubmitReview(ctx context.Context, ref PRReference, review SubmitReviewRequest) (SubmitReviewResult, error) {
	if err := ref.Validate(); err != nil {
		return SubmitReviewResult{}, err
	}
	return c.ForgeByID(ref.Forge).SubmitReview(ctx, ref, review)
}

func (c *Core) ReplyToThread(ctx context.Context, ref PRReference, threadID string, databaseID int64, body string) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).ReplyToThread(ctx, ref, threadID, databaseID, body)
}

func (c *Core) SetThreadResolved(ctx context.Context, ref PRReference, threadID string, resolved bool) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).SetThreadResolved(ctx, ref, threadID, resolved)
}

// ListPRCIJobs is ReadPR for the pipeline alone.
func (c *Core) ListPRCIJobs(ctx context.Context, ref PRReference, prev *CIPipeline, stepsFor []string) (CIPipeline, error) {
	read, err := c.ReadPR(ctx, ref, PRReadParts{CI: true}, prev, stepsFor)
	return read.CI, err
}

// CILogStreams reports whether ref's forge streams a running job's
// log (see Forge.CILogStreams).
func (c *Core) CILogStreams(ref PRReference) bool {
	return c.ForgeByID(ref.Forge).CILogStreams()
}

func (c *Core) GetCIJobLog(ctx context.Context, ref PRReference, req CIJobLogRequest) (CIJobLog, error) {
	if err := ref.Validate(); err != nil {
		return CIJobLog{}, err
	}
	return c.ForgeByID(ref.Forge).GetCIJobLog(ctx, ref, req)
}
