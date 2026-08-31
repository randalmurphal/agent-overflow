package app

import (
	"agent-overflow/internal/aocli"
	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/memory"
	workflowrunner "agent-overflow/internal/workflow/runner"
	"agent-overflow/internal/workflowhost"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/entityid"
)

// workflowHostAdapter is `internal/app`'s implementation of the workflow runner's host
// seams (`internal/workflowhost/host.go`). Every method is a forward to the
// App's own, unexported one and nothing else: an interface declared outside
// `internal/app` cannot name an unexported method, and exporting ~19 App methods
// for this one consumer would ripple through this package far further than
// these forwards do. Behavior belongs on the App methods; this file must stay
// pure glue.
type workflowHostAdapter struct{ app *App }

var _ workflowhost.Host = workflowHostAdapter{}

func (h workflowHostAdapter) StartSession(ctx context.Context, threadID string) error {
	return h.app.startSession(ctx, threadID)
}

func (h workflowHostAdapter) StartSessionTakingLock(ctx context.Context, threadID string) error {
	return h.app.startSessionTakingLock(ctx, threadID)
}

func (h workflowHostAdapter) StopSession(threadID string) error {
	return h.app.stopSession(threadID)
}

func (h workflowHostAdapter) SessionActive(threadID string) bool {
	_, active := h.app.sessionManager().get(threadID)
	return active
}

func (h workflowHostAdapter) SubscribeThreadTurnObserver(
	threadID string, observer func(string, provider.ProviderEvent),
) func() {
	return h.app.subscribeThreadTurnObserver(threadID, observer)
}

func (h workflowHostAdapter) SendWorkflowMessage(
	ctx context.Context, threadID, content string,
	outputSchema json.RawMessage, onDispatch func(workflowhost.DispatchIdentity),
) error {
	return h.app.sendWorkflowMessage(ctx, threadID, content, outputSchema, onDispatch)
}

func (h workflowHostAdapter) CreateWorkflowThread(spec workflowhost.ThreadSpec) (store.Thread, error) {
	return h.app.createWorkflowThread(spec)
}

func (h workflowHostAdapter) ThreadAssistantTexts(threadID string) ([]string, error) {
	return h.app.threadAssistantTexts(threadID)
}

func (h workflowHostAdapter) GitCore() *gitops.Core { return h.app.gitCore() }

func (h workflowHostAdapter) FindWorktree(project, path string) (gitops.Worktree, bool, error) {
	return h.app.findWorktree(project, path)
}

func (h workflowHostAdapter) CutWorktreeFromFreshBase(
	ctx context.Context, projectPath, worktreePath, baseBranch, newBranch string,
) error {
	return h.app.cutWorktreeFromFreshBase(ctx, projectPath, worktreePath, baseBranch, newBranch)
}

func (h workflowHostAdapter) DefaultWorktreePath(projectPath, branch string) (string, error) {
	return h.app.defaultWorktreePath(projectPath, branch)
}

func (h workflowHostAdapter) WorktreeBranchPrefix() string { return h.app.worktreeBranchPrefix() }

func (h workflowHostAdapter) WorkflowPromptAncestry(
	itemID string, workflow def.Workflow,
) workflowrunner.PromptContext {
	return h.app.workflowPromptAncestry(itemID, workflow)
}

func (h workflowHostAdapter) RecordEnvelopeMemory(key engine.RunKey, drafts []memory.Draft) {
	h.app.recordEnvelopeMemory(key, drafts)
}

func (h workflowHostAdapter) Emit(name eventchan.Channel, data any) { h.app.emit(name, data) }

func (h workflowHostAdapter) RequireWorkflowEngine() (*engine.Engine, error) {
	return h.app.requireWorkflowEngine()
}

func (h workflowHostAdapter) LifeCtx() context.Context { return h.app.lifeCtx() }

func (h workflowHostAdapter) ClaudeProjectsDir() (string, error) {
	return h.app.claudeProjectsDir()
}

