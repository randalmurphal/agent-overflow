package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/forgeapi"
)

// GitHub Actions' CI actions (ForgeCIActor). GitHub re-runs a job or a
// run's failed jobs and cancels a whole workflow run; it has no job
// cancel and no manual jobs, and keeps artifacts per run. A pipeline may
// span several runs (a PR's head checks), and a pipeline action acts on
// each run that qualifies (githubCIRuns). Verified shapes 2026-11
// (docs.github.com, REST 2022-11-28): rerun and rerun-failed-jobs answer
// 201, cancel 202 and 409 for a run that cannot be canceled, the
// artifact zip a 302 to its blob and 410 once expired.

// ciArtifactTimeout bounds one artifact download, headers and body: an
// archive of ciArtifactMaxBytes at under 2 MB/s.
const ciArtifactTimeout = 10 * time.Minute

// githubRunState is one workflow run of a pipeline as its jobs show it:
// live while a job of it (or, for the run ReadPipeline read, the run
// itself) is queued or running; bad when a job of it finished failed or
// canceled.
type githubRunState struct {
	id        string
	live, bad bool
	jobIDs    []string
}

// githubCIRuns lists a pipeline's workflow runs in the order its jobs
// name them, the run ReadPipeline read (pipeline.ID) included when it
// lists no job yet.
func githubCIRuns(pipeline CIPipeline) []githubRunState {
	var runs []githubRunState
	index := map[string]int{}
	at := func(id string) *githubRunState {
		i, ok := index[id]
		if !ok {
			i = len(runs)
			index[id] = i
			runs = append(runs, githubRunState{id: id})
		}
		return &runs[i]
	}
	if pipeline.ID != "" {
		at(pipeline.ID).live = CIJobLive(pipeline.Status)
	}
	for _, stage := range pipeline.Stages {
		for _, job := range stage.Jobs {
			if job.RunID == "" || job.ID == "" {
				continue
			}
			run := at(job.RunID)
			run.jobIDs = append(run.jobIDs, job.ID)
			run.live = run.live || CIJobLive(job.Status)
			run.bad = run.bad || ciJobFinishedBad(job.Status)
		}
	}
	return runs
}

// fillGitHubCIActions sets the actions of a pipeline read: permission is
// the viewer's (githubCIPermission), artifacts the completed runs that
// keep any.
func fillGitHubCIActions(pipeline *CIPipeline, permission CIAction, artifacts map[string]bool) {
	runs := githubCIRuns(*pipeline)
	live := make(map[string]bool, len(runs))
	for _, run := range runs {
		live[run.id] = run.live
		switch {
		case run.live:
			pipeline.Actions.Cancel = permission
		case run.bad:
			pipeline.Actions.RetryFailed = permission
		}
		if artifacts[run.id] {
			pipeline.Actions.Artifacts = CIActionAllowed
		}
	}
	for i := range pipeline.Stages {
		for j := range pipeline.Stages[i].Jobs {
			job := &pipeline.Stages[i].Jobs[j]
			if job.RunID != "" && job.ID != "" && !live[job.RunID] && ciJobFinishedBad(job.Status) {
				job.Actions.Retry = permission
			}
		}
	}
}

// githubCIPermission reads GraphQL's viewerPermission: write or above may
// re-run and cancel. A GitHub App token reads null, and the forge decides.
func githubCIPermission(viewerPermission string) CIAction {
	switch strings.ToUpper(viewerPermission) {
	case "", "WRITE", "MAINTAIN", "ADMIN":
		return CIActionAllowed
	}
	return CIActionDenied
}

// repoCIPermission is the viewer's permission on repo from the REST
// repository (permissions.push), read at most once per login per
// gitlabCacheTTL and published under the credential that got the answer,
// for ReadPipeline, whose REST reads carry none. A token whose answer has
// no permissions (a GitHub App's) is allowed, and the forge decides.
func (f *githubForge) repoCIPermission(ctx context.Context, client *forgeapi.Client, repo RepoReference) (CIAction, error) {
	fp, err := client.CredentialFingerprint(ctx)
	if err != nil {
		return "", err
	}
	key := func(fp string) string { return fp + " " + repo.Host + " " + repo.Project() }
	if permission, ok := f.ciPermissions.get(key(fp), f.core.nowFn()); ok {
		return permission, nil
	}
	var raw struct {
		Permissions *struct {
			Admin    bool `json:"admin"`
			Maintain bool `json:"maintain"`
			Push     bool `json:"push"`
		} `json:"permissions"`
	}
	resp, err := client.JSON(ctx, forgeapi.Request{Path: githubRepoPath(repo)}, &raw)
	if err != nil {
		return "", err
	}
	permission := CIActionAllowed
	if p := raw.Permissions; p != nil && !p.Admin && !p.Maintain && !p.Push {
		permission = CIActionDenied
	}
	f.ciPermissions.put(key(resp.Credential), permission, f.core.nowFn())
	return permission, nil
}

