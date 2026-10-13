package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sync"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmcp"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/catalog"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflowtools"
)

// ao-workflow-tools: workflow runs, definitions and campaign memory for agent
// sessions (docs/specs/agent-workflow-tools.md). It rides the thread tools'
// transport and capability URLs. The caller's authority is derived once, when
// the session registers, from the same scope the session's credential carries
// (deriveCallerScope): a phase's grants come from its run's frozen snapshot,
// so a definition edited mid-run cannot widen them.

const workflowMCPName = workflowtools.MCPServerName

// workflowMCPAccess is what one capability URL acts as: the session it was
// issued to, the scope the bound workflow methods authorize rows against, and
// the caller the tool list and the grant checks are computed from.
type workflowMCPAccess struct {
	ThreadID     string
	SessionToken string
	Scope        transport.CallerScope
	Caller       workflowtools.Caller
}

type appWorkflowMCP struct {
	once   sync.Once
	server *threadmcp.Server[workflowMCPAccess]
	tools  *workflowtools.Server
}

// WorkflowToolsAllowedTool is the Claude allowlist entry that admits every
// ao-workflow-tools call without a prompt, on both Claude providers.
const WorkflowToolsAllowedTool = "mcp__" + workflowMCPName + "__*"

func (a *App) workflowMCPServer() *threadmcp.Server[workflowMCPAccess] {
	a.workflowMCP.once.Do(func() {
		a.workflowMCP.tools = workflowtools.New(workflowToolsAdapter{a})
		server := threadmcp.New(workflowMCPName, "", func(access workflowMCPAccess) []map[string]any {
			return a.workflowMCP.tools.Tools(access.Caller)
		}, a.callWorkflowMCP)
		server.SetInstructionsFunc(func(access workflowMCPAccess) string {
			return a.workflowMCP.tools.Instructions(access.Caller)
		})
		a.workflowMCP.server = server
	})
	return a.workflowMCP.server
}

func (a *App) workflowToolsServer() *workflowtools.Server {
	a.workflowMCPServer()
	return a.workflowMCP.tools
}

// workflowMCPConfigForThread registers the thread's session and returns its
// server entry. Every Claude, claude-tui and Codex session that belongs to a
// project gets one: an interactive thread with every tool, a workflow phase or
// unit with the tools its grants and access admit. A projectless thread has no
// project to run workflows in and gets none.
func (a *App) workflowMCPConfigForThread(thread store.Thread, sessionToken string) (map[string]any, error) {
	switch thread.Provider {
	case string(provider.Claude), string(provider.ClaudeTUI), string(provider.Codex):
	default:
		return nil, nil
	}
	scope, ok, err := a.deriveCallerScope(thread)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return a.workflowMCPServer().RegisterThread(thread.ID, sessionToken, workflowMCPAccess{
		ThreadID: thread.ID, SessionToken: sessionToken, Scope: scope, Caller: workflowCaller(thread, scope),
	})
}

// workflowCaller is the tool-level authority of a thread's session.
func workflowCaller(thread store.Thread, scope transport.CallerScope) workflowtools.Caller {
	caller := workflowtools.Caller{ThreadID: thread.ID, Phase: scope.IsPhase(), Grants: scope.Grants}
	if scope.IsPhase() {
		// A phase thread's runtime mode is its declared access
		// (workflowPhaseRuntimeMode): only `write` runs with full access.
		caller.ReadOnly = thread.RuntimeMode != string(provider.RuntimeFullAccess)
	}
	return caller
}

