package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/workflow/engine"
)

const (
	workflowDigestTimeout    = 60 * time.Second
	workflowDigestMaxRunes   = 280
	workflowDigestPromptSize = 12 * 1024
)

const workflowDigestSchemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "whatHappened": {"type": "string"},
    "whatItNeeds": {"type": "string"}
  },
  "required": ["whatHappened", "whatItNeeds"]
}`

type WorkflowDigest struct {
	WhatHappened string `json:"whatHappened"`
	WhatItNeeds  string `json:"whatItNeeds"`
}

type workflowDigestContext struct {
	Phase    string
	Question string
	Stuck    string
	Checks   []string
}

func (a *App) prepareWorkflowEngineEvent(name string, payload any) {
	if name != "workflow:item-state" {
		return
	}
	event, ok := payload.(engine.StateEvent)
	if !ok || event.From == event.To || event.To != engine.StateNeedsHuman && event.To != engine.StateFailed {
		return
	}
	context, err := a.store.GetWorkItemAttentionContext(event.ItemID)
	if err != nil {
		log.Printf("workflow digest %s: load attention context: %v", event.ItemID, err)
		return
	}
	digest := workflowTemplateDigest(
		context.Item, context.PhaseID, context.OutputEnvelope, context.Check,
	)
	encoded, err := json.Marshal(digest)
	if err != nil {
		log.Printf("workflow digest %s: encode template: %v", event.ItemID, err)
		return
	}
	if err := a.store.UpdateWorkItemDigest(event.ItemID, encoded); err != nil {
		// Digest quality must never make a lifecycle transition fail.
		log.Printf("workflow digest %s: persist template: %v", event.ItemID, err)
	}
}

func workflowTemplateDigest(
	item store.WorkItem,
	phaseID string,
	outputEnvelope json.RawMessage,
	check string,
) WorkflowDigest {
	ctx := workflowDigestInputs(phaseID, outputEnvelope, check)
	phase := ctx.Phase
	if phase == "" {
		phase = "the current phase"
	}
	reason := engine.Reason(item.Reason)
	digest := WorkflowDigest{}
	if item.State == string(engine.StateFailed) {
		digest.WhatHappened = fmt.Sprintf("The run failed during %s.", phase)
	} else {
		switch reason {
		case engine.ReasonGate:
			digest.WhatHappened = fmt.Sprintf("The run paused after %s for review.", phase)
		case engine.ReasonQuestion:
			digest.WhatHappened = fmt.Sprintf("The run paused in %s with a question.", phase)
		case engine.ReasonStuck:
			digest.WhatHappened = fmt.Sprintf("The run is stuck in %s%s.", phase, optionalDigestDetail(ctx.Stuck))
		case engine.ReasonDisposition:
			digest.WhatHappened = "The work finished, but its branch could not be disposed cleanly."
		default:
			digest.WhatHappened = fmt.Sprintf("The run paused in %s because %s.", phase, workflowReasonText(reason))
		}
	}

	switch reason {
	case engine.ReasonQuestion:
		if ctx.Question != "" {
			digest.WhatItNeeds = ctx.Question
		} else {
			digest.WhatItNeeds = "Answer the phase's question so the run can continue."
		}
	case engine.ReasonGate:
		if len(ctx.Checks) > 0 {
			digest.WhatItNeeds = fmt.Sprintf("Review %s and the %s check results, then choose whether the run should continue.", phase, strings.Join(ctx.Checks, ", "))
		} else {
			digest.WhatItNeeds = fmt.Sprintf("Review %s and choose whether the run should continue.", phase)
		}
	case engine.ReasonStuck:
		digest.WhatItNeeds = "Provide guidance or continue the work with an agent."
	case engine.ReasonCheckFailedGenuine:
		if len(ctx.Checks) > 0 {
			digest.WhatItNeeds = "Investigate the failed checks: " + strings.Join(ctx.Checks, ", ") + "."
		} else {
			digest.WhatItNeeds = "Investigate the failed deterministic checks and decide whether to retry."
		}
	case engine.ReasonDisposition:
		digest.WhatItNeeds = "Resolve the branch or worktree issue, then retry merge or PR creation."
	case engine.ReasonSetupFailed:
		digest.WhatItNeeds = "Repair the worktree setup problem, then resume the run."
	case engine.ReasonBudgetExhausted:
		digest.WhatItNeeds = "Review the run's spend and choose whether to resume with a larger budget."
	case engine.ReasonTakenOver:
		digest.WhatItNeeds = "Finish the human takeover or return the phase to the workflow."
	case engine.ReasonAgentError:
		digest.WhatItNeeds = "Review the agent failure and decide whether to retry or take over."
	default:
		digest.WhatItNeeds = "Review the run and choose whether to retry, take over, or discard it."
	}
	digest.WhatHappened = textgen.CapRunesWithEllipsis(digest.WhatHappened, workflowDigestMaxRunes)
	digest.WhatItNeeds = textgen.CapRunesWithEllipsis(digest.WhatItNeeds, workflowDigestMaxRunes)
	return digest
}

func workflowDigestInputs(phaseID string, outputEnvelope json.RawMessage, check string) workflowDigestContext {
	ctx := workflowDigestContext{Phase: strings.TrimSpace(phaseID)}
	if len(outputEnvelope) > 0 {
		var envelope struct {
			Question *string `json:"question"`
			Reason   *string `json:"reason"`
		}
		if json.Unmarshal(outputEnvelope, &envelope) == nil {
			if envelope.Question != nil {
				ctx.Question = strings.TrimSpace(*envelope.Question)
			}
			if envelope.Reason != nil {
				ctx.Stuck = strings.TrimSpace(*envelope.Reason)
			}
		}
	}
	if check = strings.TrimSpace(check); check != "" {
		ctx.Checks = []string{check}
	}
	return ctx
}

func optionalDigestDetail(value string) string {
	if value == "" {
		return ""
	}
	return ": " + textgen.CapRunesWithEllipsis(value, 120)
}

func workflowReasonText(reason engine.Reason) string {
	switch reason {
	case engine.ReasonStalled:
		return "the active phase stopped producing activity"
	case engine.ReasonBudgetExhausted:
		return "the run reached its budget"
	case engine.ReasonRetriesExhausted:
		return "transient retries were exhausted"
	case engine.ReasonAgentError:
		return "the agent could not produce a valid result"
	case engine.ReasonWiringError:
		return "the workflow definition could not route safely"
	case engine.ReasonSetupFailed:
		return "worktree setup failed"
	case engine.ReasonInterrupted:
		return "execution was interrupted"
	case engine.ReasonTakenOver:
		return "a human took control of the phase"
	default:
		return "human input is required"
	}
}

func (a *App) upgradeWorkflowDigest(item store.WorkItem, template WorkflowDigest, expected []byte) {
	generated, err := a.generateWorkflowDigest(item, template)
	if err != nil {
		log.Printf("workflow digest %s: async upgrade: %v", item.ID, err)
		return
	}
	current, err := a.store.GetWorkItem(item.ID)
	if err != nil {
		log.Printf("workflow digest %s: reload before upgrade: %v", item.ID, err)
		return
	}
	if current.State != item.State || current.Reason != item.Reason || !bytes.Equal(current.Digest, expected) {
		return
	}
	encoded, err := json.Marshal(generated)
	if err != nil {
		log.Printf("workflow digest %s: encode upgrade: %v", item.ID, err)
		return
	}
	if err := a.store.UpdateWorkItemDigest(item.ID, encoded); err != nil {
		log.Printf("workflow digest %s: persist upgrade: %v", item.ID, err)
		return
	}
	a.emit("workflow:item-state", engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID,
		From: engine.State(item.State), To: engine.State(item.State), Reason: engine.Reason(item.Reason),
	})
}

func (a *App) queueWorkflowDigestUpgrade(item store.WorkItem, template WorkflowDigest, expected []byte) {
	a.workflowNotificationsMu.Lock()
	if a.workflowDigestSlots == nil {
		a.workflowDigestSlots = make(chan struct{}, 2)
	}
	slots := a.workflowDigestSlots
	select {
	case slots <- struct{}{}:
	default:
		a.workflowNotificationsMu.Unlock()
		log.Printf("workflow digest %s: async upgrade skipped because both generator slots are busy", item.ID)
		return
	}
	a.workflowNotificationsMu.Unlock()
	go func() {
		defer func() { <-slots }()
		if a.lifeCtx().Err() != nil {
			return
		}
		a.upgradeWorkflowDigest(item, template, expected)
	}()
}

func (a *App) generateWorkflowDigest(item store.WorkItem, template WorkflowDigest) (WorkflowDigest, error) {
	if a.generateWorkflowDigestFn != nil {
		return a.generateWorkflowDigestFn(a.lifeCtx(), item, template)
	}
	deadline := time.Now().Add(workflowDigestTimeout)
	primary := a.resolveTextGenerationConfig()
	return runTextGenWithFallback(a, primary, deadline, func(cfg textgen.Config) (WorkflowDigest, error) {
		return a.runWorkflowDigestOnce(cfg, item, template, deadline)
	})
}

func (a *App) runWorkflowDigestOnce(cfg textgen.Config, item store.WorkItem, template WorkflowDigest, deadline time.Time) (WorkflowDigest, error) {
	ctx, cancel := context.WithDeadline(a.lifeCtx(), deadline)
	defer cancel()
	projectRow, err := a.store.GetProject(item.ProjectID)
	if err != nil {
		return WorkflowDigest{}, err
	}
	workspace := projectRow.Path
	if item.WorktreePath != "" {
		workspace = item.WorktreePath
	}
	prompt := textgen.LimitPromptSection(fmt.Sprintf(`Rewrite this deterministic workflow digest into two terse, plain-language sentences for a human run-detail view.