// newWorkflowAppRunner builds the workflow runner against this App. It is the
// one place the adapter is constructed, so nothing else in `main` has to know
// the seam exists.
func newWorkflowAppRunner(app *App, dataRoot string, profiles engine.ProfileSource) *workflowhost.Runner {
	return workflowhost.New(
		workflowHostAdapter{app: app}, app.store, dataRoot, profiles, app.interruptTurnCtx,
	)
}

// WorkflowArtifact is one app-managed file deliverable copied from a phase
// workspace. Files are discovered from the deterministic per-item directory.
type WorkflowArtifact struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
}

func (a *App) workflowDataRoot() string {
	if root := a.workflowApplication().DataRoot(); strings.TrimSpace(root) != "" {
		return root
	}
	return a.configDir
}

// listWorkflowArtifacts is the wire-model adapter over workflowhost's
// filesystem-owned artifact listing.
func listWorkflowArtifacts(dataRoot, itemID string) ([]WorkflowArtifact, error) {
	listed, err := workflowhost.ListArtifacts(dataRoot, itemID)
	if err != nil {
		return nil, err
	}
	artifacts := make([]WorkflowArtifact, len(listed))
	for index, artifact := range listed {
		artifacts[index] = WorkflowArtifact{
			Name: artifact.Name, Path: artifact.Path, Size: artifact.Size, ModTime: artifact.ModTime,
		}
	}
	return artifacts, nil
}

// The `/workflow` composer context (spec §5, D15/D31). Sending a message that
// starts with `/workflow` appends a text block telling the agent that the
// `agent-overflow` command exists, that its credentials are already in the
// environment, which workflows this project has, and what is already running.
//
// The block's format is owned by internal/aocli (pure, unit-tested); this
// resolver produces the live data behind it. It is NOT a bound method: the
// block never reaches the frontend — the send path
// (app_composer_commands.go) is its only caller, and it appends the block to
// the provider payload only.

// workflowComposerBlock renders the `/workflow` block for one thread.
func (a *App) workflowComposerBlock(threadID string) (string, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return "", fmt.Errorf("workflow composer context: thread id is required")
	}
	if a.store == nil {
		return "", fmt.Errorf("workflow composer context: store unavailable")
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", err
	}
	context := aocli.ComposerContext{
		SessionReady: len(a.sessionAOEnv(threadID)) > 0,
		// Boot publishes the command under its canonical name and hands the
		// directory to every session's PATH (D30); an empty cliBinDir means
		// that failed and the block has to say so.
		CommandOnPath: a.cliBinDir != "",
	}
	slug := ""
	if strings.TrimSpace(thread.ProjectID) != "" {
		projectRow, err := a.store.GetProject(thread.ProjectID)
		if err != nil {
			return "", err
		}
		context.ProjectName = projectRow.Name
		slug = projectRow.Slug
		context.ProjectSlug = slug
	}
	context.SharedDir, context.ProjectDir = aocli.WorkflowSourceDirs(a.workflowDataRoot(), slug)

	resolved, err := aocli.ResolveConfigured(a.workflowDataRoot(), slug)
	if err != nil {
		return "", fmt.Errorf("workflow composer context: %w", err)
	}
	context.Workflows = make([]aocli.ComposerWorkflow, 0, len(resolved))
	for _, workflow := range resolved {
		context.Workflows = append(context.Workflows, aocli.ComposerWorkflow{
			ID: workflow.Workflow.ID, Name: workflow.Workflow.Name, Scope: string(workflow.Scope),
		})
	}

	if thread.ProjectID != "" {
		runs, err := a.store.ListWorkItemSummaries(store.WorkItemListFilter{
			ProjectID: thread.ProjectID,
			States:    []string{string(engine.StateRunning), string(engine.StateNeedsHuman)},
		})
		if err != nil {
			return "", err
		}
		// Newest first: a composer block is read top-down, and the run someone is
		// about to ask about is almost always the most recent one.
		context.Runs = make([]aocli.ComposerRun, 0, len(runs))
		for i := len(runs) - 1; i >= 0; i-- {
			context.Runs = append(context.Runs, aocli.ComposerRun{
				ItemID: runs[i].ID, WorkflowID: runs[i].WorkflowID, State: runs[i].State,
				Reason: runs[i].Reason, PhaseID: runs[i].CurrentPhaseID,
			})
		}
	}
	return aocli.RenderComposerContext(context), nil
}

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
	if item.State == string(engine.StateDone) {
		digest.WhatHappened = "The workflow run completed."
		digest.WhatItNeeds = "Review the completed work and decide the next step."
		return digest
	}
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
		case engine.ReasonCheckpoint:
			// The one park that is not a problem: the run did exactly what it was
			// told. Saying "paused because …" here would read as a fault report
			// for the human's own instruction.
			digest.WhatHappened = "The run stopped at the checkpoint you asked for, before starting the next call."
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
	case engine.ReasonProviderUsageLimited:
		digest.WhatItNeeds = "Wait for the provider usage limit to reset or switch provider accounts, then resume the run."
	case engine.ReasonCheckpoint:
		digest.WhatItNeeds = "Resume the run to continue, or leave it stopped."
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
		return "provider retries or a workflow loop limit were exhausted"
	case engine.ReasonProviderRetriesExhausted:
		return "provider retries were exhausted"
	case engine.ReasonProviderUsageLimited:
		return "the provider account reached its usage limit"
	case engine.ReasonLoopLimitExhausted:
		return "the workflow loop limit was exhausted"
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