// callWorkflowMCP runs one tool. The session must still be the one the
// capability was issued to; the bound workflow methods then read the
// session's scope off the context for row-level authorization.
func (a *App) callWorkflowMCP(w http.ResponseWriter, ctx context.Context, req threadmcp.Request, access workflowMCPAccess) {
	live, ok := a.sessionManager().get(access.ThreadID)
	if !ok || live.Token != access.SessionToken {
		threadmcp.WriteToolError(w, req.ID, errorsx.Public("workflow_session_inactive",
			"The agent session is no longer active. Resume the conversation before using workflow tools.", nil))
		return
	}
	call, err := threadmcp.DecodeToolCall(req.Params)
	if err != nil {
		threadmcp.WriteError(w, req.ID, http.StatusOK, -32602, "invalid tools/call params")
		return
	}
	ctx = transport.WithCallerScope(ctx, access.Scope)
	result, err := a.workflowToolsServer().Call(ctx, access.Caller, call.Name, call.Arguments)
	if err != nil {
		if _, _, public := errorsx.PublicDetails(err); !public {
			log.Printf("workflow tools: %s on %s: %v", call.Name, access.ThreadID, err)
			err = errorsx.Public(workflowtools.CodeFailed, "Workflow tools could not complete that call.", err)
		}
		threadmcp.WriteToolError(w, req.ID, err)
		return
	}
	threadmcp.WriteToolJSON(w, req.ID, result)
}

// withWorkflowMCPRow shows the workflow tools in a thread's MCP menu. They are
// part of every project session rather than a per-conversation choice, so the
// row carries no toggle.
func (a *App) withWorkflowMCPRow(thread store.Thread, rows []ThreadMCPServer, live bool) []ThreadMCPServer {
	switch thread.Provider {
	case string(provider.Claude), string(provider.ClaudeTUI), string(provider.Codex):
	default:
		return rows
	}
	if _, ok, err := a.deriveCallerScope(thread); err != nil || !ok {
		return slices.DeleteFunc(rows, func(row ThreadMCPServer) bool { return row.Name == workflowMCPName })
	}
	source := mcpRowSourceConfig
	if live {
		source = mcpRowSourceSession
	}
	for i := range rows {
		if rows[i].Name != workflowMCPName {
			continue
		}
		rows[i].Source = source
		rows[i].Disabled = false
		rows[i].ToggleDisabledReason = workflowToolsAlwaysOn
		return rows
	}
	return append(rows, ThreadMCPServer{
		Provider: thread.Provider, Name: workflowMCPName, Status: string(mcpstatus.StatusNotStarted),
		Source: source, ToggleDisabledReason: workflowToolsAlwaysOn,
	})
}

const workflowToolsAlwaysOn = "Workflow tools are part of every project session."

// errWorkflowToolsAlwaysOn refuses switching the workflow tools off a thread.
var errWorkflowToolsAlwaysOn = errors.New("set thread mcp server enabled: workflow tools are part of every project session")

// workflowToolsAdapter implements workflowtools.App over the bound workflow
// methods. Every call carries the session's scope on ctx (callWorkflowMCP),
// which is what the methods authorize rows against.
type workflowToolsAdapter struct{ a *App }

func (w workflowToolsAdapter) projectSlug(ctx context.Context) (string, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return "", err
	}
	project, err := w.a.store.GetProject(scope.ProjectID)
	if err != nil {
		return "", fmt.Errorf("load project %s: %w", scope.ProjectID, err)
	}
	return project.Slug, nil
}

func (w workflowToolsAdapter) ListWorkflows(ctx context.Context) (catalog.Listing, error) {
	slug, err := w.projectSlug(ctx)
	if err != nil {
		return catalog.Listing{}, err
	}
	return catalog.List(w.a.workflowDataRoot(), slug)
}

func (w workflowToolsAdapter) ValidateWorkflow(ctx context.Context, id, path string) (def.ValidationResult, error) {
	slug, err := w.projectSlug(ctx)
	if err != nil {
		return def.ValidationResult{}, err
	}
	if path != "" {
		return catalog.ValidatePath(w.a.workflowDataRoot(), slug, path)
	}
	return catalog.ValidateID(w.a.workflowDataRoot(), slug, id)
}

