package forgefake

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Fixture is the forge state a test declares. Field names are the JSON a
// spec writes; every field a handler reads is documented where it is
// declared, and the handler that reads it names the field back.
type Fixture struct {
	// Viewer is the signed-in forge user (`gh api user`). Defaults to
	// "ao-viewer".
	Viewer string `json:"viewer,omitempty"`
	Repos  []Repo `json:"repos"`
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
	// ID is the GitLab numeric project id, the other spelling a GitLab
	// API path may use for the project. Generated when omitted.
	ID int64 `json:"id,omitempty"`
	// Pulls are the repository's pull or merge requests.
	Pulls []Pull `json:"pulls,omitempty"`
	// Attachments are the files the forge serves for the repository's
	// markdown.
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Pull is one pull request (GitHub) or merge request (GitLab).
type Pull struct {
	// Number is the PR number or MR iid.
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body,omitempty"`
	// State is "open" (default), "closed" or "merged".
	State  string `json:"state,omitempty"`
	Draft  bool   `json:"draft,omitempty"`
	Author string `json:"author,omitempty"`
	// HeadRef and BaseRef default to "feature" and "main".
	HeadRef string `json:"headRef,omitempty"`
	BaseRef string `json:"baseRef,omitempty"`
	// HeadSHA, BaseSHA and StartSHA are generated when omitted.
	HeadSHA  string `json:"headSha,omitempty"`
	BaseSHA  string `json:"baseSha,omitempty"`
	StartSHA string `json:"startSha,omitempty"`
	// Diff is the unified diff `gh pr diff` / `glab mr diff` print. File
	// and line counts are derived from it.
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
	CI *Pipeline `json:"ci,omitempty"`
}

// Comment is one comment or note.
type Comment struct {
	// ID is the numeric id (GitHub databaseId, GitLab note id).
	// Generated when omitted.
	ID        int64  `json:"id,omitempty"`
	Author    string `json:"author,omitempty"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt,omitempty"`
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
	Author string `json:"author"`
	// State is GitHub's vocabulary: APPROVED, CHANGES_REQUESTED,
	// COMMENTED.
	State       string `json:"state"`
	Body        string `json:"body,omitempty"`
	SubmittedAt string `json:"submittedAt,omitempty"`
	CommitSHA   string `json:"commitSha,omitempty"`
}

// Pipeline is the CI run a pull carries.
type Pipeline struct {
	// ID is the GitLab pipeline id or GitHub Actions run id. Generated
	// when omitted.
	ID int64 `json:"id,omitempty"`
	// Name is the GitHub workflow name. Defaults to "CI".
	Name string `json:"name,omitempty"`
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
	Log   string `json:"log,omitempty"`
	Steps []Step `json:"steps,omitempty"`
}

// Step is one GitHub Actions job step.
type Step struct {
	Name   string `json:"name"`
	Status string `json:"status"`
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
	jobStatuses        = map[string]bool{"success": true, "failed": true, "running": true, "pending": true, "skipped": true, "canceled": true}
	reviewStates       = map[string]bool{"APPROVED": true, "CHANGES_REQUESTED": true, "COMMENTED": true, "DISMISSED": true, "PENDING": true}
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
		if r.ID != 0 {
			return errors.New("id is a GitLab project id")
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
	for _, part := range strings.Split(r.Project, "/") {
		if part == "" || strings.TrimSpace(part) != part {
			return fmt.Errorf("project %q has an empty or padded segment", r.Project)
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
	}
	for i := range r.Attachments {
		if err := r.Attachments[i].normalize(r.Forge); err != nil {
			return fmt.Errorf("attachments[%d]: %w", i, err)
		}
	}
	return nil
}

func (p *Pull) normalize(forge string, ids *idSource) error {
	if p.Number <= 0 {
		return errors.New("number must be positive")
	}
	if strings.TrimSpace(p.Title) == "" {
		return errors.New("title is required")
	}
	switch p.State {
	case "":
		p.State = "open"
	case "open", "closed", "merged":
	default:
		return fmt.Errorf("state %q is not open, closed or merged", p.State)
	}
	switch p.Mergeable {
	case "":
		p.Mergeable = "clean"
	case "clean", "conflicts", "checking":
	default:
		return fmt.Errorf("mergeable %q is not clean, conflicts or checking", p.Mergeable)
	}
	p.Author = defaultString(p.Author, "ao-author")
	p.HeadRef = defaultString(p.HeadRef, "feature")
	p.BaseRef = defaultString(p.BaseRef, "main")
	p.HeadSHA = defaultString(p.HeadSHA, ids.sha())
	p.BaseSHA = defaultString(p.BaseSHA, ids.sha())
	p.StartSHA = defaultString(p.StartSHA, p.BaseSHA)
	for i := range p.Comments {
		if err := p.Comments[i].normalize(p.Author, ids); err != nil {
			return fmt.Errorf("comments[%d]: %w", i, err)
		}
	}
	for i := range p.Threads {
		thread := &p.Threads[i]
		if err := thread.normalize(forge, p.Author, ids); err != nil {
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
		p.Reviews[i].SubmittedAt = defaultString(review.SubmittedAt, "2026-01-01T00:00:00Z")
		p.Reviews[i].CommitSHA = defaultString(review.CommitSHA, p.HeadSHA)
	}
	if p.CI != nil {
		if err := p.CI.normalize(ids); err != nil {
			return fmt.Errorf("ci: %w", err)
		}
	}
	return nil
}

func (c *Comment) normalize(author string, ids *idSource) error {
	if c.ID == 0 {
		c.ID = ids.next()
	}
	c.Author = defaultString(c.Author, author)
	c.CreatedAt = defaultString(c.CreatedAt, "2026-01-01T00:00:00Z")
	return nil
}

func (t *Thread) normalize(forge, author string, ids *idSource) error {
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
		if err := t.Comments[i].normalize(author, ids); err != nil {
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
		for j, step := range job.Steps {
			if step.Name == "" || !jobStatuses[step.Status] {
				return fmt.Errorf("jobs[%d].steps[%d]: name and a job status are required", i, j)
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
