package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
)

// Acting on CI: retry a job, retry a pipeline's failed jobs, cancel a job
// (GitLab) or a pipeline, run a manual job (GitLab), and download
// artifacts. The read side is CIJob.Actions and CIPipeline.Actions, filled
// by each forge's read from what that read already carries; the writes
// are ForgeCIActor. See docs/specs/pull-requests.md and
// docs/architecture/forge-transport.md.

// CIAction is whether one action applies to a job or pipeline and whether
// the viewer may take it. Empty: the action does not apply, and the UI
// shows nothing for it. CIActionAllowed: it applies and the forge reports
// nothing against the viewer taking it. CIActionDenied: it applies, but
// the forge reports the viewer may not take it (GitLab's job and pipeline
// permissions, GitHub's repository permission).
type CIAction string

const (
	CIActionAllowed CIAction = "allowed"
	CIActionDenied  CIAction = "denied"
)

// ciPermitted is the action's state once it applies.
func ciPermitted(permitted bool) CIAction {
	if permitted {
		return CIActionAllowed
	}
	return CIActionDenied
}

// CIJobActions are the job-level actions. Retry applies to a job that
// finished failed or canceled (on GitHub, once its whole workflow run
// completed, since GitHub re-runs a job of a completed run only); Cancel
// to a running or pending GitLab job; Play to a GitLab job waiting for a
// manual start; Artifacts to a GitLab job with an artifacts archive.
// GitHub cancels runs, not jobs, starts no manual jobs and keeps
// artifacts per run, so a GitHub job carries Retry alone. A job with no
// ID (a GitHub external check, a GitLab bridge or commit status) carries
// none.
type CIJobActions struct {
	Retry     CIAction `json:"retry,omitempty"`
	Cancel    CIAction `json:"cancel,omitempty"`
	Play      CIAction `json:"play,omitempty"`
	Artifacts CIAction `json:"artifacts,omitempty"`
}

// CIPipelineActions are the pipeline-level actions. RetryFailed applies
// to a finished pipeline with a failed or canceled job, Cancel to a
// running or pending one. A GitHub pipeline is every workflow run its
// jobs belong to: RetryFailed applies when a completed run holds a failed
// or canceled job, Cancel when a run is still live, and each acts on the
// runs that qualify. Artifacts is GitHub's alone (artifacts belong to a
// run): a completed run of the pipeline has artifacts; GitLab's are per
// job.
type CIPipelineActions struct {
	RetryFailed CIAction `json:"retryFailed,omitempty"`
	Cancel      CIAction `json:"cancel,omitempty"`
	Artifacts   CIAction `json:"artifacts,omitempty"`
}

// CIActionKind names one write ForgeCIActor performs.
type CIActionKind string

const (
	CIActionRetryJob       CIActionKind = "retry_job"
	CIActionCancelJob      CIActionKind = "cancel_job"
	CIActionPlayJob        CIActionKind = "play_job"
	CIActionRetryFailed    CIActionKind = "retry_failed"
	CIActionCancelPipeline CIActionKind = "cancel_pipeline"
)

// JobScoped reports whether the action names a job.
func (k CIActionKind) JobScoped() bool {
	return k == CIActionRetryJob || k == CIActionCancelJob || k == CIActionPlayJob
}

// Validate refuses a kind outside the set.
func (k CIActionKind) Validate() error {
	switch k {
	case CIActionRetryJob, CIActionCancelJob, CIActionPlayJob, CIActionRetryFailed, CIActionCancelPipeline:
		return nil
	}
	return fmt.Errorf("unknown CI action %q", k)
}

// State is the action's state on the job or pipeline as a read reported
// it: the field of CIJobActions or CIPipelineActions the kind reads.
func (k CIActionKind) State(job *CIJob, pipeline CIPipeline) CIAction {
	switch k {
	case CIActionRetryJob:
		if job != nil {
			return job.Actions.Retry
		}
	case CIActionCancelJob:
		if job != nil {
			return job.Actions.Cancel
		}
	case CIActionPlayJob:
		if job != nil {
			return job.Actions.Play
		}
	case CIActionRetryFailed:
		return pipeline.Actions.RetryFailed
	case CIActionCancelPipeline:
		return pipeline.Actions.Cancel
	}
	return ""
}

// ForgeAction is the verb the error sentences use.
func (k CIActionKind) ForgeAction() ForgeAction {
	switch k {
	case CIActionRetryJob:
		return ActionRetryJob
	case CIActionCancelJob, CIActionCancelPipeline:
		return ActionCancelCI
	case CIActionPlayJob:
		return ActionPlayJob
	case CIActionRetryFailed:
		return ActionRetryFailed
	}
	return ForgeAction(k)
}