func (a *App) generateWorkflowDigest(item store.WorkItem, template WorkflowDigest) (WorkflowDigest, error) {
	primary := a.resolveTextGenerationConfig()
	return runTextGenWithFallback(a, primary, workflowDigestTimeout, func(cfg textgen.Config, deadline time.Time) (WorkflowDigest, error) {
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
		raw, err = textgen.RunCodex(ctx, cfg, workspace, workflowDigestSchemaJSON, nil, prompt, workflowDigestTimeout)
	case string(provider.Claude):
		raw, err = textgen.RunClaude(ctx, cfg, workspace, workflowDigestSchemaJSON, nil, prompt, workflowDigestTimeout)
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

// Campaign memory (Packet L). A run tree accumulates what its work learned so
// later waves stop relearning it, and every element of the tree gets a bounded
// digest of that in its prompt.
//
// WRITE OWNERSHIP IS THE APP'S, entirely. The engine carries no memory config
// and holds no writer interface: it already does not know where a narrative
// file goes, where a worktree is cut, or where the config root is, and the two
// channels a note arrives on are both app-side seams — the scoped RPC the CLI
// speaks, and the envelope-lift `finish` already performs for `narrative`. The
// pure half (what a note is, where the log lives, how it renders) is
// `internal/workflow/memory`; this file is the run-shaped half: which tree a
// note belongs to, what provenance it is stamped with, and who may write it.
//
// The tree is keyed by the ROOT run because a campaign is the tree, not the run:
// a recursive self-calling spine and every lane it fans out to share one memory,
// which is the whole point.

// workflowMemoryTree is one run's resolved memory coordinates.
type workflowMemoryTree struct {
	// RootID is the run tree's root, which names the directory.
	RootID string
	// NotesPath is the append-only log.
	NotesPath string
	// Wave is the writing run's caller-chain depth relative to the root. It is
	// the engine's own `call_depth`, read off the row rather than recounted.
	Wave int
}

// workflowMemoryTreeFor resolves the memory tree one run writes into and reads
// from. Linkage is immutable, so this is a stable fact about the run.
func (a *App) workflowMemoryTreeFor(item store.WorkItem) (workflowMemoryTree, error) {
	ancestry, err := a.workflowApplication().Ancestry(item)
	if err != nil {
		return workflowMemoryTree{}, fmt.Errorf("workflow memory tree for run %s: %w", item.ID, err)
	}
	return a.workflowMemoryTreeOf(ancestry)
}

// workflowMemoryTreeOf is the same answer from an ancestry the caller has
// ALREADY walked. Prompt assembly needs the goal chain and this tree from one
// linkage walk; resolving them independently paid for a depth-forty walk twice
// per element, which is what `workflowMemoryDigest`'s "this side never repeats
// it" was always meant to describe.
func (a *App) workflowMemoryTreeOf(ancestry []store.WorkItem) (workflowMemoryTree, error) {
	if len(ancestry) == 0 {
		return workflowMemoryTree{}, fmt.Errorf("workflow memory tree: empty ancestry")
	}
	root, item := ancestry[0], ancestry[len(ancestry)-1]
	notesPath, err := memory.NotesPath(a.workflowDataRoot(), root.ID)
	if err != nil {
		return workflowMemoryTree{}, err
	}
	return workflowMemoryTree{RootID: root.ID, NotesPath: notesPath, Wave: item.CallDepth}, nil
}

// workflowMemoryDigest renders the campaign-memory block one element's prompt
// carries, from the tree its run resolved to. It is called on every agent
// phase, unit, join, and takeover finalize, through `workflowPromptAncestry` —
// which is what owns the ancestry walk the tree comes from, so this side never
// repeats it.
//
// A failure to read is LOGGED and yields an empty digest rather than failing the
// attempt. Memory is context, not contract: an element that runs without it does
// the work with less to go on, while an element that never starts does none —
// and a run whose config root moved would otherwise be unable to take a single
// turn.
func (a *App) workflowMemoryDigest(tree workflowMemoryTree) workflowrunner.MemoryDigest {
	notes, skipped, err := memory.ReadNotes(tree.NotesPath)
	if err != nil {
		log.Printf("workflow memory: read %s: %v", tree.NotesPath, err)
		return ""
	}
	for _, entry := range skipped {
		log.Printf("workflow memory: %s line %d skipped: %s", tree.NotesPath, entry.Line, entry.Reason)
	}
	return workflowrunner.MemoryDigest(memory.Render(notes, memory.RenderOptions{
		NotesPath: tree.NotesPath, Budget: memory.DefaultInjectionBytes,
	}))
}

// recordWorkflowMemory appends validated drafts to a run's tree with app-stamped
// provenance. It is the ONE writer: both channels — the CLI verb and the
// envelope lift — land here, so neither can stamp a note the other could not.
func (a *App) recordWorkflowMemory(item store.WorkItem, provenance memory.Provenance, drafts []memory.Draft) (int, error) {
	if len(drafts) == 0 {
		return 0, nil
	}
	tree, err := a.workflowMemoryTreeFor(item)
	if err != nil {
		return 0, err
	}
	provenance.RunID = item.ID
	provenance.Wave = tree.Wave
	now := time.Now().UnixMilli()
	written := 0
	for _, draft := range drafts {
		note, err := memory.NewNote(draft, provenance, now)
		if err != nil {
			return written, err
		}
		if err := memory.Append(tree.NotesPath, note); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// recordEnvelopeMemory lands the notes an element carried in its envelope's
// `memory` field. It runs at the same seam the narrative is lifted at, so an
// element that ended its turn correctly has its notes recorded before anything
// downstream sees the envelope — and a failure here never fails the attempt,
// for the same reason a missing digest does not: the work is done and the
// envelope is valid, and losing an unrecordable note is strictly better than
// losing the turn that produced it.
func (a *App) recordEnvelopeMemory(key engine.RunKey, drafts []memory.Draft) {
	if len(drafts) == 0 {
		return
	}
	item, err := a.store.GetWorkItem(key.ItemID)
	if err != nil {
		log.Printf("workflow memory: load run %s for envelope notes: %v", key.ItemID, err)
		return
	}
	provenance := memory.Provenance{PhaseID: key.PhaseID, Attempt: key.Attempt, UnitID: key.UnitID}
	written, err := a.recordWorkflowMemory(item, provenance, drafts)
	if err != nil {
		log.Printf("workflow memory: record %d envelope notes for %s/%s: %v",
			len(drafts)-written, key.ItemID, key.PhaseID, err)
	}
}

// removeWorkflowMemoryTree deletes one root run's memory directory. It is
// called exactly where run RECORDS are deleted (project deletion), because the
// tree is that history's companion: a directory whose root run no longer exists
// is unreachable by every read verb and by every injection.
//
// Discard deliberately does NOT call it. Discard removes worktrees and branches
// and leaves the run rows in place, so the memory a discarded campaign
// accumulated stays readable exactly as its narratives and envelopes do.
func (a *App) removeWorkflowMemoryTree(rootRunID string) error {
	dir, err := memory.Dir(a.workflowDataRoot(), rootRunID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove workflow memory %s: %w", rootRunID, err)
	}
	return nil
}

// workflowMemoryProvenance resolves the element coordinates behind a scoped
// token. A phase token names its (item, phase); the ATTEMPT and the unit are
// facts about the live session, so they come off the row the session's thread is
// attached to — a unit row first, exactly as `workflowPhaseForThread` does,
// because a unit thread has no phase row of its own.
//
// A session whose rows have gone (a torn-down attempt still holding a token)
// yields the phase coordinate with no attempt rather than an error: the note is
// still this phase's, and refusing to record it would lose a lesson over a
// missing line number.
func (a *App) workflowMemoryProvenance(threadID, phaseID string) memory.Provenance {
	provenance := memory.Provenance{PhaseID: phaseID}
	unit, found, err := a.store.GetWorkItemUnitByThread(threadID)
	if err != nil {
		log.Printf("workflow memory: resolve unit for thread %s: %v", threadID, err)
		return provenance
	}
	if found {
		provenance.UnitID = unit.UnitID
		provenance.Attempt = unit.Attempt
		return provenance
	}
	phase, found, err := a.store.GetWorkItemPhaseByThread(threadID)
	if err != nil {
		log.Printf("workflow memory: resolve phase for thread %s: %v", threadID, err)
		return provenance
	}
	if found {
		provenance.Attempt = phase.Attempt
	}
	return provenance
}

// describeMemoryFindings renders draft validation for a CLI caller. The CLI
// prints the error verbatim, so it has to name every rule broken and the value
// that broke it.
func describeMemoryFindings(action string, err error) error {
	var validation *memory.ValidationError
	if !errors.As(err, &validation) {
		return err
	}
	parts := make([]string, 0, len(validation.Findings))
	for _, finding := range validation.Findings {
		parts = append(parts, strings.TrimPrefix(finding.Path, ".")+" "+finding.Message)
	}
	return fmt.Errorf("%s: %s", action, strings.Join(parts, "; "))
}

// workflowAssistantTextKind is the item kind a provider's prose is persisted
// under. The literal matches the triage write path's own constant; store exports
// no item-kind vocabulary, and inventing one for a single reader would widen
// that package's surface for nothing.
const workflowAssistantTextKind = "assistant_text"

// threadAssistantTexts returns one thread's top-level assistant prose, oldest
// first. Subagent rows (`parent_id` set) are excluded: an element's narrative is
// what it said, not what something it launched said.
func (a *App) threadAssistantTexts(threadID string) ([]string, error) {
	if threadID == "" {
		return nil, errors.New("thread id is required")
	}
	items, err := a.store.ListItems(threadID)
	if err != nil {
		return nil, err
	}
	texts := make([]string, 0, 4)
	for _, item := range items {
		if item.Kind != workflowAssistantTextKind || item.ParentID != "" || item.Summary == "" {
			continue
		}
		texts = append(texts, item.Summary)
	}
	return texts, nil
}

// The app-resolved half of prompt assembly (Packet O). Two of the blocks every
// element's prompt carries are facts about the run TREE rather than about the
// element: the campaign-memory digest, which is keyed on the tree's root, and
// the goal chain, which IS the tree's call linkage. Both need the same ancestry
// walk, so they are resolved together — walking twice per element would pay for
// the same parent rows once each.
//
// Everything here is best effort. A failure to resolve is LOGGED and yields the
// blocks it could build rather than failing the attempt: the goal chain and the
// memory digest are context, not contract, and an element that runs without
// them does the work with less to go on while an element that never starts does
// none.

// workflowPromptAncestry resolves the tree-shaped prompt blocks for one run.
// `workflow` is the definition the run is executing — the caller already holds
// it on the run request, and it is where THIS run's non-goals come from, so
// re-decoding the run's own snapshot to read them would be a second answer to a
// question already in hand.
func (a *App) workflowPromptAncestry(itemID string, workflow def.Workflow) workflowrunner.PromptContext {
	// Summary, not the full row: nothing this file reads about the run ITSELF
	// lives in the snapshot, and this resolves once per element — every unit of
	// every wave, on the path that starts its turn.
	item, err := a.store.GetWorkItemSummary(itemID)
	if err != nil {
		// The definition is in hand, so its non-goals still stand: they are
		// def-owned, and dropping the run's stated boundaries because a row read
		// failed would lose the one part of the block that never needed the store.
		log.Printf("workflow prompt context: load run %s: %v", itemID, err)
		return workflowrunner.PromptContext{Goals: a.workflowGoalChain(nil, workflow)}
	}
	ancestry, err := a.workflowApplication().Ancestry(item)
	if err != nil {
		// The run's own facts still stand: it is its own root as far as anything
		// resolvable goes, and the non-goals it must respect are on the
		// definition in hand. Falling back to them beats dropping the block.
		log.Printf("workflow prompt context: resolve ancestry of run %s: %v", itemID, err)
		ancestry = []store.WorkItem{item}
	}
	context := workflowrunner.PromptContext{Goals: a.workflowGoalChain(ancestry, workflow)}
	// The tree comes from the ancestry just walked. Both blocks are facts about
	// the same call linkage, and resolving the tree from the run instead would
	// walk it a second time — the parent rows are already in hand.
	tree, err := a.workflowMemoryTreeOf(ancestry)
	if err != nil {
		log.Printf("workflow memory: %v", err)
		return context
	}
	context.Memory = a.workflowMemoryDigest(tree)
	return context
}

// workflowGoalChain assembles the goals of the call chain root-first plus the
// non-goals in force.
//
// CONSECUTIVE runs sharing one goal collapse into a single link. The engine
// copies a caller's goal onto every run it calls (`invokeCall`), so a forty-wave
// campaign's chain is forty copies of one sentence; rendering them all would
// cost every element forty times the bytes to say exactly what one link says.
// The link keeps the ROOT-most run that stated the goal, because that is the run
// the goal was actually recorded on.
func (a *App) workflowGoalChain(ancestry []store.WorkItem, workflow def.Workflow) workflowrunner.GoalChain {
	// The non-goals come off the definition rather than the ancestry, so an
	// unresolvable chain still carries them.
	chain := workflowrunner.GoalChain{NonGoals: workflow.NonGoals, WorkflowID: workflow.ID}
	if len(ancestry) == 0 {
		return chain
	}
	root, current := ancestry[0], ancestry[len(ancestry)-1]
	for index, run := range ancestry {
		goal := strings.TrimSpace(run.Goal)
		if goal == "" {
			continue
		}
		if last := len(chain.Links) - 1; last >= 0 && chain.Links[last].Goal == goal {
			continue
		}
		chain.Links = append(chain.Links, workflowrunner.GoalLink{
			RunID: run.ID, WorkflowID: run.WorkflowID, Goal: goal,
			Root: index == 0, Current: run.ID == current.ID,
		})
	}
	if root.ID == current.ID {
		return chain
	}
	// The root's non-goals bind this run too — it is executing inside the
	// campaign that root started. They are carried only when they DIFFER,
	// because a recursive campaign whose every wave runs one definition would
	// otherwise print the same list twice under two headings.
	//
	// This is the ONE snapshot the block needs, and it is read here — for the
	// root alone, only when the root is not this run — rather than by making the
	// ancestry walk carry a frozen definition for every ancestor it touched.
	rootRow, err := a.store.GetWorkItem(root.ID)
	if err != nil {
		log.Printf("workflow prompt context: load root %s for its non-goals: %v", root.ID, err)
		return chain
	}
	rootWorkflow, ok := workflowSnapshotWorkflow(rootRow)
	if !ok || len(rootWorkflow.NonGoals) == 0 || slices.Equal(rootWorkflow.NonGoals, workflow.NonGoals) {
		return chain
	}
	chain.RootNonGoals, chain.RootWorkflowID = rootWorkflow.NonGoals, rootWorkflow.ID
	return chain
}

// workflowSnapshotWorkflow decodes the definition a run froze at start. A run
// with no snapshot (one that never got past admission) and one whose snapshot
// will not decode both report false: the caller is reading context, and a
// missing block is strictly better than a failed attempt.
func workflowSnapshotWorkflow(item store.WorkItem) (def.Workflow, bool) {
	if len(item.Snapshot) == 0 {
		return def.Workflow{}, false
	}
	var snapshot engine.Snapshot
	if err := json.Unmarshal(item.Snapshot, &snapshot); err != nil {
		log.Printf("workflow prompt context: decode snapshot of run %s: %v", item.ID, err)
		return def.Workflow{}, false
	}
	return snapshot.Workflow, true
}

// createWorkflowThread creates the AO thread one workflow turn runs on.
func (a *App) createWorkflowThread(spec workflowhost.ThreadSpec) (store.Thread, error) {
	if !provider.CapabilitiesForProvider(spec.ProviderName).EnforcesRuntimeMode {
		// The element declares an access level the provider's session config
		// cannot apply. Refusing here is the point: starting anyway would run
		// unattended work with its `access` declaration silently inert, which is
		// the exact hole D22 closes. Typed as a wiring error — the frozen
		// definition and the runtime cannot produce the work it describes — so
		// the item parks with an actionable reason.
		return store.Thread{}, fmt.Errorf(
			"%w: workflow runner: %s declares access %q but provider %q does not enforce runtime modes",
			engine.ErrWiringFailed, spec.Label, spec.Access, spec.ProviderName,
		)
	}
	workspace := spec.Workspace.Path
	if strings.TrimSpace(workspace) == "" {
		return store.Thread{}, fmt.Errorf("workflow runner: %s has no workspace", spec.Label)
	}
	model := provider.NormalizeModelSlug(spec.ProviderName, spec.Model)
	// A workflow lane's model settings come from the catalog's defaults for
	// (provider, model) plus what the definition authored — deliberately NOT from
	// `chat_model_profiles`. Seeding from the last-remembered CHAT profile would
	// make a phase's reasoning effort, context window, and fast mode depend on
	// unrelated interactive use of the same model, so the same run could behave
	// differently on two machines, or on one machine a week apart.
	seed := chatmodel.FallbackProfile(spec.ProviderName, model)
	effort := seed.ReasoningEffort
	if authored := strings.TrimSpace(spec.Effort); authored != "" {
		// Validation accepted the tier as a NAME; whether this model advertises it
		// is a catalog question, so it is answered here and coerced onto the
		// model's own default when the answer is no. `threads.reasoning_effort` is
		// NOT NULL under a per-provider CHECK — what persists is always a legal
		// tier, and an argv builder is what decides a model with no tiers at all
		// gets no flag (see internal/provider/AGENTS.md §Model catalogs).
		effort = a.coerceReasoningEffortForModel(spec.ProviderName, model, authored)
	}
	now := time.Now().UnixMilli()
	thread := store.Thread{
		// Globally unique by construction (internal/entityid).
		ID: entityid.New(), ProjectID: spec.Workspace.Project.ID, ProjectPath: spec.Workspace.Project.Path,
		Title: spec.Title, Provider: spec.ProviderName,
		Model:         model,
		WorkspacePath: workspace, Mode: "workflow",
		ReasoningEffort: effort, FastMode: seed.FastMode,
		ContextWindow: seed.ContextWindow,
		RuntimeMode:   string(workflowPhaseRuntimeMode(spec.Access)),
		CreatedAt:     now, UpdatedAt: now,
	}
	if !gitops.SameFilesystemPath(workspace, spec.Workspace.Project.Path) {
		if spec.Workspace.Branch == "" {
			return store.Thread{}, fmt.Errorf(
				"workflow runner: %s runs in worktree %q with no branch recorded", spec.Label, workspace,
			)
		}
		thread.WorktreePath = workspace
		thread.Branch = spec.Workspace.Branch
	}
	// sanitizeThreadModelSettings does not touch RuntimeMode (see its doc
	// comment), so the access mapping set above survives it.
	thread = a.sanitizeThreadModelSettings(thread)
	// The engine runs this thread, so there is no screen to attribute it to:
	// stampThreadCreation records the workspace's git coordinates and leaves
	// the device empty, which is the true answer for engine-created work.
	a.stampThreadCreation(context.Background(), &thread)
	if err := a.store.CreateThread(thread); err != nil {
		return store.Thread{}, fmt.Errorf("workflow runner: create thread for %s: %w", spec.Label, err)
	}
	return a.store.GetThread(thread.ID)
}

// workflowPhaseRuntimeMode maps a phase's or unit's effective access declaration
// onto the provider session's runtime mode (decision D22, spec §9). This is the
// single translation point: the thread row it feeds is the source of truth that
// ThreadView → SessionOptions derives from, so restarts, resumes, and
// Answer-continuations all inherit the declaration without re-deriving it.
//
// `write` gets full access rather than a supervised tier because writing work
// already runs inside its own isolated workspace and there is nobody present to
// answer a prompt; `read-only` gets the restricted mode, which denies mutations
// outright instead of asking about them.
func workflowPhaseRuntimeMode(access def.Access) provider.RuntimeMode {
	if access == def.AccessWrite {
		return provider.RuntimeFullAccess
	}
	return provider.RuntimeReadOnly
}

// workflowOpenTriageThread opens or returns the item-linked hand-off thread,
// seeding a newly created thread as its first user turn so work starts
// immediately.
//
// Not a bound method: D32 removed every UI affordance that spawned a thread
// from a workflow surface. It survives as the PR follow-up surfaces' thread
// (`WorkflowSendPRReviewCommentsToThread`, `WorkflowDiscussPR`, §4.7) — those
// ride the run's ONE linked conversation, and this is what makes it one.
func (a *App) workflowOpenTriageThread(itemID string) (store.Thread, error) {
	return a.workflowApplication().OpenTriageThread(itemID)
}

func (a *App) newWorkflowTriageThread(threadID string, project store.Project, workspace, branch, title, providerName, model string) store.Thread {
	seed := a.seedChatModelProfile(providerName, model)
	now := time.Now().UnixMilli()
	thread := store.Thread{
		ID: threadID, ProjectID: project.ID, ProjectPath: project.Path,
		Title: title, Provider: seed.Provider, Model: seed.Model,
		WorkspacePath: workspace, Mode: threadmode.ModeWorkflowTriage,
		ReasoningEffort: seed.ReasoningEffort, FastMode: seed.FastMode,
		ContextWindow: seed.ContextWindow,
		// The thread works the run's own worktree, which the engine already
		// wrote to autonomously; asking for approvals it never asked for
		// during the run would be theatre.
		RuntimeMode: string(provider.RuntimeFullAccess),
		CreatedAt:   now, UpdatedAt: now,
	}
	if !gitops.SameFilesystemPath(workspace, project.Path) {
		thread.WorktreePath = workspace
		thread.Branch = branch
	}
	a.stampThreadCreation(context.Background(), &thread)
	return a.sanitizeThreadModelSettings(thread)
}

func (a *App) workflowTriageSeed(item store.WorkItem, phases []store.WorkItemPhase, workspace string) (string, error) {
	return a.workflowApplication().TriageSeed(item, phases, workspace)
}
