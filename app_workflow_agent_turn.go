package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
	workflowrunner "agent-overflow/internal/workflow/runner"
)

type workflowFullPrompt func(workflowrunner.PromptContext) (string, error)

type workflowAgentTurnPlan struct {
	request       engine.RunRequest
	thread        workflowThreadSpec
	schema        json.RawMessage
	promptContext workflowrunner.PromptContext
	buildFull     workflowFullPrompt
	attach        func(string) error
}

type preparedWorkflowAgentTurn struct {
	threadID       string
	prompt         string
	createdThread  bool
	startedSession bool
	attached       bool
	attach         func(string) error
}

// prepareAgentTurn is the single pre-send path for phase turns, work units, and
// joins. It resolves the launch, proves a reused provider context is available,
// assembles the prompt, and returns one complete preparation for install.
// Attachment happens during install, after the duplicate-start guard and before
// the send. Failures before attachment erase their draft thread; failures after
// attachment retain it as attempt provenance. Every element kind follows that
// same boundary and cleanup path.
func (r *workflowAppRunner) prepareAgentTurn(ctx context.Context, plan workflowAgentTurnPlan) (preparedWorkflowAgentTurn, error) {
	var prepared preparedWorkflowAgentTurn
	if plan.attach == nil {
		return prepared, errors.New("workflow runner: agent turn attachment is required")
	}
	if plan.buildFull == nil {
		return prepared, errors.New("workflow runner: full-prompt builder is required")
	}
	prepared, err := r.prepareAgentTurnThread(ctx, plan)
	if err != nil {
		return preparedWorkflowAgentTurn{}, err
	}
	discard := func(err error) (preparedWorkflowAgentTurn, error) {
		return preparedWorkflowAgentTurn{}, prepared.discard(r, err)
	}
	if err := ctx.Err(); err != nil {
		return discard(fmt.Errorf("workflow runner: startup cancelled: %w", err))
	}

	promptContext := plan.promptContext
	switch {
	case plan.request.Launch.FinalizesTakeover():
		prepared.prompt, err = workflowrunner.BuildTakeoverFinalizePrompt(promptContext)
	case plan.request.Launch.ContinuesThread():
		promptContext.Feedback = plan.request.Feedback
		prepared.prompt, err = workflowrunner.BuildContinuationPrompt(promptContext)
	default:
		promptContext.Feedback = plan.request.Feedback
		promptContext.Guidance = plan.request.Guidance
		prepared.prompt, err = plan.buildFull(promptContext)
	}
	if err != nil {
		return discard(err)
	}
	prepared.attach = plan.attach
	return prepared, nil
}

func (r *workflowAppRunner) prepareAgentTurnThread(ctx context.Context, plan workflowAgentTurnPlan) (preparedWorkflowAgentTurn, error) {
	threadID := plan.request.Launch.ThreadID()
	if threadID == "" {
		return r.createPreparedWorkflowThread(plan.thread)
	}
	prior, err := r.validatePriorThread(plan.thread, threadID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return preparedWorkflowAgentTurn{}, unavailableAgentTurn(threadID, err)
		}
		return preparedWorkflowAgentTurn{}, err
	}
	started, err := r.ensureReusableWorkflowSession(ctx, prior, plan.schema)
	if err != nil {
		if errors.Is(err, engine.ErrProviderContextUnavailable) {
			return preparedWorkflowAgentTurn{}, unavailableAgentTurn(threadID, err)
		}
		return preparedWorkflowAgentTurn{}, err
	}
	return preparedWorkflowAgentTurn{threadID: threadID, startedSession: started}, nil
}

func unavailableAgentTurn(threadID string, cause error) error {
	return errors.Join(engine.ErrProviderContextUnavailable,
		fmt.Errorf("workflow runner: provider context for thread %q is unavailable: %w", threadID, cause))
}

func (r *workflowAppRunner) createPreparedWorkflowThread(spec workflowThreadSpec) (preparedWorkflowAgentTurn, error) {
	thread, err := r.app.createWorkflowThread(spec)
	if err != nil {
		return preparedWorkflowAgentTurn{}, err
	}
	return preparedWorkflowAgentTurn{threadID: thread.ID, createdThread: true}, nil
}

// ensureReusableWorkflowSession makes provider-context availability a fact,
// not an inference from a non-empty database cursor. A live process is already
// proof. Cold Claude resumes are preflighted against the transcript; cold Codex
// resumes are proven by its synchronous thread/resume handshake.
func (r *workflowAppRunner) ensureReusableWorkflowSession(ctx context.Context, thread store.Thread, schema json.RawMessage) (bool, error) {
	if _, active := r.app.sessionManager().get(thread.ID); active {
		return false, nil
	}
	if thread.ResolvedSessionRef() == "" {
		return false, errors.Join(engine.ErrProviderContextUnavailable,
			fmt.Errorf("workflow runner: thread %q has neither a live provider process nor a durable session cursor", thread.ID))
	}
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("workflow runner: startup cancelled: %w", err)
	}
	if thread.Provider == string(provider.Claude) {
		leaf, err := claude.ScanSessionLeaf(thread.ResolvedSessionRef(), thread.WorkspacePath)
		if errors.Is(err, sessionfork.ErrSessionFileNotFound) {
			return false, errors.Join(engine.ErrProviderContextUnavailable,
				fmt.Errorf("workflow runner: Claude session %q has no transcript", thread.ResolvedSessionRef()))
		}
		if err != nil {
			return false, fmt.Errorf("workflow runner: validate Claude session %q: %w", thread.ResolvedSessionRef(), err)
		}
		if leaf.CanonicalLeafUUID == "" {
			return false, errors.Join(engine.ErrProviderContextUnavailable,
				fmt.Errorf("workflow runner: Claude session %q has no resumable transcript leaf", thread.ResolvedSessionRef()))
		}
	}

	if err := r.registerTemporarySchema(thread.ID, schema); err != nil {
		return false, err
	}
	if err := r.app.StartSession(thread.ID); err != nil {
		r.removeTemporarySchema(thread.ID)
		if codex.IsThreadNotFound(err) || errors.Is(err, sql.ErrNoRows) {
			return false, errors.Join(engine.ErrProviderContextUnavailable, err)
		}
		return false, fmt.Errorf("workflow runner: resume provider session %q: %w", thread.ID, err)
	}
	return true, nil
}

func (r *workflowAppRunner) registerTemporarySchema(threadID string, schema json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.schemas[threadID]; exists {
		return fmt.Errorf("workflow runner: thread %q already has an output schema registered", threadID)
	}
	r.schemas[threadID] = append(json.RawMessage(nil), schema...)
	return nil
}

func (p preparedWorkflowAgentTurn) discard(r *workflowAppRunner, cause error) error {
	var errs []error
	errs = append(errs, cause)
	if p.startedSession {
		if err := r.app.stopSession(p.threadID); err != nil {
			errs = append(errs, fmt.Errorf("stop prestarted workflow session %q: %w", p.threadID, err))
		}
		r.removeTemporarySchema(p.threadID)
	}
	// Once attached, the thread is attempt provenance even if cancellation wins
	// before the opening send. The engine will settle that startup failure and a
	// later resume will see the intentionally cursor-less thread. Only an
	// unattached draft is safe to erase here.
	if p.createdThread && !p.attached {
		if err := r.app.store.DeleteThread(p.threadID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