// The CI action verbs of the error sentences ("GitHub did not let you
// retry job lint of o/r.").
const (
	ActionRetryJob    ForgeAction = "retry"
	ActionRetryFailed ForgeAction = "retry the failed jobs of"
	ActionCancelCI    ForgeAction = "cancel"
	ActionPlayJob     ForgeAction = "run"
	ActionArtifacts   ForgeAction = "download the artifacts of"
)

// CIArtifact is one artifact a download saves: a GitLab job's archive
// (JobID, Size unknown, 0) or one artifact of a GitHub workflow run (ID,
// RunID, Size as the list reports it). Name is the forge's name for it.
type CIArtifact struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	JobID   string `json:"jobId,omitempty"`
	RunID   string `json:"runId,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Expired bool   `json:"expired,omitempty"`
}

// CIArtifactsRequest names whose artifacts to download: one GitLab job's
// archive (JobID), or every artifact of the GitHub workflow runs RunIDs.
type CIArtifactsRequest struct {
	JobID  string   `json:"jobId,omitempty"`
	RunIDs []string `json:"runIds,omitempty"`
}

// maxCIArtifactRuns bounds the runs one download lists, the runs a
// GitHub PR pipeline reads (githubCIMaxRuns).
const maxCIArtifactRuns = githubCIMaxRuns

// Validate checks the request's ids before any is spliced into a path.
func (r CIArtifactsRequest) Validate() error {
	switch {
	case r.JobID != "" && len(r.RunIDs) > 0:
		return errors.New("an artifacts download names a job or runs, not both")
	case r.JobID != "":
		return ValidateCIJobID(r.JobID)
	case len(r.RunIDs) == 0:
		return errors.New("an artifacts download names a job or runs")
	case len(r.RunIDs) > maxCIArtifactRuns:
		return fmt.Errorf("an artifacts download names at most %d runs, got %d", maxCIArtifactRuns, len(r.RunIDs))
	}
	for _, id := range r.RunIDs {
		if err := ValidateCIJobID(id); err != nil {
			return fmt.Errorf("run id: %w", err)
		}
	}
	return nil
}

// ErrCIArtifactExpired is an artifact the forge no longer serves (GitHub
// answers 410 Gone, and lists it expired).
var ErrCIArtifactExpired = errors.New("the forge no longer keeps this artifact")

// ErrNoCIArtifacts is a download that found nothing to save.
var ErrNoCIArtifacts = errors.New("there are no artifacts to download")

// CIPartialError is a pipeline action that acts on several GitHub
// workflow runs and failed on some after succeeding on others: Done of
// Total runs took it, and Err is the first failure.
type CIPartialError struct {
	Done, Total int
	Err         error
}

func (e *CIPartialError) Error() string {
	return fmt.Sprintf("%d of %d workflow runs took the action; the rest failed: %v", e.Done, e.Total, e.Err)
}

func (e *CIPartialError) Unwrap() error { return e.Err }

// ForgeCIActor is the CI-action half of Forge. The writes take an
// interactive context (the caller marks it); a job is named by the id
// its read reported and a pipeline by the read itself, from which a
// GitHub forge takes the workflow runs that qualify (githubCIRuns).
type ForgeCIActor interface {
	RetryCIJob(ctx context.Context, repo RepoReference, jobID string) error
	CancelCIJob(ctx context.Context, repo RepoReference, jobID string) error
	PlayCIJob(ctx context.Context, repo RepoReference, jobID string) error
	RetryCIPipeline(ctx context.Context, repo RepoReference, pipeline CIPipeline) error
	CancelCIPipeline(ctx context.Context, repo RepoReference, pipeline CIPipeline) error
	// ListCIArtifacts lists what a download of req saves.
	ListCIArtifacts(ctx context.Context, repo RepoReference, req CIArtifactsRequest) ([]CIArtifact, error)
	// StreamCIArtifact writes one artifact's archive to dst, failing with
	// forgeapi.ErrBodyTooLarge past limit bytes.
	StreamCIArtifact(ctx context.Context, repo RepoReference, artifact CIArtifact, dst io.Writer, limit int64) error
}

func (nullForge) RetryCIJob(context.Context, RepoReference, string) error { return ErrUnsupportedForge }
func (nullForge) CancelCIJob(context.Context, RepoReference, string) error {
	return ErrUnsupportedForge
}
func (nullForge) PlayCIJob(context.Context, RepoReference, string) error { return ErrUnsupportedForge }
func (nullForge) RetryCIPipeline(context.Context, RepoReference, CIPipeline) error {
	return ErrUnsupportedForge
}
func (nullForge) CancelCIPipeline(context.Context, RepoReference, CIPipeline) error {
	return ErrUnsupportedForge
}
func (nullForge) ListCIArtifacts(context.Context, RepoReference, CIArtifactsRequest) ([]CIArtifact, error) {
	return nil, ErrUnsupportedForge
}
func (nullForge) StreamCIArtifact(context.Context, RepoReference, CIArtifact, io.Writer, int64) error {
	return ErrUnsupportedForge
}

// RunCIAction runs one action on repo's pipeline: a job action on jobID,
// a pipeline action on pipeline. The write outlives ctx's cancellation
// (forgeWrite), as the merge actions' do.
func (c *Core) RunCIAction(ctx context.Context, repo RepoReference, kind CIActionKind, jobID string, pipeline CIPipeline) error {
	if err := repo.Validate(); err != nil {
		return err
	}
	if err := kind.Validate(); err != nil {
		return err
	}
	forge := c.ForgeByID(repo.Forge)
	ctx = forgeWrite(ctx)
	if kind.JobScoped() {
		if err := ValidateCIJobID(jobID); err != nil {
			return err
		}
		switch kind {
		case CIActionRetryJob:
			return forge.RetryCIJob(ctx, repo, jobID)
		case CIActionCancelJob:
			return forge.CancelCIJob(ctx, repo, jobID)
		default:
			return forge.PlayCIJob(ctx, repo, jobID)
		}
	}
	if kind == CIActionRetryFailed {
		return forge.RetryCIPipeline(ctx, repo, pipeline)
	}
	return forge.CancelCIPipeline(ctx, repo, pipeline)
}

// ListCIArtifacts lists what a download of req saves (see
// ForgeCIActor.ListCIArtifacts).
func (c *Core) ListCIArtifacts(ctx context.Context, repo RepoReference, req CIArtifactsRequest) ([]CIArtifact, error) {
	if err := repo.Validate(); err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	return c.ForgeByID(repo.Forge).ListCIArtifacts(ctx, repo, req)
}

// StreamCIArtifact writes one listed artifact to dst (see
// ForgeCIActor.StreamCIArtifact).
func (c *Core) StreamCIArtifact(ctx context.Context, repo RepoReference, artifact CIArtifact, dst io.Writer, limit int64) error {
	if err := repo.Validate(); err != nil {
		return err
	}
	if limit <= 0 {
		return errors.New("an artifact download needs a positive size limit")
	}
	for _, id := range []string{artifact.ID, artifact.JobID, artifact.RunID} {
		if id != "" {
			if err := ValidateCIJobID(id); err != nil {
				return err
			}
		}
	}
	return c.ForgeByID(repo.Forge).StreamCIArtifact(ctx, repo, artifact, dst, limit)
}

// FindCIJobRetry returns the job that took over from old in pipeline: the
// one in the same stage with the same name and the highest id above
// old's, or nil. Both forges give a retried job a new, higher id and keep
// its name and stage (GitHub: the new attempt's job; GitLab: the retry,
// the old job dropping out of the read as retried), so this is how a log
// view open on old follows its retry.
func FindCIJobRetry(pipeline CIPipeline, stage string, old CIJob) *CIJob {
	oldID, err := strconv.ParseInt(old.ID, 10, 64)
	if err != nil {
		return nil
	}
	var found *CIJob
	var foundID int64
	for i := range pipeline.Stages {
		if pipeline.Stages[i].Name != stage {
			continue
		}
		for j := range pipeline.Stages[i].Jobs {
			job := &pipeline.Stages[i].Jobs[j]
			id, err := strconv.ParseInt(job.ID, 10, 64)
			if err != nil || job.Name != old.Name || id <= oldID || (found != nil && id < foundID) {
				continue
			}
			found, foundID = job, id
		}
	}
	return found
}

// ciJobFinishedBad is a job status Retry applies to.
func ciJobFinishedBad(status string) bool {
	return status == CIStatusFailed || status == CIStatusCanceled
}

// ciHasFinishedBadJob reports a failed or canceled job among jobs.
func ciHasFinishedBadJob(jobs []CIJob) bool {
	return slices.ContainsFunc(jobs, func(job CIJob) bool { return job.ID != "" && ciJobFinishedBad(job.Status) })
}