Do not mention envelopes, JSON, schemas, gate traces, implementation internals, or speculate beyond the supplied facts.
Keep each field under %d characters.

Goal: %s
State: %s
Reason class: %s
Template WHAT HAPPENED: %s
Template WHAT IT NEEDS: %s`, workflowDigestMaxRunes, item.Goal, item.State, item.Reason, template.WhatHappened, template.WhatItNeeds), workflowDigestPromptSize)

	var raw []byte
	switch cfg.Provider {
	case string(provider.Codex):
		raw, err = textgen.RunCodex(ctx, cfg, workspace, workflowDigestSchemaJSON, nil, prompt, remainingBudget(ctx, workflowDigestTimeout))
	case string(provider.Claude):
		raw, err = textgen.RunClaude(ctx, cfg, workspace, workflowDigestSchemaJSON, nil, prompt, remainingBudget(ctx, workflowDigestTimeout))
		if err == nil {
			decoded, decodeErr := textgen.DecodeClaudeStructuredLastLine[WorkflowDigest](raw)
			if decodeErr != nil {
				return WorkflowDigest{}, decodeErr
			}
			return sanitizeWorkflowDigest(decoded)
		}
	default:
		return WorkflowDigest{}, fmt.Errorf("workflow digest: unsupported provider %q", cfg.Provider)
	}
	if err != nil {
		return WorkflowDigest{}, err
	}
	var decoded WorkflowDigest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return WorkflowDigest{}, fmt.Errorf("workflow digest: decode structured output: %w", err)
	}
	return sanitizeWorkflowDigest(decoded)
}

func sanitizeWorkflowDigest(digest WorkflowDigest) (WorkflowDigest, error) {
	digest.WhatHappened = textgen.CapRunesWithEllipsis(textgen.NormalizeStructuredOutputLine(digest.WhatHappened), workflowDigestMaxRunes)
	digest.WhatItNeeds = textgen.CapRunesWithEllipsis(textgen.NormalizeStructuredOutputLine(digest.WhatItNeeds), workflowDigestMaxRunes)
	if digest.WhatHappened == "" || digest.WhatItNeeds == "" {
		return WorkflowDigest{}, fmt.Errorf("workflow digest: structured output fields must be non-empty")
	}
	return digest, nil
}
