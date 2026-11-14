package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"agent-overflow/internal/forgeapi"
)

// GitLab CI's actions (ForgeCIActor), on the REST API: a job's retry
// (201 with the new job), cancel and play, a pipeline's retry of its
// failed jobs and cancel, and a job's artifacts archive. GitLab refuses a
// job it cannot retry with 403 "Job is not retryable" and a job it cannot
// play with 400 "Unplayable Job" (docs.gitlab.com, jobs and pipelines
// API).

// jobPost posts one CI write, once; an error that does not prove GitLab
// refused it is an unconfirmed write (ciWriteResult).
func (f *gitlabForge) jobPost(ctx context.Context, repo RepoReference, path string) error {
	client, err := f.core.gitlabAPI(repo.Host)
	if err != nil {
		return err
	}
	_, err = client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: gitlabProjectPath(repo.Project()) + path}, nil)
	return ciWriteResult(err)
}

func (f *gitlabForge) RetryCIJob(ctx context.Context, repo RepoReference, jobID string) error {
	return f.jobPost(ctx, repo, "/jobs/"+jobID+"/retry")
}

func (f *gitlabForge) CancelCIJob(ctx context.Context, repo RepoReference, jobID string) error {
	return f.jobPost(ctx, repo, "/jobs/"+jobID+"/cancel")
}

func (f *gitlabForge) PlayCIJob(ctx context.Context, repo RepoReference, jobID string) error {
	return f.jobPost(ctx, repo, "/jobs/"+jobID+"/play")
}

func (f *gitlabForge) RetryCIPipeline(ctx context.Context, repo RepoReference, pipeline CIPipeline) error {
	if err := ValidateCIJobID(pipeline.ID); err != nil {
		return fmt.Errorf("pipeline id: %w", err)
	}
	return f.jobPost(ctx, repo, "/pipelines/"+pipeline.ID+"/retry")
}

func (f *gitlabForge) CancelCIPipeline(ctx context.Context, repo RepoReference, pipeline CIPipeline) error {
	if err := ValidateCIJobID(pipeline.ID); err != nil {
		return fmt.Errorf("pipeline id: %w", err)
	}
	return f.jobPost(ctx, repo, "/pipelines/"+pipeline.ID+"/cancel")
}

// ListCIArtifacts answers a job's archive without a request: the read
// that offered the download saw it among the job's artifacts.
func (f *gitlabForge) ListCIArtifacts(_ context.Context, _ RepoReference, req CIArtifactsRequest) ([]CIArtifact, error) {
	if req.JobID == "" {
		return nil, errors.New("GitLab keeps artifacts per job")
	}
	return []CIArtifact{{Name: "artifacts.zip", JobID: req.JobID}}, nil
}

// StreamCIArtifact downloads a job's artifacts archive. A 404 (the job
// has none, or they expired) is ErrNoCIArtifacts.
func (f *gitlabForge) StreamCIArtifact(ctx context.Context, repo RepoReference, artifact CIArtifact, dst io.Writer, limit int64) error {
	if artifact.JobID == "" {
		return errors.New("a GitLab artifact download names the job")
	}
	client, err := f.core.gitlabAPI(repo.Host)
	if err != nil {
		return err
	}
	request := forgeapi.Request{Path: gitlabProjectPath(repo.Project()) + "/jobs/" + artifact.JobID + "/artifacts", Header: http.Header{"Accept": {"*/*"}}, Timeout: ciArtifactTimeout}
	_, err = client.Stream(ctx, request, dst, limit)
	if errors.Is(err, forgeapi.ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrNoCIArtifacts, err)
	}
	return err
}
