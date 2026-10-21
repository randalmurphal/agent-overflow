package forgefake

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Fixture is the forge state a test declares. Field names are the JSON a
// spec writes; every field a handler reads is documented where it is
// declared, and the handler that reads it names the field back.
type Fixture struct {
	// Viewer is the signed-in forge user (GitHub's GraphQL viewer).
	// Defaults to "ao-viewer".
	Viewer string `json:"viewer,omitempty"`
	Repos  []Repo `json:"repos"`
	// SSHHosts maps an SSH alias to the hostname returned by ssh -G.
	SSHHosts map[string]string `json:"sshHosts,omitempty"`
}

// Repo is one GitHub repository or GitLab project.
type Repo struct {
	// Forge is "github" or "gitlab".
	Forge string `json:"forge"`
	// Project is "owner/repo" (GitHub) or "group/sub/repo" (GitLab).
	// Specs choose a name no other test uses: a repo is keyed by
	// (forge, project), and seeding the same key again replaces it.
	Project string `json:"project"`
	// Host is the web host URLs are built on. Defaults to github.com or
	// gitlab.com.
	Host string `json:"host,omitempty"`
	// ID is the forge repository id. GitLab also accepts it in API paths.
	// Generated when omitted.
	ID int64 `json:"id,omitempty"`
	// Pulls are the repository's pull or merge requests.
	Pulls []Pull `json:"pulls,omitempty"`
	// Attachments are the files the forge serves for the repository's
	// markdown.
	Attachments []Attachment `json:"attachments,omitempty"`
	// Merge is the project's merge settings and the viewer's role in it.
	// Omitted, it is a new project's: every GitHub method allowed, no
	// auto-merge, GitLab merge commits with squash off by default, and
	// the viewer a writer (GitHub WRITE, GitLab developer).
	Merge *RepoMerge `json:"merge,omitempty"`
	// Pipelines are the repository's pipelines beside its pulls' CI
	// (GitLab pipelines, GitHub workflow runs on any ref). The pipeline
	// lists answer both, newest (highest id) first.
	Pipelines []Pipeline `json:"pipelines,omitempty"`
	// DeletedBranches are head branches deleted from the repository
	// (GitHub's branch delete after a merge); a branch exists while a
	// pull names it as its head or base and it is not listed here.
	DeletedBranches []string `json:"deletedBranches,omitempty"`
	// EventsHidden makes the viewer's events on this GitLab project ones
	// GitLab no longer shows the viewer: the events feed selects them
	// into a page and drops them from it after, so a page comes back
	// short and still names its next page, as GitLab's does.
	EventsHidden bool `json:"eventsHidden,omitempty"`
}

// RepoMerge is a project's merge settings (GitHub repository settings,
// GitLab project settings) and the viewer's role. Fields of the other
// forge are refused.
type RepoMerge struct {
	// Permission is the viewer's role: GitHub READ, TRIAGE, WRITE
	// (default), MAINTAIN or ADMIN (viewerPermission); GitLab guest,
	// reporter, developer (default), maintainer or owner (access_level 10
	// to 50). Merging and approving need WRITE or developer.
	Permission string `json:"permission,omitempty"`
	// Methods are the merge methods a GitHub repository allows (merge,
	// squash, rebase), all three by default; DefaultMethod is the
	// viewer's default (viewerDefaultMergeMethod), the first allowed by
	// default. AutoMergeAllowed is the repository's "Allow auto-merge",
	// off by default as on GitHub.
	Methods          []string `json:"methods"`
	DefaultMethod    string   `json:"defaultMethod,omitempty"`
	AutoMergeAllowed bool     `json:"autoMergeAllowed,omitempty"`
	// DeleteBranchOnMerge is GitHub's deleteBranchOnMerge and GitLab's
	// remove_source_branch_after_merge (a merge request's
	// force_remove_source_branch default).
	DeleteBranchOnMerge bool `json:"deleteBranchOnMerge,omitempty"`
	// MergeMethod is a GitLab project's merge_method: merge (default),
	// rebase_merge or ff. SquashOption is its squash_option: default_off
	// (default), default_on, always or never.
	MergeMethod  string `json:"mergeMethod,omitempty"`
	SquashOption string `json:"squashOption,omitempty"`
	// PipelineMustSucceed and DiscussionsMustResolve are GitLab's
	// only_allow_merge_if_pipeline_succeeds and
	// only_allow_merge_if_all_discussions_are_resolved.
	PipelineMustSucceed    bool `json:"pipelineMustSucceed,omitempty"`
	DiscussionsMustResolve bool `json:"discussionsMustResolve,omitempty"`
}

