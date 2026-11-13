package forgefake

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// CI actions: what retry, cancel and play do to a pipeline's jobs on both
// forges, the artifacts they serve, and AdvanceJob, which moves a job to
// a later state the way its runner would, for a spec that watches a
// retried job finish. The forge-specific routes are in
// ci_actions_github.go and ci_actions_gitlab.go.

// Artifact is a GitLab build's artifacts archive (Job.Artifacts) or one
// artifact of a GitHub run (Pipeline.Artifacts), served as the zip the
// forge would serve. Base64 or Text is the content, exactly one set.
type Artifact struct {
	// ID is a GitHub artifact's id, generated when omitted; a GitLab
	// archive has none.
	ID int64 `json:"id,omitempty"`
	// Name is the GitHub artifact's name; a GitLab archive is
	// artifacts.zip.
	Name   string `json:"name,omitempty"`
	Base64 string `json:"base64,omitempty"`
	Text   string `json:"text,omitempty"`
	// Expired is a GitHub artifact past its retention: listed expired,
	// its download a 410.
	Expired bool `json:"expired,omitempty"`

	content []byte
}

func (a *Artifact) normalize() error {
	switch {
	case a.Base64 != "" && a.Text != "":
		return errors.New("an artifact sets base64 or text, not both")
	case a.Base64 != "":
		content, err := base64.StdEncoding.DecodeString(a.Base64)
		if err != nil {
			return fmt.Errorf("artifact base64: %w", err)
		}
		a.content = content
	default:
		a.content = []byte(a.Text)
	}
	return nil
}

func (job *Job) normalizeArtifacts() error {
	if job.Artifacts == nil {
		return nil
	}
	if job.Kind != "" {
		return fmt.Errorf("a %s has no artifacts", job.Kind)
	}
	if job.Artifacts.ID != 0 || job.Artifacts.Expired {
		return errors.New("a GitLab artifacts archive has no id and does not expire here")
	}
	job.Artifacts.Name = defaultString(job.Artifacts.Name, "artifacts.zip")
	return job.Artifacts.normalize()
}

func (p *Pipeline) normalizeArtifacts(ids *idSource) error {
	for i := range p.Artifacts {
		artifact := &p.Artifacts[i]
		if artifact.Name == "" {
			return fmt.Errorf("artifacts[%d]: a GitHub artifact has a name", i)
		}
		if artifact.ID == 0 {
			artifact.ID = ids.next()
		}
		if err := artifact.normalize(); err != nil {
			return fmt.Errorf("artifacts[%d]: %w", i, err)
		}
	}
	return nil
}

// normalizeForgeCI refuses the CI fields the pipeline's forge does not
// have: manual jobs and job archives are GitLab's, run artifacts
// GitHub's.
func (p *Pipeline) normalizeForgeCI(forge string) error {
	if forge == "github" {
		for i, job := range p.Jobs {
			if job.Status == "manual" {
				return fmt.Errorf("jobs[%d]: a manual job is GitLab's", i)
			}
			if job.Artifacts != nil {
				return fmt.Errorf("jobs[%d]: GitHub keeps artifacts per run (pipeline artifacts)", i)
			}
		}
		return nil
	}
	if len(p.Artifacts) > 0 {
		return errors.New("GitLab keeps artifacts per job (job artifacts)")
	}
	return nil
}

// current is the jobs the reads answer: every job no retry replaced.
func (p *Pipeline) current() []*Job {
	out := make([]*Job, 0, len(p.Jobs))
	for i := range p.Jobs {
		if !p.Jobs[i].Retried {
			out = append(out, &p.Jobs[i])
		}
	}
	return out
}

func jobLive(status string) bool { return status == "running" || status == "pending" }

// live reports a pipeline the forge would call running: its own status
// or a current job's.
func (p *Pipeline) live() bool {
	if jobLive(p.Status) {
		return true
	}
	return slices.ContainsFunc(p.current(), func(job *Job) bool { return jobLive(job.Status) })
}

// refreshStatus sets the pipeline's status from its current jobs after an
// action moved one: running, then pending, while any job is; then failed
// for a failed job that may not fail, canceled for a canceled one, and
// success. A manual or skipped job does not move it.
func (p *Pipeline) refreshStatus() {
	has := map[string]bool{}
	for _, job := range p.current() {
		if job.Status == "failed" && job.AllowFailure {
			continue
		}
		has[job.Status] = true
	}
	for _, status := range []string{"running", "pending", "failed", "canceled"} {
		if has[status] {
			p.Status = status
			return
		}
	}
	p.Status = "success"
}

