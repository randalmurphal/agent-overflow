package git

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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

// githubRerunWindow is how long after a workflow run was created GitHub
// re-runs it or its jobs ("You can re-run a workflow run up to 30 days
// after the initial run", GitHub Actions documentation, Re-running
// workflows and jobs).
const githubRerunWindow = 30 * 24 * time.Hour

// fillGitHubCIActions sets the actions of a pipeline read: permission is
// the viewer's (githubCIPermission; empty when it could not be read,
// which leaves every permission-gated action absent), artifacts the
// completed runs that keep any, and stale the runs past
// githubRerunWindow, which GitHub no longer re-runs.
func fillGitHubCIActions(pipeline *CIPipeline, permission CIAction, artifacts, stale map[string]bool) {
	runs := githubCIRuns(*pipeline)
	live := make(map[string]bool, len(runs))
	for _, run := range runs {
		live[run.id] = run.live
		switch {
		case run.live:
			pipeline.Actions.Cancel = permission
		case run.bad && !stale[run.id]:
			pipeline.Actions.RetryFailed = permission
		}
		if artifacts[run.id] {
			pipeline.Actions.Artifacts = CIActionAllowed
		}
	}
	for i := range pipeline.Stages {
		for j := range pipeline.Stages[i].Jobs {
			job := &pipeline.Stages[i].Jobs[j]
			if job.RunID != "" && job.ID != "" && !live[job.RunID] && !stale[job.RunID] && ciJobFinishedBad(job.Status) {
				job.Actions.Retry = permission
			}
		}
	}
}

// githubCICapabilities is the permission and artifacts behind a GitHub
// pipeline read's flags, each read from its remembered answer when it has
// one. A failed read leaves its flags absent and is the pipeline's
// ActionsUnavailable sentence, so the jobs the read did get are kept; it
// is logged, and asked again when its backoff is due
// (ciCapabilityCache). Only ctx's own end fails the read.
func (f *githubForge) githubCICapabilities(ctx context.Context, client *forgeapi.Client, repo RepoReference, pipeline *CIPipeline, permission CIAction, readPermission bool, stale map[string]bool) error {
	var failed error
	if readPermission {
		var err error
		if permission, err = f.repoCIPermission(ctx, client, repo); err != nil {
			permission, failed = "", err
		}
	}
	artifacts, err := f.runArtifacts(ctx, client, repo, *pipeline)
	if failed == nil {
		failed = err
	}
	if failed != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		pipeline.ActionsUnavailable = ciActionsUnavailableSentence(repo, failed)
		log.Printf("forge: CI actions of %s on %s unavailable: %v", repo.Project(), repo.Host, failed)
	}
	fillGitHubCIActions(pipeline, permission, artifacts, stale)
	return nil
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
// for ReadPipeline, whose REST reads carry none. A failure is remembered
// until its backoff is due. A token whose answer has
// no permissions (a GitHub App's) is allowed, and the forge decides.
func (f *githubForge) repoCIPermission(ctx context.Context, client *forgeapi.Client, repo RepoReference) (CIAction, error) {
	fp, err := client.CredentialFingerprint(ctx)
	if err != nil {
		return "", err
	}
	key := func(fp string) string { return fp + " " + repo.Host + " " + repo.Project() }
	if permission, err, ok := f.ciPermissions.get(key(fp), f.core.nowFn()); ok {
		return permission, err
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
		if ctx.Err() == nil {
			f.ciPermissions.fail(key(fp), err, f.core.nowFn(), f.core.prLookupErrorDelay)
		}
		return "", err
	}
	permission := CIActionAllowed
	if p := raw.Permissions; p != nil && !p.Admin && !p.Maintain && !p.Push {
		permission = CIActionDenied
	}
	now := f.core.nowFn()
	f.ciPermissions.put(key(resp.Credential), permission, now.Add(gitlabCacheTTL))
	return permission, nil
}

// githubArtifactRaw is one artifact of the REST artifacts list.
type githubArtifactRaw struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	SizeInBytes int64  `json:"size_in_bytes"`
	Expired     bool   `json:"expired"`
	ExpiresAt   string `json:"expires_at"`
}

// readGitHubRunArtifacts reads a run's artifacts, 100 per request, at
// most githubReadMaxPages; untilUsable stops after the page holding the
// first artifact that has not expired. Each artifact's expires_at, when
// GitHub gives one, is in expires.
func readGitHubRunArtifacts(ctx context.Context, client *forgeapi.Client, repo RepoReference, runID string, untilUsable bool) ([]CIArtifact, []time.Time, error) {
	request := forgeapi.Request{
		Path:  githubRepoPath(repo) + "/actions/runs/" + runID + "/artifacts",
		Query: url.Values{"per_page": {strconv.Itoa(githubCIJobsPerPage)}},
	}
	var out []CIArtifact
	var expires []time.Time
	pages := 0
	usable := false
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
			usable = usable || !artifact.Expired
			if at, err := time.Parse(time.RFC3339, artifact.ExpiresAt); err == nil && !artifact.Expired {
				expires = append(expires, at)
			}
		}
		pages++
		return !(untilUsable && usable) && len(raw.Artifacts) == githubCIJobsPerPage && len(out) < raw.TotalCount && pages < githubReadMaxPages, nil
	})
	return out, expires, err
}