// githubArtifactRaw is one artifact of the REST artifacts list.
type githubArtifactRaw struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	SizeInBytes int64  `json:"size_in_bytes"`
	Expired     bool   `json:"expired"`
}

// readGitHubRunArtifacts reads a run's artifacts, 100 per request, at
// most githubReadMaxPages; first stops after the first page.
func readGitHubRunArtifacts(ctx context.Context, client *forgeapi.Client, repo RepoReference, runID string, first bool) ([]CIArtifact, error) {
	request := forgeapi.Request{
		Path:  githubRepoPath(repo) + "/actions/runs/" + runID + "/artifacts",
		Query: url.Values{"per_page": {strconv.Itoa(githubCIJobsPerPage)}},
	}
	var out []CIArtifact
	pages := 0
	err := client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		var raw struct {
			TotalCount int                 `json:"total_count"`
			Artifacts  []githubArtifactRaw `json:"artifacts"`
		}
		if err := json.Unmarshal(resp.Body, &raw); err != nil {
			return false, fmt.Errorf("GitHub run %s artifacts: decode response: %w", runID, err)
		}
		for _, artifact := range raw.Artifacts {
			out = append(out, CIArtifact{ID: strconv.FormatInt(artifact.ID, 10), Name: artifact.Name, RunID: runID, Size: artifact.SizeInBytes, Expired: artifact.Expired})
		}
		pages++
		return !first && len(raw.Artifacts) == githubCIJobsPerPage && len(out) < raw.TotalCount && pages < githubReadMaxPages, nil
	})
	return out, err
}

// runArtifacts answers which completed runs keep an artifact that has not
// expired, from one list page per run, remembered per run and job set (a
// re-run's new jobs read it again) for gitlabCacheTTL; after that the
// read goes to the transport, whose ETag store revalidates it. A live run
// is not asked: its artifacts are still being uploaded.
func (f *githubForge) runArtifacts(ctx context.Context, client *forgeapi.Client, repo RepoReference, pipeline CIPipeline) (map[string]bool, error) {
	out := map[string]bool{}
	for _, run := range githubCIRuns(pipeline) {
		if run.live {
			continue
		}
		ids := slices.Clone(run.jobIDs)
		slices.Sort(ids)
		key := repo.Host + " " + repo.Project() + " " + run.id + " " + strings.Join(ids, ",")
		if has, ok := f.ciArtifacts.get(key, f.core.nowFn()); ok {
			out[run.id] = has
			continue
		}
		artifacts, err := readGitHubRunArtifacts(ctx, client, repo, run.id, true)
		if err != nil {
			return nil, err
		}
		has := slices.ContainsFunc(artifacts, func(a CIArtifact) bool { return !a.Expired })
		f.ciArtifacts.put(key, has, f.core.nowFn())
		out[run.id] = has
	}
	return out, nil
}

func (f *githubForge) ciClient(repo RepoReference) (*forgeapi.Client, error) {
	if _, _, err := splitGitHubProject(repo.Project()); err != nil {
		return nil, err
	}
	return f.core.githubAPI(repo.Host)
}

// RetryCIJob re-runs one job of a completed run: a new attempt of the
// run whose job has a new id.
func (f *githubForge) RetryCIJob(ctx context.Context, repo RepoReference, jobID string) error {
	client, err := f.ciClient(repo)
	if err != nil {
		return err
	}
	_, err = client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: githubRepoPath(repo) + "/actions/jobs/" + jobID + "/rerun"}, nil)
	return err
}

// CancelCIJob is not offered: GitHub cancels runs, not jobs.
func (f *githubForge) CancelCIJob(context.Context, RepoReference, string) error {
	return errors.New("GitHub cancels workflow runs, not jobs")
}

