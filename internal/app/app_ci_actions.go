package app

import (
	"context"
	"errors"
	"log"
	"slices"
	"time"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
)

// The CI actions of the review pane and the Pipelines tab: retry a job,
// retry a pipeline's failed jobs, cancel a job (GitLab) or a pipeline,
// run a manual job (GitLab). Both surfaces render one CI view over a CI
// phase (app_ci_phase.go): a PR pump's head pipeline or a pipeline pump's
// pipeline, so one implementation serves both, keyed by the caller's
// subscription as RefreshPRCI and RefreshPipeline are. An action is
// checked against the pipeline the phase holds (the one the caller
// shows), runs through git.Core, and on success asks the phase's pump,
// and every other pump showing the same pipeline, to poll at once. The
// answer carries the caller's pump sequence after that poll
// (CIActionResult.Seq), as PRActionResult.Seq does for the merge
// actions: the caller holds its control pending until its applied CI
// sequence reaches it. One action runs per pipeline at a time. Failures
// are the one-sentence ForgeActionError (git.ExplainCIError). See
// docs/specs/pull-requests.md.

// ciActionFollowWindow is how long after an action its phase polls at
// the live cadence whatever the pipeline shows: GitHub queues a re-run
// asynchronously, so the poll right after it can still read the run
// completed.
const ciActionFollowWindow = time.Minute

// CIActionRequest is one action on the caller's pipeline: Kind, and the
// job's id for a job action (retry_job, cancel_job, play_job).
type CIActionRequest struct {
	Kind  gitops.CIActionKind `json:"kind"`
	JobID string              `json:"jobId,omitempty"`
}

// CIActionResult is an action's answer. Seq is the caller's pump sequence
// once the poll the action asked for ran: the CI frame that carried the
// forge's answer (pr:ci_updated or ci:pipeline_updated) has that sequence,
// or a smaller one when the poll found the pipeline unchanged, so a
// caller whose applied CI sequence has reached Seq shows the forge's
// answer.
type CIActionResult struct {
	Seq uint64 `json:"seq"`
}

// ciActionTarget is the phase an action acts through, found under
// App.prUpdates.mu: its pump, and the PR whose head checks it shows (nil
// for a pipeline pump).
type ciActionTarget struct {
	phase *ciPhase
	core  *pumpCore
	pr    *gitops.PRReference
}

// RunPRCIAction runs one CI action on a subscribed pull request's head
// pipeline (SubscribePRUpdates' id).
//
//ao:scope git:operate
//ao:route home
func (a *App) RunPRCIAction(ctx context.Context, subscriptionID string, req CIActionRequest) (CIActionResult, error) {
	return a.runCIAction(ctx, req, func() (ciActionTarget, bool) {
		handle, ok := a.prUpdates.handles[subscriptionID]
		if !ok || handle.pump.dead {
			return ciActionTarget{}, false
		}
		pr := handle.pump.pr
		return ciActionTarget{phase: &handle.pump.ciPhase, core: &handle.pump.pumpCore, pr: &pr}, true
	})
}

// RunPipelineAction runs one CI action on a subscribed pipeline
// (SubscribePipeline's id).
//
//ao:scope git:operate
//ao:route home
func (a *App) RunPipelineAction(ctx context.Context, subscriptionID string, req CIActionRequest) (CIActionResult, error) {
	return a.runCIAction(ctx, req, func() (ciActionTarget, bool) {
		handle, ok := a.prUpdates.pipelineHandles[subscriptionID]
		if !ok || handle.pump.dead {
			return ciActionTarget{}, false
		}
		return ciActionTarget{phase: &handle.pump.ciPhase, core: &handle.pump.pumpCore}, true
	})
}

// ciActionRefusal is an action the pipeline on screen does not allow,
// refused before any forge request.
func ciActionRefusal(kind, message string) error {
	return &gitops.ForgeActionError{Kind: kind, Message: message}
}