func (w workflowToolsAdapter) ScaffoldWorkflow(ctx context.Context, starter, id string, project bool) (catalog.ScaffoldResult, error) {
	slug, err := w.projectSlug(ctx)
	if err != nil {
		return catalog.ScaffoldResult{}, err
	}
	if !project {
		slug = ""
	}
	return catalog.Scaffold(w.a.workflowDataRoot(), starter, id, slug)
}

func (w workflowToolsAdapter) StartRun(ctx context.Context, input workflowtools.StartInput) (workflowtools.Started, error) {
	result, err := w.a.workflowAgentStartRun(ctx, WorkflowAgentStartInput{
		WorkflowID: input.WorkflowID, Scope: input.Scope, Goal: input.Goal, Seeds: input.Seeds,
		BaseBranch: input.BaseBranch, StepMode: input.StepMode,
	})
	if err != nil {
		return workflowtools.Started{}, err
	}
	return workflowtools.Started{RunID: result.ItemID, Result: result}, nil
}

func (w workflowToolsAdapter) RunStatus(ctx context.Context, runID string) (any, error) {
	return w.a.workflowAgentRunStatus(ctx, runID)
}

func (w workflowToolsAdapter) WatchRun(ctx context.Context, input workflowtools.WatchInput) (workflowtools.Watched, error) {
	result, err := w.a.workflowAgentWatchRun(ctx, WorkflowAgentWatchInput{
		ItemID: input.RunID, Cursor: input.Cursor, Tree: input.Tree,
		WaitMillis: max(input.Wait.Milliseconds(), 1),
	})
	if err != nil {
		return workflowtools.Watched{}, err
	}
	return workflowtools.Watched{
		Cursor: result.Cursor, Transitions: len(result.Transitions), Resting: result.Run.Resting, Result: result,
	}, nil
}

func (w workflowToolsAdapter) RunBoundThread(ctx context.Context, runID string) (string, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return "", err
	}
	item, err := w.a.scopedRun(scope, runID, "workflow run notify", true)
	if err != nil {
		return "", err
	}
	return item.OriginThreadID, nil
}

func (w workflowToolsAdapter) RunOutput(ctx context.Context, runID string) (any, error) {
	return w.a.workflowAgentRunOutput(ctx, runID)
}

func (w workflowToolsAdapter) InspectRun(ctx context.Context, input workflowtools.InspectInput) (any, error) {
	return w.a.workflowAgentInspectRun(ctx, WorkflowAgentInspectInput{
		ItemID: input.RunID, PhaseID: input.PhaseID, Attempt: input.Attempt,
	})
}

func (w workflowToolsAdapter) RunNarrative(ctx context.Context, input workflowtools.NarrativeInput) (any, error) {
	return w.a.workflowAgentRunNarrative(ctx, WorkflowAgentNarrativeInput{
		ItemID: input.RunID, PhaseID: input.PhaseID, Attempt: input.Attempt, UnitID: input.UnitID,
	})
}

func (w workflowToolsAdapter) ListRuns(ctx context.Context, activeOnly bool) (any, error) {
	return w.a.workflowAgentListRuns(ctx, activeOnly)
}

// WorkflowScheduledResume is run_control resume with at: the moment the
// resume armed, and the run, which stays where it is until then.
type WorkflowScheduledResume struct {
	ArmedAt string               `json:"armedAt"`
	Run     WorkflowAgentRunView `json:"run"`
}