// Pull is one pull request (GitHub) or merge request (GitLab).
type Pull struct {
	// Number is the PR number or MR iid.
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body,omitempty"`
	// State is "open" (default), "closed" or "merged".
	State string `json:"state,omitempty"`
	// MergedAt is when a merged pull merged, RFC 3339 (GitHub mergedAt,
	// GitLab merged_at). Only a merged pull has one; it defaults to the
	// time the fixture was seeded.
	MergedAt string `json:"mergedAt,omitempty"`
	// NodeID is a GitHub pull's GraphQL node id. Defaults to an id derived
	// from the repository and number, so a reseed, or the same seed in a
	// relaunched harness, answers the id a real pull keeps.
	NodeID string `json:"nodeId,omitempty"`
	Draft  bool   `json:"draft,omitempty"`
	Author string `json:"author,omitempty"`
	// AuthorName is the author's display name. Empty answers as a user
	// with no display name.
	AuthorName string `json:"authorName,omitempty"`
	// HeadRef and BaseRef default to "feature" and "main".
	HeadRef string `json:"headRef,omitempty"`
	BaseRef string `json:"baseRef,omitempty"`
	// HeadRepo is the fork the head branch lives in ("owner/repo" or a
	// GitLab path); empty is the repository itself. A fork's pull answers
	// isCrossRepository (GitHub) and a source_project_id of its own
	// (GitLab).
	HeadRepo string `json:"headRepo,omitempty"`
	// headRepoID is the fork's generated project id.
	headRepoID int64
	// Reviewers are the logins a review is requested from (GitHub
	// reviewRequests, GitLab reviewers). A GitHub review from one ends
	// its request, as on GitHub; GitLab keeps its reviewers.
	Reviewers []string `json:"reviewers,omitempty"`
	// ChangeRequests are the GitLab reviewers (each one of Reviewers) whose
	// review requested changes: their reviewer state is requested_changes
	// and the merge request's detailed_merge_status is requested_changes.
	ChangeRequests []string `json:"changeRequests,omitempty"`
	// CreatedAt and UpdatedAt are RFC 3339; they default to
	// 2026-01-01T00:00:00Z and to CreatedAt. CommittedAt is the head
	// commit's time (GitHub committedDate), defaulting to CreatedAt, and
	// Commits its commit count (GitHub commits.totalCount), 1 by default.
	CreatedAt   string `json:"createdAt,omitempty"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
	CommittedAt string `json:"committedAt,omitempty"`
	Commits     int    `json:"commits,omitempty"`
	// HeadSHA, BaseSHA and StartSHA are generated when omitted. StartSHA is
	// the base branch's tip (GitHub baseRefOid, GitLab start_sha); BaseSHA
	// is GitLab's merge base.
	HeadSHA  string `json:"headSha,omitempty"`
	BaseSHA  string `json:"baseSha,omitempty"`
	StartSHA string `json:"startSha,omitempty"`
	// DiffPending answers a GitLab merge request as GitLab does right after
	// creating it, before it has computed the diff: diff_refs is null.
	DiffPending bool `json:"diffPending,omitempty"`
	// Diff is the PR's unified diff. The file and line counts the PR reads
	// report are derived from it.
	Diff string `json:"diff,omitempty"`
	// Mergeable is "clean" (default), "conflicts" or "checking".
	Mergeable string `json:"mergeable,omitempty"`
	// Comments are PR-level conversation comments (GitHub issue comments,
	// GitLab unpositioned discussions).
	Comments []Comment `json:"comments,omitempty"`
	// Threads are review threads anchored to the diff.
	Threads []Thread `json:"threads,omitempty"`
	// Reviews are submitted verdicts. On GitLab an APPROVED review is an
	// approval; other states have no GitLab counterpart.
	Reviews []Review `json:"reviews,omitempty"`
	// CI is the head pipeline (GitLab) or workflow run (GitHub Actions).
	// It defaults to a merge request pipeline of the pull's head.
	CI *Pipeline `json:"ci,omitempty"`
	// Readiness is what the base branch and the project require of the
	// pull and where it stands beyond its reviews, CI, threads and
	// conflicts. Omitted, nothing is required: an open pull with no
	// conflicts reads as ready on both forges, as on an unprotected
	// branch.
	Readiness *Readiness `json:"readiness,omitempty"`
	// MergedBy is who merged a merged pull; defaults to its author.
	// MergeCommitSHA is the merge commit, generated for a merged pull.
	MergedBy       string `json:"mergedBy,omitempty"`
	MergeCommitSHA string `json:"mergeCommitSha,omitempty"`
	// ClosedAt is when a closed or merged pull closed, RFC 3339; it
	// defaults to MergedAt for a merged pull and to the time it was seeded
	// for a closed one. ClosedBy is who closed it; defaults to its author.
	ClosedAt string `json:"closedAt,omitempty"`
	ClosedBy string `json:"closedBy,omitempty"`
	// Events are the pull's forge activity beyond its reviews: GitHub
	// timeline items, GitLab system notes. A merge action, an approval,
	// an auto-merge and the like append theirs. A GitHub review is its
	// own timeline item (from Reviews); a seeded GitLab approval carries
	// no note, so seed an approved event beside it for one. A seeded
	// GitHub pull that is merged or closed gets its merged and closed
	// events when it has none.
	Events []Event `json:"events,omitempty"`

	// squash is how a GitLab merge request merges or merged (its squash
	// flag), squashSHA the squash commit of a squashed merge; forceRemove
	// is its force_remove_source_branch.
	squash      bool
	squashSHA   string
	forceRemove bool
}

// Readiness is the merge box state a pull carries beyond its reviews, CI,
// threads and conflicts.
type Readiness struct {
	// RequiredApprovals is the approvals the merge needs: GitHub's base
	// protection requiredApprovingReviewCount, GitLab's
	// approvals_required.
	RequiredApprovals int `json:"requiredApprovals,omitempty"`
	// RequiredChecks names the CI jobs GitHub's base requires to pass
	// (each check's isRequired); a required name with no job reads as
	// expected and pending. GitLab requires the whole pipeline instead,
	// with RepoMerge.PipelineMustSucceed.
	RequiredChecks []string `json:"requiredChecks,omitempty"`
	// ResolveThreads is GitHub's base requiresConversationResolution.
	// GitLab's is RepoMerge.DiscussionsMustResolve.
	ResolveThreads bool `json:"resolveThreads,omitempty"`
	// Behind is a head behind its base where the base requires it be up
	// to date: GitHub BEHIND, GitLab need_rebase (a fast-forward
	// project). An update or rebase clears it.
	Behind bool `json:"behind,omitempty"`
	// ViewerCanApprove overrides whether GitLab takes the viewer's
	// approval (user_can_approve, which reads false once they approved);
	// nil derives it from the viewer's role. GitHub refuses only the
	// author.
	ViewerCanApprove *bool `json:"viewerCanApprove,omitempty"`
	// AutoMerge is an armed auto-merge.
	AutoMerge *AutoMerge `json:"autoMerge,omitempty"`
}

// AutoMerge is an armed merge-when-ready.
type AutoMerge struct {
	// Method is merge (default), squash or rebase.
	Method string `json:"method,omitempty"`
	// By is who armed it, defaulting to the pull's author; ByName their
	// display name.
	By     string `json:"by,omitempty"`
	ByName string `json:"byName,omitempty"`
	// At is when, RFC 3339; defaults to 2026-01-01T00:00:00Z.
	At string `json:"at,omitempty"`
}

// Event is one forge activity item. Kind is the app's vocabulary and
// each forge answers the kinds it records: GitHub review_dismissed,
// merged, closed, reopened, marked_draft, marked_ready, auto_merge_set,
// auto_merge_canceled, force_pushed, base_changed, review_requested;
// GitLab approved, unapproved, merged, closed, reopened, marked_draft,
// marked_ready, auto_merge_set, auto_merge_canceled, base_changed,
// review_requested (each in the system note phrasing GitLab writes).
type Event struct {
	// ID is the timeline item or note id; generated when omitted.
	ID   int64  `json:"id,omitempty"`
	Kind string `json:"kind"`
	// Actor defaults to the pull's author; ActorName needs Actor.
	Actor     string `json:"actor,omitempty"`
	ActorName string `json:"actorName,omitempty"`
	// At is RFC 3339; defaults to 2026-01-01T00:00:00Z.
	At string `json:"at,omitempty"`
	// Ref is what the event names: the new base (base_changed), the
	// reviewer (review_requested; GitLab a comma list), the head after a
	// force push, the dismissed review's author, the commit an
	// auto-merge was set for (GitLab; defaults to the head).
	Ref string `json:"ref,omitempty"`
	// Method is an auto_merge_set's method on GitHub: merge (default),
	// squash or rebase.
	Method string `json:"method,omitempty"`
	// Text is a review_dismissed's message, an auto_merge_canceled's
	// reason (GitLab: "aborted the automatic merge because <text>"), or a
	// GitLab base_changed's old branch (defaults to "main").
	Text string `json:"text,omitempty"`
}

// Comment is one comment or note.
type Comment struct {
	// ID is the numeric id (GitHub databaseId, GitLab note id).
	// Generated when omitted.
	ID int64 `json:"id,omitempty"`
	// Author and AuthorName default to the pull's author. AuthorName
	// needs an explicit Author; on GitHub an empty name answers as a bot,
	// whose author carries no name.
	Author     string `json:"author,omitempty"`
	AuthorName string `json:"authorName,omitempty"`
	Body       string `json:"body"`
	CreatedAt  string `json:"createdAt,omitempty"`
}

// Thread is one review thread.
type Thread struct {
	// ID is the GitHub node id or GitLab discussion id. Generated when
	// omitted.
	ID   string `json:"id,omitempty"`
	Path string `json:"path"`
	// Line and StartLine are nil for a file-level thread.
	Line      *int `json:"line,omitempty"`
	StartLine *int `json:"startLine,omitempty"`
	// Side is "right" (default), "left" or "file".
	Side     string    `json:"side,omitempty"`
	Resolved bool      `json:"resolved,omitempty"`
	Outdated bool      `json:"outdated,omitempty"`
	Comments []Comment `json:"comments"`
}

// Review is one submitted review verdict.
type Review struct {
	// ID is the review's numeric id (GitHub databaseId; its node id is
	// derived from it). Generated when omitted.
	ID     int64  `json:"id,omitempty"`
	Author string `json:"author"`
	// AuthorName is the reviewer's display name. Empty answers as a user
	// with no display name (GitLab) or a bot (GitHub).
	AuthorName string `json:"authorName,omitempty"`
	// State is GitHub's vocabulary: APPROVED, CHANGES_REQUESTED,
	// COMMENTED.
	State       string `json:"state"`
	Body        string `json:"body,omitempty"`
	SubmittedAt string `json:"submittedAt,omitempty"`
	CommitSHA   string `json:"commitSha,omitempty"`
}

// Pipeline is one CI run: a pull's, or one of the repository's.
type Pipeline struct {
	// ID is the GitLab pipeline id or GitHub Actions run id, unique in its
	// repository. Generated when omitted.
	ID int64 `json:"id,omitempty"`
	// Name is the GitHub workflow name. Defaults to "CI".
	Name string `json:"name,omitempty"`
	// RefKind is what Ref is: branch, tag or merge_request. A pull's
	// pipeline defaults to merge_request, a repository's to branch. Each
	// forge answers it as it does: GitHub only by the run's event, GitLab
	// by its merge request ref and a single pipeline's tag flag.
	RefKind string `json:"refKind,omitempty"`
	// Ref is the ref it ran on: a pull's head branch (GitLab: its
	// refs/merge-requests/N/head), else main. A GitLab merge_request
	// pipeline's ref is refs/merge-requests/N/head, merge or train.
	Ref string `json:"ref,omitempty"`
	// SHA defaults to the pull's head, else a generated commit.
	SHA string `json:"sha,omitempty"`
	// Source is GitLab's source or GitHub's event; it defaults by kind
	// (GitHub push, release, pull_request; GitLab push, push,
	// merge_request_event).
	Source string `json:"source,omitempty"`
	// Title is the GitHub run's display title; defaults to Name.
	Title string `json:"title,omitempty"`
	// CreatedAt and UpdatedAt are RFC 3339; they default to
	// 2026-01-01T00:00:00Z and to CreatedAt.
	CreatedAt string `json:"createdAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
	// Status is the pipeline status in the job vocabulary below.
	// Defaults to the aggregate of the jobs.
	Status string `json:"status,omitempty"`
	Jobs   []Job  `json:"jobs,omitempty"`
}