// runCIAction is both bound methods: lookup runs under App.prUpdates.mu
// and names the caller's phase.
func (a *App) runCIAction(ctx context.Context, req CIActionRequest, lookup func() (ciActionTarget, bool)) (CIActionResult, error) {
	if a.shuttingDown.Load() {
		return CIActionResult{}, ErrShuttingDown
	}
	if err := req.Kind.Validate(); err != nil {
		return CIActionResult{}, err
	}
	if req.Kind.JobScoped() {
		if err := gitops.ValidateCIJobID(req.JobID); err != nil {
			return CIActionResult{}, err
		}
	} else if req.JobID != "" {
		return CIActionResult{}, errors.New("a pipeline action names no job")
	}

	a.prUpdates.mu.Lock()
	target, ok := lookup()
	if !ok {
		a.prUpdates.mu.Unlock()
		return CIActionResult{}, errPRUpdatesUnknownSubscription
	}
	phase := target.phase
	if !phase.ciKnown {
		a.prUpdates.mu.Unlock()
		return CIActionResult{}, ciActionRefusal(gitops.ForgeErrorRefused, "The pipeline has not been read yet. Refresh and try again.")
	}
	pipeline := phase.ci
	repo := phase.repo
	subject := gitops.CIPipelineSubject(repo, pipeline.ID, target.pr)
	var job *gitops.CIJob
	if req.Kind.JobScoped() {
		job = gitops.FindCIJob(pipeline, req.JobID)
		if job == nil {
			a.prUpdates.mu.Unlock()
			return CIActionResult{}, ciActionRefusal(gitops.ForgeErrorNotFound, "This job is no longer in the pipeline. Refresh to see its current jobs.")
		}
		subject = gitops.CIJobSubject(repo, job.Name)
	}
	action := req.Kind.ForgeAction()
	switch req.Kind.State(job, pipeline) {
	case gitops.CIActionAllowed:
	case gitops.CIActionDenied:
		a.prUpdates.mu.Unlock()
		return CIActionResult{}, ciActionRefusal(gitops.ForgeErrorForbidden, "This login may not "+string(action)+" "+subject+".")
	default:
		a.prUpdates.mu.Unlock()
		return CIActionResult{}, ciActionRefusal(gitops.ForgeErrorRefused, "You can no longer "+string(action)+" "+subject+". Refresh to see its current state.")
	}
	keys := ciActionKeys(repo, pipeline, job, phase)
	if slices.ContainsFunc(keys, func(key string) bool { return a.prUpdates.ciActions[key] }) {
		a.prUpdates.mu.Unlock()
		return CIActionResult{}, ciActionRefusal(gitops.ForgeErrorRefused, "Another action on this pipeline is still running. Wait for it to finish.")
	}
	if a.prUpdates.ciActions == nil {
		a.prUpdates.ciActions = make(map[string]bool)
	}
	for _, key := range keys {
		a.prUpdates.ciActions[key] = true
	}
	a.prUpdates.mu.Unlock()
	defer func() {
		a.prUpdates.mu.Lock()
		for _, key := range keys {
			delete(a.prUpdates.ciActions, key)
		}
		a.prUpdates.mu.Unlock()
	}()

	err := a.gitCore().RunCIAction(forgeapi.WithInteractive(ctx), repo, req.Kind, req.JobID, pipeline)
	// A write that took effect, or may have, is followed on the forge; one
	// the forge refused in full changed nothing.
	if partial, ok := errors.AsType[*gitops.CIPartialError](err); err != nil && (!ok || !partial.Applied()) {
		return CIActionResult{}, gitops.ExplainCIError(repo, subject, action, err)
	}
	seq := a.refreshCIAfterAction(ctx, target, repo, pipeline)
	return CIActionResult{Seq: seq}, gitops.ExplainCIError(repo, subject, action, err)
}