func (w workflowToolsAdapter) ControlRun(ctx context.Context, input workflowtools.ControlInput) (any, error) {
	var err error
	switch input.Action {
	case "pause":
		err = w.a.WorkflowPauseItem(ctx, input.RunID)
	case "cancel":
		err = w.a.WorkflowCancelItem(ctx, input.RunID)
	case "resume":
		if input.At != "" {
			armed, err := w.a.WorkflowScheduleResume(ctx, input.RunID, input.At)
			if err != nil {
				return nil, err
			}
			view, err := w.a.workflowAgentRunStatus(ctx, input.RunID)
			if err != nil {
				return nil, err
			}
			return WorkflowScheduledResume{ArmedAt: armed, Run: view}, nil
		}
		err = w.a.WorkflowResumeItem(ctx, input.RunID, input.PhaseID, input.RefreshDef)
	case "rerun":
		err = w.a.WorkflowRerunItem(ctx, input.RunID, input.Guidance, input.RefreshDef)
	case "soft_stop":
		err = w.a.WorkflowRequestSoftStop(ctx, input.RunID, !input.Clear)
	case "retry_unit":
		err = w.a.WorkflowRetryUnit(ctx, input.RunID, input.UnitID, input.Note)
	case "retry_failed_units":
		err = w.a.WorkflowRetryFailedUnits(ctx, input.RunID, input.Note)
	default:
		return nil, fmt.Errorf("unknown run action %q", input.Action)
	}
	if err != nil {
		return nil, err
	}
	// What state the run is in now is the only useful answer to acting on it.
	return w.a.workflowAgentRunStatus(ctx, input.RunID)
}

func (w workflowToolsAdapter) AmendSeeds(ctx context.Context, runID string, seeds json.RawMessage) (any, error) {
	return w.a.workflowAgentAmendSeeds(ctx, WorkflowAgentAmendSeedsInput{ItemID: runID, Seeds: seeds})
}

func (w workflowToolsAdapter) GuideRun(ctx context.Context, runID, text string) (any, error) {
	return w.a.workflowAgentGuideRun(ctx, WorkflowAgentGuideRunInput{ItemID: runID, Text: text})
}

func (w workflowToolsAdapter) ResolveRun(ctx context.Context, input workflowtools.ResolveInput) (any, error) {
	var err error
	switch input.Decision {
	case "approve", "reject":
		err = w.a.WorkflowResolveGate(ctx, input.RunID, input.Decision, input.Note)
	case "answer":
		err = w.a.WorkflowAnswerQuestion(ctx, input.RunID, input.Answer)
	default:
		err = errors.New("decision must be approve, reject or answer")
	}
	if err != nil {
		return nil, err
	}
	return w.a.workflowAgentRunStatus(ctx, input.RunID)
}

func (w workflowToolsAdapter) AddMemory(ctx context.Context, input workflowtools.MemoryInput) (any, error) {
	return w.a.workflowAgentAddMemory(ctx, WorkflowAgentMemoryInput{
		ItemID: input.RunID, Kind: input.Kind, Text: input.Text, Files: input.Files,
	})
}

func (w workflowToolsAdapter) ListMemory(ctx context.Context, runID, kind string) (any, error) {
	return w.a.workflowAgentListMemory(ctx, WorkflowAgentMemoryListInput{ItemID: runID, Kind: kind})
}

var _ workflowtools.App = workflowToolsAdapter{}

// workflowToolsInstructions is the guide a Codex session receives in its
// developer instructions, computed from the same caller its registration
// carries. Codex never shows a server's instructions to the model.
func (a *App) workflowToolsInstructions(threadID string) string {
	// A guide that named tools the session does not have would be worse than
	// none, so a thread whose authority cannot be read gets none; its calls
	// are refused by the same derivation.
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		log.Printf("workflow tools: guide for %s: %v", threadID, err)
		return ""
	}
	scope, ok, err := a.deriveCallerScope(thread)
	if err != nil || !ok {
		if err != nil {
			log.Printf("workflow tools: guide for %s: %v", threadID, err)
		}
		return ""
	}
	return a.workflowToolsServer().Instructions(workflowCaller(thread, scope))
}

// claudeTUIMCPServers is what the interactive provider is handed out of the
// servers a session registered: the workflow tools and nothing else of the
// app's, with their allowlist entry. Whether claude-tui hosts the thread,
// browser or remote tools is a separate decision (docs/decisions.md).
func claudeTUIMCPServers(registered map[string]any) (map[string]any, []string) {
	entry := registered[workflowMCPName]
	if entry == nil {
		return nil, nil
	}
	return map[string]any{workflowMCPName: entry}, []string{WorkflowToolsAllowedTool}
}
