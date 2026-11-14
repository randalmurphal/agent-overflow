package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/transport"
)

// The pipeline pump: one pipeline of a repository by id, for the Pull
// requests page's Pipelines tab (docs/specs/pull-requests.md). It is the
// PR pump's CI phase without a PR: the same pipeline poll, log follows and
// cadence (app_ci_phase.go), one pump per (repository, pipeline id)
// however many panes show it, refcounted, tied to each caller's
// connection, and paused while every caller is hidden. A terminal
// pipeline is read again at the PR update interval while visible, since a
// job retried on the forge restarts it with nothing else to say so.

// ErrTooManyPipelineSubscriptions is returned once the pipeline handle cap
// (maxPRUpdateHandles) is hit.
var ErrTooManyPipelineSubscriptions = errors.New("pipelines: too many active subscriptions")

// PipelineSubscriptionResult answers SubscribePipeline: the handle, the
// key ci:pipeline_updated frames name, the pump's pipeline (nil while its
// first read failed) with its active failure, and the sequence that state
// was stamped with, as PRUpdateSubscriptionResult's.
type PipelineSubscriptionResult struct {
	ID        string             `json:"id"`
	Key       string             `json:"key"`
	Pipeline  *gitops.CIPipeline `json:"pipeline"`
	Error     string             `json:"error"`
	ErrorKind string             `json:"errorKind"`
	Reserve   bool               `json:"reserve"`
	ResumeAt  string             `json:"resumeAt"`
	Seq       uint64             `json:"seq"`
}

// PipelineUpdatedEvent is the "ci:pipeline_updated" frame: the pipeline
// when it changed, or a read failure when that changed (the caller-safe
// summary and its kind fields, as PRCIUpdatedEvent's).
type PipelineUpdatedEvent struct {
	Key       string             `json:"key"`
	Pipeline  *gitops.CIPipeline `json:"pipeline,omitempty"`
	Error     string             `json:"error,omitempty"`
	ErrorKind string             `json:"errorKind,omitempty"`
	Reserve   bool               `json:"reserve,omitempty"`
	ResumeAt  string             `json:"resumeAt,omitempty"`
	Seq       uint64             `json:"seq"`
}

type pipelinePump struct {
	key  string
	repo gitops.RepoReference
	id   string
	pumpCore
	ciPhase
}

// pipelineHandle is one caller's reference on a pipeline pump, as
// prUpdateHandle is on a PR pump.
type pipelineHandle struct {
	pump    *pipelinePump
	active  bool
	follows map[string]bool
}

// pipelineKey is the entity key of a repository's pipeline: the
// repository key and the pipeline id. Keys are compared, never parsed.
func pipelineKey(repo gitops.RepoReference, id string) string {
	return repo.Key() + ":pipeline:" + id
}

func pipelineCleanupKey(id string) string { return "pipeline:" + id }

func (a *App) fetchPipeline(ctx context.Context, repo gitops.RepoReference, id string, prev *gitops.CIPipeline, stepsFor []string) (gitops.CIPipeline, error) {
	if a.prUpdates.pipelineFetchFn != nil {
		return a.prUpdates.pipelineFetchFn(ctx, repo, id, prev, stepsFor)
	}
	return a.gitCore().ReadPipeline(ctx, repo, id, prev, stepsFor)
}

// SubscribePipeline starts following one pipeline of repo and returns its
// current state and the handle that releases it. The first caller reads
// the pipeline (its action, so interactive) and creates the pump; a
// joiner is handed the pump's own state without a forge call. A failed
// first read still creates the pump, carrying the failure, which retries.
// The subscription is released when the calling connection drops.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SubscribePipeline(ctx context.Context, repo gitops.RepoReference, id string) (PipelineSubscriptionResult, error) {
	if a.shuttingDown.Load() {
		return PipelineSubscriptionResult{}, ErrShuttingDown
	}
	if err := repo.Validate(); err != nil {
		return PipelineSubscriptionResult{}, err
	}
	if err := gitops.ValidateCIJobID(id); err != nil {
		return PipelineSubscriptionResult{}, fmt.Errorf("pipeline id: %w", err)
	}
	key := pipelineKey(repo, id)
	result, joined, err := a.joinPipelinePump(key)
	if err != nil {
		return PipelineSubscriptionResult{}, err
	}
	if !joined {
		pipeline, readErr := a.fetchPipeline(forgeapi.WithInteractive(ctx), repo, id, nil, nil)
		if readErr != nil && ctx.Err() != nil {
			return PipelineSubscriptionResult{}, readErr
		}
		var start *pipelinePump
		result, start, err = a.createPipelinePump(repo, id, key, pipeline, readErr)
		if err != nil {
			return PipelineSubscriptionResult{}, err
		}
		if start != nil {
			a.prUpdates.wg.Go(func() { a.pumpPipeline(start) })
		}
	}
	if state := transport.ConnStateFromContext(ctx); state != nil {
		if !state.BindCleanup(pipelineCleanupKey(result.ID), func() { a.unsubscribePipeline(result.ID) }) {
			a.unsubscribePipeline(result.ID)
			return PipelineSubscriptionResult{}, errors.New("pipelines: connection closing")
		}
	}
	return result, nil
}

