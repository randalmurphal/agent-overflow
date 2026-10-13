// Package workflowtools is the ao-workflow-tools contract: the tool schemas,
// which tools a calling session may see and call, argument validation, the
// bounded waits, and the model-facing instructions. The product contract is
// docs/specs/agent-workflow-tools.md.
//
// It depends on the App interface below and never on internal/app or the
// store. The App carries the caller's authority to the bound workflow methods,
// which enforce which runs a caller may touch; this package enforces which
// tools it may call at all.
package workflowtools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/mcpargs"
	"agent-overflow/internal/workflow/def"
)

// MCPServerName is the server's name in every provider's MCP configuration.
const MCPServerName = "ao-workflow-tools"

// Error codes. Every refusal the model can act on has a stable code; raw
// causes stay in host logs.
const (
	// CodeInvalidRequest is an argument refused before any work started.
	CodeInvalidRequest = "workflow_invalid_request"
	// CodeGrantRequired is a tool the calling phase was not granted. It names
	// the grant, because the fix is the workflow author's.
	CodeGrantRequired = "workflow_grant_required"
	// CodeReadOnly is a tool that writes, called from a read-only phase.
	CodeReadOnly = "workflow_read_only"
	// CodeFailed is a call the app refused; its message is the app's own.
	CodeFailed = "workflow_call_failed"
)

// Caller is the authority one call runs under, resolved by the app from the
// session that holds the capability URL. It is never read from arguments.
type Caller struct {
	// ThreadID is the calling thread.
	ThreadID string
	// Phase is set for a workflow phase or unit session; otherwise the caller
	// is an interactive thread with every tool.
	Phase bool
	// Grants are the phase's frozen grants.
	Grants []string
	// ReadOnly is a phase whose access is read-only. Tools that write files
	// or campaign memory are withheld from it: such a phase records memory in
	// its envelope instead.
	ReadOnly bool
}

// Allows reports whether caller may see and call the named tool.
func (c Caller) Allows(name string) bool {
	spec, ok := lookupTool(name)
	return ok && c.refusal(spec) == nil
}

// refusal is nil when the caller may call spec, else the public error that
// says why not and what would change it.
func (c Caller) refusal(spec toolSpec) error {
	if !c.Phase {
		return nil
	}
	if spec.writes && c.ReadOnly {
		return errorsx.Public(CodeReadOnly, fmt.Sprintf(
			"%s writes, and this phase runs read-only. Record campaign memory in your envelope's memory field instead.", spec.name), nil)
	}
	if len(spec.grants) == 0 {
		return nil
	}
	for _, grant := range spec.grants {
		if slices.Contains(c.Grants, string(grant)) {
			return nil
		}
	}
	return errorsx.Public(CodeGrantRequired, fmt.Sprintf(
		"This phase was not granted %s, which %s needs. The workflow author adds it to the phase's grants: to allow it.",
		describeGrants(spec.grants), spec.name), nil)
}

func describeGrants(grants []def.Grant) string {
	quoted := make([]string, 0, len(grants))
	for _, grant := range grants {
		quoted = append(quoted, fmt.Sprintf("%q", grant))
	}
	return strings.Join(quoted, " or ")
}

// Server is the ao-workflow-tools contract bound to one app.
type Server struct {
	app App
	// now is the clock waits are measured against. Tests replace it.
	now func() time.Time
}

// New returns a server over app.
func New(app App) *Server { return &Server{app: app, now: time.Now} }

// Tools returns the schemas caller may see, in table order. A phase sees only
// the tools its grants and access admit; an interactive caller sees all.
func (s *Server) Tools(caller Caller) []map[string]any {
	tools := make([]map[string]any, 0, len(toolTable))
	for _, spec := range toolTable {
		if caller.refusal(spec) == nil {
			tools = append(tools, spec.schema())
		}
	}
	return tools
}

// Call runs one tool for caller and returns the value the transport marshals.
// A tool outside the caller's grants is refused by name, never narrowed. Every
// error it returns is public: the app's own refusals carry its message.
func (s *Server) Call(ctx context.Context, caller Caller, name string, args json.RawMessage) (any, error) {
	if s == nil || s.app == nil {
		return nil, errorsx.Public(CodeInvalidRequest, "Workflow tools are not available in this session.", nil)
	}
	if caller.ThreadID == "" {
		return nil, errorsx.Public(CodeInvalidRequest, "Workflow tools could not tell which thread called them.", nil)
	}
	spec, ok := lookupTool(name)
	if !ok {
		return nil, invalidf("There is no tool named %q. The workflow tools are: %s.", name, joinNames(ToolNames()))
	}
	if err := caller.refusal(spec); err != nil {
		return nil, err
	}
	call := &call{app: s.app, caller: caller, now: s.now}
	switch name {
	case "workflow_list":
		return call.workflowList(ctx, args)
	case "workflow_validate":
		return call.workflowValidate(ctx, args)
	case "workflow_schema":
		return call.workflowSchema(args)
	case "workflow_new":
		return call.workflowNew(ctx, args)
	case "run_start":
		return call.runStart(ctx, args)
	case "run_status":
		return call.runStatus(ctx, args)
	case "run_watch":
		return call.runWatch(ctx, args)
	case "run_output":
		return call.runOutput(ctx, args)
	case "run_inspect":
		return call.runInspect(ctx, args)
	case "run_narrative":
		return call.runNarrative(ctx, args)
	case "run_list":
		return call.runList(ctx, args)
	case "run_control":
		return call.runControl(ctx, args)
	case "run_amend":
		return call.runAmend(ctx, args)
	case "run_guide":
		return call.runGuide(ctx, args)
	case "run_resolve":
		return call.runResolve(ctx, args)
	case "memory_add":
		return call.memoryAdd(ctx, args)
	case "memory_list":
		return call.memoryList(ctx, args)
	}
	return nil, invalidf("There is no tool named %q.", name)
}

func invalidf(format string, args ...any) error {
	return errorsx.Public(CodeInvalidRequest, fmt.Sprintf(format, args...), nil)
}

// decode parses arguments under the closed schema; a refusal names the field.
func decode(raw json.RawMessage, target any) error {
	if err := mcpargs.Decode(raw, target); err != nil {
		return errorsx.Public(CodeInvalidRequest, err.Error(), nil)
	}
	return nil
}

// failed turns an app refusal into a public error. The app's workflow methods
// answer with prose written for the caller (a run in another project, a state
// that refuses the action, a missing grant on a run it did not start); a
// public error that already carries a code passes through.
func failed(err error) error {
	if err == nil {
		return nil
	}
	if _, _, ok := errorsx.PublicDetails(err); ok {
		return err
	}
	return errorsx.Public(CodeFailed, err.Error(), err)
}

func required(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return invalidf("%s is required.", field)
	}
	return nil
}