// runArtifacts answers which completed runs keep an artifact that has not
// expired, reading each run's list until it finds one or the list ends
// (at most githubReadMaxPages pages, the download's own bound). The
// answer is remembered per run and job set (a re-run's new jobs read it
// again) for gitlabCacheTTL, and never past the earliest expires_at of
// the artifacts it found; after that the read goes to the transport,
// whose ETag store revalidates it. A live run is not asked: its artifacts
// are still being uploaded. A run whose read failed is left out, and the
// first failure is the error beside the answers the others got.
func (f *githubForge) runArtifacts(ctx context.Context, client *forgeapi.Client, repo RepoReference, pipeline CIPipeline) (map[string]bool, error) {
	out := map[string]bool{}
	var failed error
	for _, run := range githubCIRuns(pipeline) {
		if run.live {
			continue
		}
		ids := slices.Clone(run.jobIDs)
		slices.Sort(ids)
		key := githubRunArtifactsKey(repo, run.id) + strings.Join(ids, ",")
		if has, err, ok := f.ciArtifacts.get(key, f.core.nowFn()); ok {
			if err != nil {
				failed = cmp.Or(failed, err)
				continue
			}
			out[run.id] = has
			continue
		}
		artifacts, expires, err := readGitHubRunArtifacts(ctx, client, repo, run.id, true)
		now := f.core.nowFn()
		if err != nil {
			if ctx.Err() != nil {
				return out, err
			}
			f.ciArtifacts.fail(key, err, now, f.core.prLookupErrorDelay)
			failed = cmp.Or(failed, fmt.Errorf("workflow run %s artifacts: %w", run.id, err))
			continue
		}
		has := slices.ContainsFunc(artifacts, func(a CIArtifact) bool { return !a.Expired })
		until := now.Add(gitlabCacheTTL)
		for _, at := range expires {
			if at.Before(until) {
				until = at
			}
		}
		f.ciArtifacts.put(key, has, until)
		out[run.id] = has
	}
	return out, failed
}

// githubRunArtifactsKey is the prefix of a run's remembered artifacts
// answers, one per job set.
func githubRunArtifactsKey(repo RepoReference, runID string) string {
	return repo.Host + " " + repo.Project() + " " + runID + " "
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
	return ciWriteResult(err)
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

// eachRun posts one run action to each run in turn, once. Every run is
// tried, and each outcome is confirmed (the forge answered success),
// refused (forgeapi.WriteNotApplied) or unconfirmed (it may have been
// applied). Every run refusing is the first refusal; any other mix short
// of every run confirmed is a *CIPartialError, its Err the first refusal,
// else the first unconfirmed failure.
func (f *githubForge) eachRun(ctx context.Context, repo RepoReference, runs []string, action string) error {
	client, err := f.ciClient(repo)
	if err != nil {
		return err
	}
	done, unconfirmed := 0, 0
	var refused, unknown error
	for _, run := range runs {
		_, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: githubRepoPath(repo) + "/actions/runs/" + run + action}, nil)
		switch {
		case err == nil:
			done++
		case forgeapi.WriteNotApplied(err):
			refused = cmp.Or(refused, fmt.Errorf("workflow run %s: %w", run, err))
		default:
			unconfirmed++
			unknown = cmp.Or(unknown, fmt.Errorf("workflow run %s: %w", run, err))
		}
	}
	switch {
	case done == len(runs):
		return nil
	case done == 0 && unconfirmed == 0:
		return refused
	}
	return &CIPartialError{Done: done, Unconfirmed: unconfirmed, Total: len(runs), Err: cmp.Or(refused, unknown)}
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
		artifacts, _, err := readGitHubRunArtifacts(ctx, client, repo, run, false)
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
	if len(out) > 0 {
		return out, nil
	}
	// The flag that offered this download is stale: the next read asks
	// again instead of answering from the remembered capability.
	for _, run := range req.RunIDs {
		f.ciArtifacts.drop(githubRunArtifactsKey(repo, run))
	}
	if expired {
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
		f.ciArtifacts.drop(githubRunArtifactsKey(repo, artifact.RunID))
		return fmt.Errorf("%w: %w", ErrCIArtifactExpired, err)
	}
	return err
}