func (a *App) ensurePipelineMapsLocked() {
	if a.prUpdates.pipelines == nil {
		a.prUpdates.pipelines = make(map[string]*pipelinePump)
	}
	if a.prUpdates.pipelineHandles == nil {
		a.prUpdates.pipelineHandles = make(map[string]*pipelineHandle)
	}
}

func (a *App) livePipelinePumpLocked(key string) *pipelinePump {
	pump, ok := a.prUpdates.pipelines[key]
	if !ok || pump.dead {
		return nil
	}
	return pump
}

func (a *App) joinPipelinePump(key string) (PipelineSubscriptionResult, bool, error) {
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	a.ensurePipelineMapsLocked()
	if len(a.prUpdates.pipelineHandles) >= maxPRUpdateHandles {
		return PipelineSubscriptionResult{}, false, ErrTooManyPipelineSubscriptions
	}
	pump := a.livePipelinePumpLocked(key)
	if pump == nil {
		return PipelineSubscriptionResult{}, false, nil
	}
	return a.takePipelineReferenceLocked(pump), true, nil
}

// createPipelinePump registers a pump seeded with the caller's read, or
// joins one created meanwhile, which wins: its subscribers must all start
// from its state. start is the pump whose goroutine the caller launches.
func (a *App) createPipelinePump(repo gitops.RepoReference, id, key string, pipeline gitops.CIPipeline, readErr error) (PipelineSubscriptionResult, *pipelinePump, error) {
	var fail prForgeFailure
	var encoded []byte
	if readErr != nil {
		fail = a.newRepoForgeFailure(repo, readErr)
		log.Printf("pipelines: first read failed for %s (id: %s): %v", key, fail.correlationID, readErr)
	} else {
		var err error
		if encoded, err = json.Marshal(pipeline); err != nil {
			return PipelineSubscriptionResult{}, nil, err
		}
	}
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	a.ensurePipelineMapsLocked()
	if len(a.prUpdates.pipelineHandles) >= maxPRUpdateHandles {
		return PipelineSubscriptionResult{}, nil, ErrTooManyPipelineSubscriptions
	}
	if existing := a.livePipelinePumpLocked(key); existing != nil {
		return a.takePipelineReferenceLocked(existing), nil, nil
	}
	pump := &pipelinePump{key: key, repo: repo, id: id, pumpCore: a.newPumpCore()}
	pump.ciPhase = ciPhase{
		core:        &pump.pumpCore,
		repo:        repo,
		pipelineKey: key,
		fetch: func(ctx context.Context, prev *gitops.CIPipeline, stepsFor []string) (gitops.CIPipeline, error) {
			return a.fetchPipeline(ctx, repo, id, prev, stepsFor)
		},
		failure: func(err error) prForgeFailure { return a.newRepoForgeFailure(repo, err) },
		ciFail:  fail,
	}
	if readErr == nil {
		pump.ci, pump.ciKnown, pump.ciLast = pipeline, true, encoded
	}
	pump.seq = a.nextPRUpdateSeqLocked()
	a.prUpdates.pipelines[key] = pump
	return a.takePipelineReferenceLocked(pump), pump, nil
}