// ciActionKeys are the forge identities an action reserves while it runs:
// the workflow runs (GitHub) or the pipeline (GitLab) its writes reach.
// A job action reaches its own run or pipeline, a pipeline action every
// one the pipeline shows (ciPipelineIDs), so a PR's head checks and a
// Pipelines-tab view of one of their runs share the run's key. Keys are
// per repository; a pipeline with no identity at all (no run read yet)
// is keyed by its PR.
func ciActionKeys(repo gitops.RepoReference, pipeline gitops.CIPipeline, job *gitops.CIJob, phase *ciPhase) []string {
	var ids []string
	switch {
	case job != nil && job.RunID != "":
		ids = []string{job.RunID}
	case job != nil && pipeline.ID != "":
		ids = []string{pipeline.ID}
	default:
		for id := range ciPipelineIDs(pipeline) {
			ids = append(ids, id)
		}
	}
	keys := make([]string, 0, len(ids)+1)
	for _, id := range ids {
		keys = append(keys, "ci:"+repo.Key()+" "+id)
	}
	if len(keys) == 0 {
		keys = append(keys, "pr:"+phase.prKey)
	}
	return keys
}

// ciPipelineIDs are the forge ids a pipeline read names: its own and, on
// GitHub, every workflow run its jobs belong to.
func ciPipelineIDs(pipeline gitops.CIPipeline) map[string]bool {
	ids := map[string]bool{}
	if pipeline.ID != "" {
		ids[pipeline.ID] = true
	}
	for _, stage := range pipeline.Stages {
		for _, job := range stage.Jobs {
			if job.RunID != "" {
				ids[job.RunID] = true
			}
		}
	}
	return ids
}

// refreshCIAfterAction has the caller's pump poll its pipeline now and
// answers the pump's sequence after that poll; every other live pump
// showing a pipeline the action touched polls too, without holding up
// the answer. Each affected phase then polls at the live cadence for
// ciActionFollowWindow. A poll's own failure does not fail the action:
// the pump reports it on its own channel.
func (a *App) refreshCIAfterAction(ctx context.Context, target ciActionTarget, repo gitops.RepoReference, pipeline gitops.CIPipeline) uint64 {
	touched := ciPipelineIDs(pipeline)
	a.prUpdates.mu.Lock()
	target.phase.actedAt = time.Now()
	a.prUpdates.mu.Unlock()
	a.pollCIPumpsShowing(repo, target.core, true, func(phase *ciPhase) bool {
		for id := range ciPipelineIDs(phase.ci) {
			if touched[id] {
				return true
			}
		}
		return false
	})
	if err := a.requestPump(ctx, target.core, &prPumpRequest{ci: true}); err != nil && !errors.Is(err, errPRUpdatesUnknownSubscription) && !errors.Is(err, context.Canceled) {
		log.Printf("pr updates: ci refresh after an action failed for %s: %v", target.phase.name(), err)
	}
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	return target.core.seq
}

// pollCIPumpsShowing asks every live pump, PR or pipeline, whose phase
// holds a pipeline of repo that shows selects, other than skip, to poll
// its CI at once, without waiting for the polls. acted starts each one's
// follow window (ciActionFollowWindow).
func (a *App) pollCIPumpsShowing(repo gitops.RepoReference, skip *pumpCore, acted bool, shows func(*ciPhase) bool) {
	match := func(core *pumpCore, phase *ciPhase) bool {
		return core != skip && phase.repo.Key() == repo.Key() && phase.ciKnown && shows(phase)
	}
	now := time.Now()
	a.prUpdates.mu.Lock()
	var cores []*pumpCore
	for _, pump := range a.prUpdates.pumps {
		if !pump.dead && match(&pump.pumpCore, &pump.ciPhase) {
			cores = append(cores, &pump.pumpCore)
			if acted {
				pump.ciPhase.actedAt = now
			}
		}
	}
	for _, pump := range a.prUpdates.pipelines {
		if !pump.dead && match(&pump.pumpCore, &pump.ciPhase) {
			cores = append(cores, &pump.pumpCore)
			if acted {
				pump.ciPhase.actedAt = now
			}
		}
	}
	a.prUpdates.mu.Unlock()
	for _, core := range cores {
		a.prUpdates.wg.Go(func() {
			if err := a.requestPump(a.lifeCtx(), core, &prPumpRequest{ci: true}); err != nil && !errors.Is(err, errPRUpdatesUnknownSubscription) && !errors.Is(err, ErrShuttingDown) && !errors.Is(err, context.Canceled) {
				log.Printf("pr updates: ci refresh of a pipeline another caller changed failed: %v", err)
			}
		})
	}
}