// PlayCIJob is not offered: GitHub has no manual jobs.
func (f *githubForge) PlayCIJob(context.Context, RepoReference, string) error {
	return errors.New("GitHub has no manual jobs")
}

// RetryCIPipeline re-runs the failed jobs of every completed run of the
// pipeline holding a failed or canceled job.
func (f *githubForge) RetryCIPipeline(ctx context.Context, repo RepoReference, pipeline CIPipeline) error {
	var runs []string
	for _, run := range githubCIRuns(pipeline) {
		if !run.live && run.bad {
			runs = append(runs, run.id)
		}
	}
	if len(runs) == 0 {
		return errors.New("no completed workflow run of this pipeline has a failed job")
	}
	return f.eachRun(ctx, repo, runs, "/rerun-failed-jobs")
}

// CancelCIPipeline cancels every live run of the pipeline.
func (f *githubForge) CancelCIPipeline(ctx context.Context, repo RepoReference, pipeline CIPipeline) error {
	var runs []string
	for _, run := range githubCIRuns(pipeline) {
		if run.live {
			runs = append(runs, run.id)
		}
	}
	if len(runs) == 0 {
		return errors.New("no workflow run of this pipeline is running")
	}
	return f.eachRun(ctx, repo, runs, "/cancel")
}

// eachRun posts one run action to each run in turn. Every run is tried; a
// failure after another run took the action is a *CIPartialError, and
// when none took it the first failure is the answer.
func (f *githubForge) eachRun(ctx context.Context, repo RepoReference, runs []string, action string) error {
	client, err := f.ciClient(repo)
	if err != nil {
		return err
	}
	done := 0
	var first error
	for _, run := range runs {
		_, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: githubRepoPath(repo) + "/actions/runs/" + run + action}, nil)
		switch {
		case err == nil:
			done++
		case first == nil:
			first = fmt.Errorf("workflow run %s: %w", run, err)
		}
	}
	switch {
	case first == nil:
		return nil
	case done == 0:
		return first
	}
	return &CIPartialError{Done: done, Total: len(runs), Err: first}
}

// ListCIArtifacts lists the artifacts of each run of req that GitHub
// still keeps. A run with none adds nothing; none at all is
// ErrNoCIArtifacts, only expired ones ErrCIArtifactExpired.
func (f *githubForge) ListCIArtifacts(ctx context.Context, repo RepoReference, req CIArtifactsRequest) ([]CIArtifact, error) {
	if req.JobID != "" {
		return nil, errors.New("GitHub keeps artifacts per workflow run, not per job")
	}
	client, err := f.ciClient(repo)
	if err != nil {
		return nil, err
	}
	var out []CIArtifact
	expired := false
	for _, run := range req.RunIDs {
		artifacts, err := readGitHubRunArtifacts(ctx, client, repo, run, false)
		if err != nil {
			return nil, err
		}
		for _, artifact := range artifacts {
			if artifact.Expired {
				expired = true
				continue
			}
			out = append(out, artifact)
		}
	}
	switch {
	case len(out) > 0:
		return out, nil
	case expired:
		return nil, ErrCIArtifactExpired
	}
	return nil, ErrNoCIArtifacts
}

// StreamCIArtifact downloads one artifact's zip. The endpoint redirects to
// its blob, which the transport follows without the token; a 410 is
// ErrCIArtifactExpired. An artifact the list sized past limit is refused
// before any request.
func (f *githubForge) StreamCIArtifact(ctx context.Context, repo RepoReference, artifact CIArtifact, dst io.Writer, limit int64) error {
	if artifact.ID == "" {
		return errors.New("a GitHub artifact download names the artifact")
	}
	if artifact.Size > limit {
		return fmt.Errorf("artifact %s: %w (%d bytes, limit %d)", artifact.Name, forgeapi.ErrBodyTooLarge, artifact.Size, limit)
	}
	client, err := f.ciClient(repo)
	if err != nil {
		return err
	}
	_, err = client.Stream(ctx, forgeapi.Request{Path: githubRepoPath(repo) + "/actions/artifacts/" + artifact.ID + "/zip", Timeout: ciArtifactTimeout}, dst, limit)
	var status *forgeapi.StatusError
	if errors.As(err, &status) && status.Status == http.StatusGone {
		return fmt.Errorf("%w: %w", ErrCIArtifactExpired, err)
	}
	return err
}