// Job is one CI job.
type Job struct {
	// ID is generated when omitted.
	ID    int64  `json:"id,omitempty"`
	Name  string `json:"name"`
	Stage string `json:"stage,omitempty"`
	// Status is one of success, failed, running, pending, skipped,
	// canceled. Each forge's handler spells it the way that forge does.
	Status       string `json:"status"`
	AllowFailure bool   `json:"allowFailure,omitempty"`
	StartedAt    string `json:"startedAt,omitempty"`
	CompletedAt  string `json:"completedAt,omitempty"`
	// Log is the job log. An empty log on a started job is served empty.
	Log string `json:"log,omitempty"`
	// LogWithheld answers the job's log with the forge's 404, as a forge
	// does for a completed job whose log it has not published yet.
	LogWithheld bool `json:"logWithheld,omitempty"`
	// Synthetic generates the log on each request instead of Log (see
	// SyntheticLog), for a log of many megabytes.
	Synthetic *SyntheticLog `json:"synthetic,omitempty"`
	Steps     []Step        `json:"steps,omitempty"`
}

// Step is one GitHub Actions job step.
type Step struct {
	// Number is GitHub's step number. Defaults to one past the previous
	// step's; give one to model the gaps real jobs have.
	Number int    `json:"number,omitempty"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// StartedAt and CompletedAt are RFC 3339 times, answered as null when
	// empty, as the API does for a step that has not started or ended.
	StartedAt   string `json:"startedAt,omitempty"`
	CompletedAt string `json:"completedAt,omitempty"`
}

// Attachment is one forge-hosted file.
type Attachment struct {
	// URL is a GitHub attachment's request URL, exactly as the app asks
	// gh for it (https://github.com/user-attachments/assets/<id>, ...).
	URL string `json:"url,omitempty"`
	// Secret and Filename name a GitLab upload: /uploads/<secret>/<filename>.
	Secret   string `json:"secret,omitempty"`
	Filename string `json:"filename,omitempty"`
	// ContentType is sent when a caller asks for response headers.
	// Defaults to application/octet-stream.
	ContentType string `json:"contentType,omitempty"`
	// Base64 or Text is the content; exactly one is set.
	Base64 string `json:"base64,omitempty"`
	Text   string `json:"text,omitempty"`

	content []byte
}

var (
	gitLabUploadSecret = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// gitlabDraftTitle is GitLab's draft title prefix (MergeRequest
	// DRAFT_REGEX): a merge request is a draft exactly when its title
	// carries it.
	gitlabDraftTitle = regexp.MustCompile(`(?i)^(\[draft\]|\(draft\)|draft:)+\s*`)
	mergeMethods     = map[string]bool{"merge": true, "squash": true, "rebase": true}
	githubRoles      = map[string]int{"READ": 1, "TRIAGE": 2, "WRITE": 3, "MAINTAIN": 4, "ADMIN": 5}
	gitlabRoles      = map[string]int{"guest": 10, "reporter": 20, "developer": 30, "maintainer": 40, "owner": 50}
	githubEventKinds = map[string]bool{
		"review_dismissed": true, "merged": true, "closed": true, "reopened": true, "marked_draft": true, "marked_ready": true,
		"auto_merge_set": true, "auto_merge_canceled": true, "force_pushed": true, "base_changed": true, "review_requested": true,
	}
	gitlabEventKinds = map[string]bool{
		"approved": true, "unapproved": true, "merged": true, "closed": true, "reopened": true, "marked_draft": true,
		"marked_ready": true, "auto_merge_set": true, "auto_merge_canceled": true, "base_changed": true, "review_requested": true,
	}
	jobStatuses  = map[string]bool{"success": true, "failed": true, "running": true, "pending": true, "skipped": true, "canceled": true}
	reviewStates = map[string]bool{"APPROVED": true, "CHANGES_REQUESTED": true, "COMMENTED": true, "DISMISSED": true, "PENDING": true}
)

// clone is a deep copy, so the engine never shares a slice or pointer
// with its caller. A JSON round trip copies exactly the declared fields;
// the decoded attachment content is rebuilt by normalize.
func (f Fixture) clone() Fixture {
	raw, err := json.Marshal(f)
	if err != nil {
		panic(fmt.Sprintf("forgefake: marshal fixture: %v", err))
	}
	var out Fixture
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(fmt.Sprintf("forgefake: unmarshal fixture: %v", err))
	}
	return out
}

// normalize validates the fixture and fills every generated default, so
// the handlers read one complete shape and the caller gets back the ids
// it did not choose.
func (f *Fixture) normalize(ids *idSource) error {
	for alias, host := range f.SSHHosts {
		if !fixtureSSHHost.MatchString(alias) || !fixtureSSHHost.MatchString(host) {
			return fmt.Errorf("invalid SSH host mapping")
		}
	}
	if len(f.Repos) == 0 {
		return errors.New("forge fixture: no repos")
	}
	seen := make(map[string]bool, len(f.Repos))
	for i := range f.Repos {
		repo := &f.Repos[i]
		if err := repo.normalize(ids); err != nil {
			return fmt.Errorf("forge fixture: repos[%d] (%s %s): %w", i, repo.Forge, repo.Project, err)
		}
		key := repo.key()
		if seen[key] {
			return fmt.Errorf("forge fixture: %s %s is declared twice", repo.Forge, repo.Project)
		}
		seen[key] = true
	}
	return nil
}

func (r *Repo) key() string { return r.Forge + "\x00" + strings.ToLower(r.Project) }

func (r *Repo) normalize(ids *idSource) error {
	switch r.Forge {
	case "github":
		if strings.Count(r.Project, "/") != 1 {
			return errors.New(`a GitHub project is "owner/repo"`)
		}
		if r.Host == "" {
			r.Host = "github.com"
		}
		if r.ID == 0 {
			r.ID = ids.next()
		}
	case "gitlab":
		if strings.Count(r.Project, "/") < 1 {
			return errors.New(`a GitLab project is "group/repo"`)
		}
		if r.Host == "" {
			r.Host = "gitlab.com"
		}
		if r.ID == 0 {
			r.ID = ids.next()
		}
	default:
		return fmt.Errorf("forge %q is not github or gitlab", r.Forge)
	}
	if r.EventsHidden && r.Forge != "gitlab" {
		return errors.New("eventsHidden is a GitLab project's")
	}
	for _, part := range strings.Split(r.Project, "/") {
		if part == "" || strings.TrimSpace(part) != part {
			return fmt.Errorf("project %q has an empty or padded segment", r.Project)
		}
	}
	if r.Merge == nil {
		r.Merge = &RepoMerge{}
	}
	if err := r.Merge.normalize(r.Forge); err != nil {
		return fmt.Errorf("merge: %w", err)
	}
	for _, branch := range r.DeletedBranches {
		if strings.TrimSpace(branch) == "" {
			return errors.New("deletedBranches holds an empty name")
		}
	}
	numbers := make(map[int]bool, len(r.Pulls))
	for i := range r.Pulls {
		pull := &r.Pulls[i]
		if err := pull.normalize(r.Forge, ids); err != nil {
			return fmt.Errorf("pulls[%d] (#%d): %w", i, pull.Number, err)
		}
		if numbers[pull.Number] {
			return fmt.Errorf("pull #%d is declared twice", pull.Number)
		}
		numbers[pull.Number] = true
		r.defaultNodeID(pull)
		if err := pull.normalizeMerge(r, ids); err != nil {
			return fmt.Errorf("pulls[%d] (#%d): %w", i, pull.Number, err)
		}
		if pull.HeadRepo != "" {
			if !strings.Contains(pull.HeadRepo, "/") || strings.EqualFold(pull.HeadRepo, r.Project) {
				return fmt.Errorf("pulls[%d] (#%d): headRepo %q is not another repository's path", i, pull.Number, pull.HeadRepo)
			}
			pull.headRepoID = ids.next()
		}
	}
	if err := r.normalizePipelines(ids); err != nil {
		return err
	}
	for i := range r.Attachments {
		if err := r.Attachments[i].normalize(r.Forge); err != nil {
			return fmt.Errorf("attachments[%d]: %w", i, err)
		}
	}
	return nil
}

func (m *RepoMerge) normalize(forge string) error {
	if forge == "github" {
		if m.MergeMethod != "" || m.SquashOption != "" || m.PipelineMustSucceed || m.DiscussionsMustResolve {
			return errors.New("mergeMethod, squashOption, pipelineMustSucceed and discussionsMustResolve are GitLab project settings")
		}
		m.Permission = defaultString(m.Permission, "WRITE")
		if githubRoles[m.Permission] == 0 {
			return fmt.Errorf("permission %q is not READ, TRIAGE, WRITE, MAINTAIN or ADMIN", m.Permission)
		}
		if m.Methods == nil {
			m.Methods = []string{"merge", "squash", "rebase"}
		}
		if len(m.Methods) == 0 {
			return errors.New("a GitHub repository allows at least one merge method")
		}
		for _, method := range m.Methods {
			if !mergeMethods[method] {
				return fmt.Errorf("method %q is not merge, squash or rebase", method)
			}
		}
		m.DefaultMethod = defaultString(m.DefaultMethod, m.Methods[0])
		if !slices.Contains(m.Methods, m.DefaultMethod) {
			return fmt.Errorf("defaultMethod %q is not an allowed method", m.DefaultMethod)
		}
		return nil
	}
	if m.Methods != nil || m.DefaultMethod != "" || m.AutoMergeAllowed {
		return errors.New("methods, defaultMethod and autoMergeAllowed are GitHub repository settings")
	}
	m.Permission = defaultString(m.Permission, "developer")
	if gitlabRoles[m.Permission] == 0 {
		return fmt.Errorf("permission %q is not guest, reporter, developer, maintainer or owner", m.Permission)
	}
	m.MergeMethod = defaultString(m.MergeMethod, "merge")
	switch m.MergeMethod {
	case "merge", "rebase_merge", "ff":
	default:
		return fmt.Errorf("mergeMethod %q is not merge, rebase_merge or ff", m.MergeMethod)
	}
	m.SquashOption = defaultString(m.SquashOption, "default_off")
	switch m.SquashOption {
	case "default_off", "default_on", "always", "never":
	default:
		return fmt.Errorf("squashOption %q is not default_off, default_on, always or never", m.SquashOption)
	}
	return nil
}

// defaultNodeID gives a GitHub pull without one the node id derived from
// its repository and number.
func (r *Repo) defaultNodeID(p *Pull) {
	if r.Forge != "github" || p.NodeID != "" {
		return
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%s#%d", strings.ToLower(r.Host), strings.ToLower(r.Project), p.Number))
	p.NodeID = "PR_ao" + hex.EncodeToString(sum[:10])
}

func (p *Pull) normalize(forge string, ids *idSource) error {
	if p.Number <= 0 {
		return errors.New("number must be positive")
	}
	if strings.TrimSpace(p.Title) == "" {
		return errors.New("title is required")
	}
	if p.DiffPending && forge != "gitlab" {
		return errors.New("diffPending is a GitLab merge request state")
	}
	switch p.State {
	case "":
		p.State = "open"
	case "open", "closed", "merged":
	default:
		return fmt.Errorf("state %q is not open, closed or merged", p.State)
	}
	switch {
	case p.State != "merged" && p.MergedAt != "":
		return errors.New("mergedAt is set on a pull that did not merge")
	case p.State == "merged" && p.MergedAt == "":
		p.MergedAt = time.Now().UTC().Format(time.RFC3339)
	case p.State == "merged":
		if _, err := time.Parse(time.RFC3339, p.MergedAt); err != nil {
			return fmt.Errorf("mergedAt %q is not RFC 3339", p.MergedAt)
		}
	}
	if p.NodeID != "" && forge != "github" {
		return errors.New("nodeId is a GitHub pull's")
	}
	switch p.Mergeable {
	case "":
		p.Mergeable = "clean"
	case "clean", "conflicts", "checking":
	default:
		return fmt.Errorf("mergeable %q is not clean, conflicts or checking", p.Mergeable)
	}
	if err := p.normalizeList(); err != nil {
		return err
	}
	p.Author = defaultString(p.Author, "ao-author")
	p.HeadRef = defaultString(p.HeadRef, "feature")
	p.BaseRef = defaultString(p.BaseRef, "main")
	p.HeadSHA = defaultString(p.HeadSHA, ids.sha())
	p.BaseSHA = defaultString(p.BaseSHA, ids.sha())
	p.StartSHA = defaultString(p.StartSHA, p.BaseSHA)
	for i := range p.Comments {
		if err := p.Comments[i].normalize(p.Author, p.AuthorName, ids); err != nil {
			return fmt.Errorf("comments[%d]: %w", i, err)
		}
	}
	for i := range p.Threads {
		thread := &p.Threads[i]
		if err := thread.normalize(forge, p.Author, p.AuthorName, ids); err != nil {
			return fmt.Errorf("threads[%d]: %w", i, err)
		}
	}
	for i, review := range p.Reviews {
		if review.Author == "" || !reviewStates[review.State] {
			return fmt.Errorf("reviews[%d]: author and a GitHub review state are required", i)
		}
		if forge == "gitlab" && review.State != "APPROVED" {
			return fmt.Errorf("reviews[%d]: GitLab has approvals only; state must be APPROVED", i)
		}
		if review.ID == 0 {
			p.Reviews[i].ID = ids.next()
		}
		p.Reviews[i].SubmittedAt = defaultString(review.SubmittedAt, "2026-01-01T00:00:00Z")
		p.Reviews[i].CommitSHA = defaultString(review.CommitSHA, p.HeadSHA)
	}
	for i, login := range p.ChangeRequests {
		same := func(other string) bool { return strings.EqualFold(other, login) }
		switch {
		case forge != "gitlab":
			return fmt.Errorf("changeRequests: GitLab only; a GitHub change request is a review")
		case !slices.ContainsFunc(p.Reviewers, same):
			return fmt.Errorf("changeRequests[%d]: %q is not one of the reviewers", i, login)
		case slices.ContainsFunc(p.Reviews, func(r Review) bool { return same(r.Author) }):
			return fmt.Errorf("changeRequests[%d]: %q also approved", i, login)
		}
	}
	if p.CI != nil {
		if err := p.CI.normalize(ids); err != nil {
			return fmt.Errorf("ci: %w", err)
		}
		if err := p.CI.normalizeRun(forge, p, ids); err != nil {
			return fmt.Errorf("ci: %w", err)
		}
	}
	return nil
}

// normalizeMerge validates and fills the pull's merge state: its
// readiness, its end of life and its events. It runs once the pull's own
// fields are filled.
func (p *Pull) normalizeMerge(r *Repo, ids *idSource) error {
	forge := r.Forge
	if forge == "gitlab" {
		// GitLab's draft is its title prefix: a draft seed without one
		// gets GitLab's own, a title with one is a draft.
		switch {
		case gitlabDraftTitle.MatchString(p.Title):
			p.Draft = true
		case p.Draft:
			p.Title = "Draft: " + p.Title
		}
		p.squash = r.Merge.SquashOption == "default_on" || r.Merge.SquashOption == "always"
		p.forceRemove = r.Merge.DeleteBranchOnMerge
	}
	switch {
	case p.State != "merged" && (p.MergedBy != "" || p.MergeCommitSHA != ""):
		return errors.New("mergedBy and mergeCommitSha are a merged pull's")
	case p.State == "open" && (p.ClosedAt != "" || p.ClosedBy != ""):
		return errors.New("closedAt and closedBy are a closed or merged pull's")
	}
	switch p.State {
	case "merged":
		p.MergedBy = defaultString(p.MergedBy, p.Author)
		p.MergeCommitSHA = defaultString(p.MergeCommitSHA, ids.sha())
		p.ClosedAt = defaultString(p.ClosedAt, p.MergedAt)
		p.ClosedBy = defaultString(p.ClosedBy, p.MergedBy)
	case "closed":
		p.ClosedAt = defaultString(p.ClosedAt, time.Now().UTC().Format(time.RFC3339))
		p.ClosedBy = defaultString(p.ClosedBy, p.Author)
	}
	if p.ClosedAt != "" {
		if _, err := time.Parse(time.RFC3339, p.ClosedAt); err != nil {
			return fmt.Errorf("closedAt %q is not RFC 3339", p.ClosedAt)
		}
	}
	if p.Readiness == nil {
		p.Readiness = &Readiness{}
	}
	if err := p.Readiness.normalize(forge, p); err != nil {
		return fmt.Errorf("readiness: %w", err)
	}
	if forge == "gitlab" && p.Readiness.Behind && r.Merge.MergeMethod == "merge" {
		return errors.New("readiness: behind is GitLab's need_rebase, which a merge-commit project never reports; set merge.mergeMethod to ff or rebase_merge")
	}
	kinds := githubEventKinds
	if forge == "gitlab" {
		kinds = gitlabEventKinds
	}
	for i := range p.Events {
		event := &p.Events[i]
		if !kinds[event.Kind] {
			return fmt.Errorf("events[%d]: kind %q is not a %s event", i, event.Kind, forge)
		}
		if err := event.normalize(forge, p, ids); err != nil {
			return fmt.Errorf("events[%d]: %w", i, err)
		}
	}
	if forge == "github" && p.State != "open" {
		// GitHub records every merge and close in the timeline: a merge
		// leaves a MergedEvent and a ClosedEvent in the same second.
		has := func(kind string) bool {
			return slices.ContainsFunc(p.Events, func(e Event) bool { return e.Kind == kind })
		}
		if p.State == "merged" && !has("merged") {
			p.Events = append(p.Events, Event{ID: ids.next(), Kind: "merged", Actor: p.MergedBy, At: p.MergedAt, Ref: p.MergeCommitSHA})
		}
		if !has("closed") {
			p.Events = append(p.Events, Event{ID: ids.next(), Kind: "closed", Actor: p.ClosedBy, At: p.ClosedAt})
		}
	}
	return nil
}

func (rd *Readiness) normalize(forge string, p *Pull) error {
	if rd.RequiredApprovals < 0 {
		return errors.New("requiredApprovals is negative")
	}
	if forge == "gitlab" && (len(rd.RequiredChecks) > 0 || rd.ResolveThreads) {
		return errors.New("requiredChecks and resolveThreads are GitHub base protection; a GitLab project requires its pipeline and threads through merge.pipelineMustSucceed and merge.discussionsMustResolve")
	}
	if forge == "github" && rd.ViewerCanApprove != nil {
		return errors.New("viewerCanApprove is GitLab's user_can_approve; GitHub refuses only the author")
	}
	for _, name := range rd.RequiredChecks {
		if strings.TrimSpace(name) == "" {
			return errors.New("requiredChecks holds an empty name")
		}
	}
	if armed := rd.AutoMerge; armed != nil {
		if p.State != "open" {
			return errors.New("autoMerge is armed on an open pull only")
		}
		armed.Method = defaultString(armed.Method, "merge")
		if !mergeMethods[armed.Method] {
			return fmt.Errorf("autoMerge method %q is not merge, squash or rebase", armed.Method)
		}
		if armed.By == "" && armed.ByName != "" {
			return errors.New("autoMerge byName needs by")
		}
		armed.By = defaultString(armed.By, p.Author)
		armed.At = defaultString(armed.At, "2026-01-01T00:00:00Z")
		if _, err := time.Parse(time.RFC3339, armed.At); err != nil {
			return fmt.Errorf("autoMerge at %q is not RFC 3339", armed.At)
		}
		if forge == "gitlab" {
			p.squash = armed.Method == "squash"
		}
	}
	return nil
}

func (ev *Event) normalize(forge string, p *Pull, ids *idSource) error {
	if ev.ID == 0 {
		ev.ID = ids.next()
	}
	if ev.Actor == "" {
		if ev.ActorName != "" {
			return errors.New("actorName needs actor")
		}
		ev.Actor = p.Author
	}
	ev.At = defaultString(ev.At, "2026-01-01T00:00:00Z")
	if _, err := time.Parse(time.RFC3339, ev.At); err != nil {
		return fmt.Errorf("at %q is not RFC 3339", ev.At)
	}
	if ev.Method != "" && (forge != "github" || ev.Kind != "auto_merge_set") {
		return errors.New("method is a GitHub auto_merge_set's")
	}
	switch ev.Kind {
	case "auto_merge_set":
		if forge == "github" {
			ev.Method = defaultString(ev.Method, "merge")
			if !mergeMethods[ev.Method] {
				return fmt.Errorf("method %q is not merge, squash or rebase", ev.Method)
			}
		} else {
			ev.Ref = defaultString(ev.Ref, p.HeadSHA)
		}
	case "base_changed", "review_requested":
		if ev.Ref == "" {
			return fmt.Errorf("a %s event names its ref", ev.Kind)
		}
		if forge == "gitlab" && ev.Kind == "base_changed" {
			ev.Text = defaultString(ev.Text, "main")
		}
	case "review_dismissed":
		ev.Ref = defaultString(ev.Ref, p.Author)
	case "force_pushed":
		ev.Ref = defaultString(ev.Ref, p.HeadSHA)
	}
	return nil
}

func (c *Comment) normalize(author, authorName string, ids *idSource) error {
	if c.ID == 0 {
		c.ID = ids.next()
	}
	if c.Author == "" {
		if c.AuthorName != "" {
			return errors.New("authorName needs author")
		}
		c.Author, c.AuthorName = author, authorName
	}
	c.CreatedAt = defaultString(c.CreatedAt, "2026-01-01T00:00:00Z")
	return nil
}

func (t *Thread) normalize(forge, author, authorName string, ids *idSource) error {
	if t.Path == "" {
		return errors.New("path is required")
	}
	if len(t.Comments) == 0 {
		return errors.New("a thread needs at least one comment")
	}
	switch t.Side {
	case "":
		t.Side = "right"
	case "right", "left":
	case "file":
		if t.Line != nil || t.StartLine != nil {
			return errors.New("a file-level thread has no line")
		}
	default:
		return fmt.Errorf("side %q is not right, left or file", t.Side)
	}
	if t.Side != "file" && t.Line == nil {
		return errors.New("a line thread needs line")
	}
	if t.ID == "" {
		if forge == "github" {
			t.ID = fmt.Sprintf("PRRT_ao%d", ids.next())
		} else {
			t.ID = ids.sha()
		}
	}
	for i := range t.Comments {
		if err := t.Comments[i].normalize(author, authorName, ids); err != nil {
			return fmt.Errorf("comments[%d]: %w", i, err)
		}
	}
	return nil
}

func (p *Pipeline) normalize(ids *idSource) error {
	if p.ID == 0 {
		p.ID = ids.next()
	}
	p.Name = defaultString(p.Name, "CI")
	for i := range p.Jobs {
		job := &p.Jobs[i]
		if job.Name == "" || !jobStatuses[job.Status] {
			return fmt.Errorf("jobs[%d]: name and a status (success, failed, running, pending, skipped, canceled) are required", i)
		}
		for j := range job.Steps {
			step := &job.Steps[j]
			if step.Name == "" || !jobStatuses[step.Status] {
				return fmt.Errorf("jobs[%d].steps[%d]: name and a job status are required", i, j)
			}
			previous := 0
			if j > 0 {
				previous = job.Steps[j-1].Number
			}
			if step.Number == 0 {
				step.Number = previous + 1
			} else if step.Number <= previous {
				return fmt.Errorf("jobs[%d].steps[%d]: number %d does not follow %d", i, j, step.Number, previous)
			}
		}
		if job.Synthetic != nil {
			if job.Log != "" {
				return fmt.Errorf("jobs[%d]: log and synthetic are exclusive", i)
			}
			if err := job.Synthetic.normalize(); err != nil {
				return fmt.Errorf("jobs[%d]: %w", i, err)
			}
		}
		if job.ID == 0 {
			job.ID = ids.next()
		}
		job.Stage = defaultString(job.Stage, "test")
		if job.StartedAt == "" && job.Status != "pending" && job.Status != "skipped" {
			job.StartedAt = "2026-01-01T00:00:00Z"
		}
		if job.CompletedAt == "" && job.StartedAt != "" && job.Status != "running" {
			job.CompletedAt = "2026-01-01T00:01:00Z"
		}
	}
	if p.Status == "" {
		p.Status = aggregateJobStatus(p.Jobs)
	} else if !jobStatuses[p.Status] {
		return fmt.Errorf("status %q is not a job status", p.Status)
	}
	return nil
}

func aggregateJobStatus(jobs []Job) string {
	status := "success"
	for _, job := range jobs {
		switch {
		case job.Status == "failed" && !job.AllowFailure:
			return "failed"
		case job.Status == "running":
			status = "running"
		case job.Status == "pending" && status != "running":
			status = "pending"
		}
	}
	return status
}

func (a *Attachment) normalize(forge string) error {
	switch forge {
	case "github":
		if !strings.HasPrefix(a.URL, "https://") || a.Secret != "" || a.Filename != "" {
			return errors.New("a GitHub attachment is keyed by its https url only")
		}
	case "gitlab":
		if a.URL != "" || !gitLabUploadSecret.MatchString(a.Secret) || a.Filename == "" || strings.Contains(a.Filename, "/") {
			return errors.New("a GitLab upload is keyed by a 32-hex secret and a filename")
		}
	}
	switch {
	case a.Base64 != "" && a.Text != "":
		return errors.New("set base64 or text, not both")
	case a.Base64 != "":
		content, err := base64.StdEncoding.DecodeString(a.Base64)
		if err != nil {
			return fmt.Errorf("base64: %w", err)
		}
		a.content = content
	default:
		a.content = []byte(a.Text)
	}
	a.ContentType = defaultString(a.ContentType, "application/octet-stream")
	return nil
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// idSource hands out ids that never repeat for the life of the engine,
// across resets, so an id a previous test saw can only miss.
type idSource struct{ n int64 }

func (s *idSource) next() int64 {
	s.n++
	return 1_000_000 + s.n
}

// sha is a 40-hex id, distinct per call.
func (s *idSource) sha() string {
	return fmt.Sprintf("%040x", s.next())
}