func (a *App) takePipelineReferenceLocked(pump *pipelinePump) PipelineSubscriptionResult {
	id := uuid.NewString()
	pump.takeLocked()
	a.prUpdates.pipelineHandles[id] = &pipelineHandle{pump: pump, active: true}
	result := PipelineSubscriptionResult{
		ID:        id,
		Key:       pump.key,
		Error:     pump.ciFail.wire,
		ErrorKind: pump.ciFail.kind,
		Reserve:   pump.ciFail.reserve,
		ResumeAt:  pump.ciFail.resumeAt,
		Seq:       pump.seq,
	}
	if pump.ciKnown {
		pipeline := pump.ci
		result.Pipeline = &pipeline
	}
	return result
}

// SetPipelineActive reports whether one subscriber currently shows its
// pipeline; the pump pauses while every subscriber is hidden. An unknown
// id is a no-op, as SetPRUpdatesActive's.
//
//ao:scope git:operate
//ao:route home
func (a *App) SetPipelineActive(ctx context.Context, subscriptionID string, active bool) error {
	a.prUpdates.mu.Lock()
	handle, ok := a.prUpdates.pipelineHandles[subscriptionID]
	if !ok || handle.active == active || handle.pump.dead {
		a.prUpdates.mu.Unlock()
		return nil
	}
	handle.active = active
	resumed := handle.pump.voteLocked(active)
	a.prUpdates.mu.Unlock()
	if resumed {
		handle.pump.wakeUp()
	}
	return nil
}

// SetPipelineFollows replaces the job logs one pipeline subscription
// follows, as SetPRCILogFollows does for a PR's: the pump fetches each
// requested job now and the reply carries their logs; later changes ride
// pr:ci_log frames carrying the pipeline's key.
//
//ao:scope git:operate
//ao:route home
func (a *App) SetPipelineFollows(ctx context.Context, subscriptionID string, jobIDs []string) (PRCILogFollowResult, error) {
	if a.shuttingDown.Load() {
		return PRCILogFollowResult{}, ErrShuttingDown
	}
	for _, jobID := range jobIDs {
		if err := gitops.ValidateCIJobID(jobID); err != nil {
			return PRCILogFollowResult{}, err
		}
	}
	if len(jobIDs) > maxPRCILogFollowsPerHandle {
		return PRCILogFollowResult{}, ErrTooManyPRCILogFollows
	}
	return a.setCILogFollows(ctx, jobIDs, func() (*ciPhase, *map[string]bool, bool) {
		handle, ok := a.prUpdates.pipelineHandles[subscriptionID]
		if !ok || handle.pump.dead {
			return nil, nil, false
		}
		return &handle.pump.ciPhase, &handle.follows, true
	})
}

// RefreshPipeline reads a subscribed pipeline now; the change reaches
// every subscriber on ci:pipeline_updated and the error is the read's
// failure, for the button that asked.
//
//ao:scope git:operate
//ao:route home
func (a *App) RefreshPipeline(ctx context.Context, subscriptionID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	a.prUpdates.mu.Lock()
	handle, ok := a.prUpdates.pipelineHandles[subscriptionID]
	if !ok || handle.pump.dead {
		a.prUpdates.mu.Unlock()
		return errPRUpdatesUnknownSubscription
	}
	pump := handle.pump
	a.prUpdates.mu.Unlock()
	return a.requestPump(ctx, &pump.pumpCore, &prPumpRequest{ci: true})
}

//ao:scope git:operate
//ao:route home
func (a *App) UnsubscribePipeline(ctx context.Context, subscriptionID string) error {
	transport.ConnStateFromContext(ctx).UnbindCleanup(pipelineCleanupKey(subscriptionID))
	a.unsubscribePipeline(subscriptionID)
	return nil
}

// unsubscribePipeline releases one handle; the last stops its pump.
// Idempotent, since the connection's cleanup may have run first.
func (a *App) unsubscribePipeline(id string) {
	a.prUpdates.mu.Lock()
	handle, ok := a.prUpdates.pipelineHandles[id]
	if !ok {
		a.prUpdates.mu.Unlock()
		return
	}
	delete(a.prUpdates.pipelineHandles, id)
	pump := handle.pump
	releaseFollowsLocked(&pump.ciPhase, handle.follows)
	handle.follows = nil
	var teardown *pipelinePump
	if pump.releaseLocked(handle.active) {
		teardown = pump
		if a.prUpdates.pipelines[pump.key] == pump {
			delete(a.prUpdates.pipelines, pump.key)
		}
	}
	a.prUpdates.mu.Unlock()
	if teardown != nil {
		teardown.stop()
	}
}

