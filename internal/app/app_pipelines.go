package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
)

// The Pipelines tab's plain reads: a project's pipelines on any ref, and
// the stage summaries of the rows on screen. The page refreshes them
// while visible; the selected pipeline is a pipeline pump's
// (app_pipeline_pump.go).

// PipelineListResult answers ListProjectPipelines: the rows, most recent
// first, and when the forge answered (RFC 3339).
type PipelineListResult struct {
	Rows      []gitops.PipelineSummary `json:"rows"`
	FetchedAt string                   `json:"fetchedAt"`
}

// ListProjectPipelines lists the pipelines of projectID's forge repository
// that filter admits (git.PipelineListFilter). A failure is the forge's,
// explained for the project.
//
//ao:scope git:operate
func (a *App) ListProjectPipelines(ctx context.Context, projectID string, filter gitops.PipelineListFilter) (PipelineListResult, error) {
	if a.shuttingDown.Load() {
		return PipelineListResult{}, ErrShuttingDown
	}
	repo, err := a.projectForgeRepository(ctx, projectID)
	if err != nil {
		return PipelineListResult{}, err
	}
	rows, err := a.gitCore().ListPipelines(forgeapi.WithInteractive(ctx), repo, filter)
	if err != nil {
		return PipelineListResult{}, gitops.ExplainRepoForgeError(repo, gitops.ActionRead, err)
	}
	if rows == nil {
		rows = []gitops.PipelineSummary{}
	}
	return PipelineListResult{Rows: rows, FetchedAt: time.Now().UTC().Format(time.RFC3339)}, nil
}

// maxPipelineStageReads bounds one ReadPipelineStages: a screen of rows.
// pipelineStageReadConcurrency bounds the reads it runs at once.
const (
	maxPipelineStageReads        = 30
	pipelineStageReadConcurrency = 4
)

// PipelineStages is one pipeline's stage row for ReadPipelineStages: each
// stage's name and status, or the read's failure (Error, the explained
// sentence, with ErrorKind), so one pipeline that went away does not blank
// the rest.
type PipelineStages struct {
	Stages    []gitops.CIStage `json:"stages"`
	Error     string           `json:"error,omitempty"`
	ErrorKind string           `json:"errorKind,omitempty"`
}

// ReadPipelineStages reads the stages of up to maxPipelineStageReads
// pipelines of repo, keyed by id: names and statuses only, the jobs
// dropped. The reads are GETs the transport revalidates with the ETag it
// holds, so an unchanged pipeline costs a 304.
//
//ao:scope git:operate
//ao:route selected
func (a *App) ReadPipelineStages(ctx context.Context, repo gitops.RepoReference, ids []string) (map[string]PipelineStages, error) {
	if a.shuttingDown.Load() {
		return nil, ErrShuttingDown
	}
	if err := repo.Validate(); err != nil {
		return nil, err
	}
	if len(ids) > maxPipelineStageReads {
		return nil, fmt.Errorf("read pipeline stages: %d pipelines asked, at most %d", len(ids), maxPipelineStageReads)
	}
	for _, id := range ids {
		if err := gitops.ValidateCIJobID(id); err != nil {
			return nil, fmt.Errorf("pipeline id: %w", err)
		}
	}
	ctx = forgeapi.WithInteractive(ctx)
	rows := make([]PipelineStages, len(ids))
	runBounded(len(ids), pipelineStageReadConcurrency, func(i int) {
		pipeline, err := a.fetchPipeline(ctx, repo, ids[i], nil, nil)
		row := PipelineStages{Stages: []gitops.CIStage{}}
		if err != nil {
			explained := gitops.ExplainRepoForgeError(repo, gitops.ActionRead, err)
			row.Error = explained.Error()
			if forgeErr, ok := errors.AsType[*gitops.ForgeActionError](explained); ok {
				row.ErrorKind = forgeErr.Kind
			}
		}
		for _, stage := range pipeline.Stages {
			row.Stages = append(row.Stages, gitops.CIStage{Name: stage.Name, Status: stage.Status, Jobs: []gitops.CIJob{}})
		}
		rows[i] = row
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]PipelineStages, len(ids))
	for i, id := range ids {
		out[id] = rows[i]
	}
	return out, nil
}