// jobIn finds a job by id and the pipeline holding it.
func (r *Repo) jobIn(id int64) (*Pipeline, *Job) {
	for _, pipeline := range r.pipelines() {
		for j := range pipeline.Jobs {
			if pipeline.Jobs[j].ID == id {
				return pipeline, &pipeline.Jobs[j]
			}
		}
	}
	return nil, nil
}

// retryJobs retries each current job keep selects, in order (retryJob).
// It collects their positions first: a retry appends to the pipeline's
// jobs, which moves them.
func (e *Engine) retryJobs(pipeline *Pipeline, keep func(*Job) bool) int {
	var picked []int
	for i := range pipeline.Jobs {
		if job := &pipeline.Jobs[i]; !job.Retried && keep(job) {
			picked = append(picked, i)
		}
	}
	for _, i := range picked {
		e.retryJob(pipeline, &pipeline.Jobs[i])
	}
	return len(picked)
}

// retryJob replaces job with a new pending attempt under a new id, as
// both forges retry: the old job drops out of the reads, the new one has
// no log, steps or artifacts yet. It returns the new job.
func (e *Engine) retryJob(pipeline *Pipeline, job *Job) Job {
	job.Retried = true
	next := Job{
		ID:           e.ids.next(),
		Name:         job.Name,
		Stage:        job.Stage,
		Status:       "pending",
		AllowFailure: job.AllowFailure,
	}
	pipeline.Jobs = append(pipeline.Jobs, next)
	return next
}

// cancelJob ends a live job canceled.
func cancelJob(job *Job) {
	job.Status = "canceled"
	if job.StartedAt != "" {
		job.CompletedAt = actionTime()
	}
}

// actionTime is when an action happens, as both forges' REST answers
// spell it.
func actionTime() string { return time.Now().UTC().Format(time.RFC3339) }

// JobAdvance moves one current job of a repository to a later state, as
// its runner would (Engine.AdvanceJob). Job names the job: the current
// attempt of that name, or the one with id ID when set. Status is its new
// status; Log, when set, replaces its log; Artifacts, when set, gives a
// GitLab build its archive.
type JobAdvance struct {
	Forge     string    `json:"forge"`
	Project   string    `json:"project"`
	Job       string    `json:"job,omitempty"`
	ID        int64     `json:"id,omitempty"`
	Status    string    `json:"status"`
	Log       *string   `json:"log,omitempty"`
	Artifacts *Artifact `json:"artifacts,omitempty"`
}

// AdvanceJob applies a JobAdvance and returns the job as it is now. A job
// leaving pending or manual starts now; one reaching a terminal status
// completes now. The pipeline's status follows its jobs.
func (e *Engine) AdvanceJob(advance JobAdvance) (Job, error) {
	if !jobStatuses[advance.Status] {
		return Job{}, fmt.Errorf("status %q is not a job status", advance.Status)
	}
	if (advance.Job == "") == (advance.ID == 0) {
		return Job{}, errors.New("advance names a job by name or by id")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.repo(advance.Forge, advance.Project)
	if r == nil {
		return Job{}, fmt.Errorf("no %s repository %s is seeded", advance.Forge, advance.Project)
	}
	var pipeline *Pipeline
	var job *Job
	for _, candidate := range r.pipelines() {
		for _, current := range candidate.current() {
			if (advance.ID != 0 && current.ID == advance.ID) || (advance.ID == 0 && strings.EqualFold(current.Name, advance.Job) && (job == nil || current.ID > job.ID)) {
				pipeline, job = candidate, current
			}
		}
	}
	if job == nil {
		return Job{}, fmt.Errorf("no current job %q (id %d) in %s", advance.Job, advance.ID, advance.Project)
	}
	if advance.Status == "manual" && r.Forge != "gitlab" {
		return Job{}, errors.New("a manual job is GitLab's")
	}
	if advance.Artifacts != nil {
		if r.Forge != "gitlab" || job.Kind != "" {
			return Job{}, errors.New("a job archive is a GitLab build's")
		}
		artifacts := *advance.Artifacts
		artifacts.Name = defaultString(artifacts.Name, "artifacts.zip")
		if err := artifacts.normalize(); err != nil {
			return Job{}, err
		}
		job.Artifacts = &artifacts
	}
	now := actionTime()
	if job.StartedAt == "" && advance.Status != "pending" && advance.Status != "manual" && advance.Status != "skipped" {
		job.StartedAt = now
	}
	job.Status = advance.Status
	if !jobLive(job.Status) && job.Status != "manual" && job.StartedAt != "" {
		job.CompletedAt = now
	} else {
		job.CompletedAt = ""
	}
	if advance.Log != nil {
		job.Log = *advance.Log
	}
	pipeline.refreshStatus()
	return *job, nil
}