// dropPipelinePump removes a pump whose goroutine exited on its own, with
// every handle on it.
func (a *App) dropPipelinePump(pump *pipelinePump) {
	pump.cancel()
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	pump.dead = true
	if a.prUpdates.pipelines[pump.key] == pump {
		delete(a.prUpdates.pipelines, pump.key)
	}
	for id, handle := range a.prUpdates.pipelineHandles {
		if handle.pump == pump {
			releaseFollowsLocked(&pump.ciPhase, handle.follows)
			handle.follows = nil
			delete(a.prUpdates.pipelineHandles, id)
		}
	}
}

// closePipelinePumps releases every pipeline handle and stops what is
// left; the caller joins the goroutines (closePRUpdatePumps).
func (a *App) closePipelinePumps() {
	a.prUpdates.mu.Lock()
	ids := make([]string, 0, len(a.prUpdates.pipelineHandles))
	for id := range a.prUpdates.pipelineHandles {
		ids = append(ids, id)
	}
	a.prUpdates.mu.Unlock()
	for _, id := range ids {
		a.unsubscribePipeline(id)
	}
	a.prUpdates.mu.Lock()
	orphans := make([]*pipelinePump, 0, len(a.prUpdates.pipelines))
	for key, pump := range a.prUpdates.pipelines {
		delete(a.prUpdates.pipelines, key)
		orphans = append(orphans, pump)
	}
	a.prUpdates.mu.Unlock()
	for _, pump := range orphans {
		pump.stop()
	}
}

// pollPipeline polls the pipeline and words a change as its frame.
func (a *App) pollPipeline(ctx context.Context, pump *pipelinePump) (PipelineUpdatedEvent, bool) {
	result, changed := a.pollCIPhase(ctx, &pump.ciPhase, nil)
	if !changed {
		return PipelineUpdatedEvent{}, false
	}
	return PipelineUpdatedEvent{
		Key:       pump.key,
		Pipeline:  result.pipeline,
		Error:     result.fail.wire,
		ErrorKind: result.fail.kind,
		Reserve:   result.fail.reserve,
		ResumeAt:  result.fail.resumeAt,
		Seq:       result.seq,
	}, true
}

func (a *App) pumpPipeline(pump *pipelinePump) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("pipelines: pump panic for %s: %v", pump.key, r)
		}
		a.dropPipelinePump(pump)
	}()
	// The slow tick reads a terminal pipeline again; the CI phase's own
	// timer runs it faster while anything is live, retries a failing
	// read, and holds for a rate limit.
	ticker := time.NewTicker(a.prUpdatePollInterval())
	defer ticker.Stop()
	var ciTick <-chan time.Time
	arm := func(delay time.Duration) {
		if delay > 0 {
			ciTick = time.After(delay)
		} else {
			ciTick = nil
		}
	}
	missed := false
	run := func(ctx context.Context, forced map[string]bool, poll bool) {
		missed = false
		arm(a.runCIPhase(ctx, &pump.ciPhase, forced, poll, func(ctx context.Context) {
			if event, changed := a.pollPipeline(ctx, pump); changed {
				a.emitPumpEvent(&pump.pumpCore, eventchan.PipelineUpdated, event)
			}
		}))
	}
	// The subscribe read the pipeline: the first turn waits for the
	// cadence it asks for (a failed read's retry included).
	arm(a.ciInterval(&pump.ciPhase))
	for {
		select {
		case <-pump.done:
			return
		case <-pump.ctx.Done():
			return
		case req := <-pump.requests:
			a.servePumpRequest(&pump.ciPhase, req, nil, run)
		case <-ciTick:
			if pump.paused.Load() {
				missed, ciTick = true, nil
				continue
			}
			run(pump.ctx, nil, true)
		case <-ticker.C:
			if pump.paused.Load() {
				missed = true
				continue
			}
			// A background read before a rate limit's release asks nothing
			// (pollCIPhase); the phase's timer is armed for the release.
			run(pump.ctx, nil, true)
		case <-pump.wake:
			if pump.paused.Load() || !missed {
				continue
			}
			run(pump.ctx, nil, true)
		}
	}
}
