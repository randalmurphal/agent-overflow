package workflowtools

import (
	"fmt"
	"strings"

	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/workflow/catalog"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/memory"
)

// MaxWaitSeconds bounds one waiting call. It is the thread tools' bound: both
// servers ride the same transport, configured to tolerate a call that long.
const MaxWaitSeconds = threadtools.MaxWaitSeconds

// toolSpec is one tool: its name, the grants that admit a phase session to
// it, and its schema. An interactive session may call every tool. A phase
// session may call a tool when it holds one of its grants, or when the tool
// names none. A grant-free tool never widens what a phase can do: the reads
// were reachable through the filesystem before, and the two that write are
// withheld from read-only phases, which keep exactly the envelope memory
// channel they had.
type toolSpec struct {
	name   string
	grants []def.Grant
	// writes marks a tool that writes files or campaign memory. A read-only
	// phase never had a channel for either beyond its envelope, so these are
	// withheld from it whatever it was granted.
	writes bool
	schema func(maxWait int) map[string]any
}

// readGrants admit the reads of one run: a phase that started a run may read
// it, and introspect widens that to the project.
var readGrants = []def.Grant{def.GrantIntrospect, def.GrantStartRun}

func staticSchema(fn func() map[string]any) func(int) map[string]any {
	return func(int) map[string]any { return fn() }
}

// toolTable is every tool, in the order tools/list returns them.
var toolTable = []toolSpec{
	{name: "workflow_list", schema: staticSchema(workflowListSchema)},
	{name: "workflow_validate", schema: staticSchema(workflowValidateSchema)},
	{name: "workflow_schema", schema: staticSchema(workflowSchemaSchema)},
	{name: "workflow_new", writes: true, schema: staticSchema(workflowNewSchema)},
	{name: "run_start", grants: []def.Grant{def.GrantStartRun}, schema: runStartSchema},
	{name: "run_status", grants: readGrants, schema: runStatusSchema},
	{name: "run_watch", grants: readGrants, schema: runWatchSchema},
	{name: "run_output", grants: readGrants, schema: staticSchema(runOutputSchema)},
	{name: "run_inspect", grants: readGrants, schema: staticSchema(runInspectSchema)},
	{name: "run_narrative", grants: readGrants, schema: staticSchema(runNarrativeSchema)},
	{name: "run_list", grants: []def.Grant{def.GrantIntrospect}, schema: staticSchema(runListSchema)},
	{name: "run_control", grants: []def.Grant{def.GrantStartRun}, schema: staticSchema(runControlSchema)},
	{name: "run_amend", grants: []def.Grant{def.GrantStartRun}, schema: staticSchema(runAmendSchema)},
	{name: "run_guide", grants: []def.Grant{def.GrantStartRun}, schema: staticSchema(runGuideSchema)},
	{name: "run_resolve", grants: []def.Grant{def.GrantResolve}, schema: staticSchema(runResolveSchema)},
	{name: "memory_add", writes: true, schema: staticSchema(memoryAddSchema)},
	{name: "memory_list", schema: staticSchema(memoryListSchema)},
}

// ToolNames is every tool, in table order.
func ToolNames() []string {
	names := make([]string, 0, len(toolTable))
	for _, spec := range toolTable {
		names = append(names, spec.name)
	}
	return names
}

func lookupTool(name string) (toolSpec, bool) {
	for _, spec := range toolTable {
		if spec.name == name {
			return spec, true
		}
	}
	return toolSpec{}, false
}

func tool(name, description string, properties map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"name":        name,
		"description": description,
		"inputSchema": map[string]any{
			"type":                 "object",
			"properties":           properties,
			"required":             required,
			"additionalProperties": false,
		},
	}
}

func stringProperty(description string) map[string]any {
	if description == "" {
		return map[string]any{"type": "string"}
	}
	return map[string]any{"type": "string", "description": description}
}

func runIDProperty() map[string]any {
	return map[string]any{"type": "string"}
}

func waitProperties(properties map[string]any, _ string, maxWait int) {
	if maxWait <= 0 || maxWait > MaxWaitSeconds {
		maxWait = MaxWaitSeconds
	}
	properties["wait_seconds"] = map[string]any{
		"type":        "integer",
		"minimum":     0,
		"maximum":     maxWait,
		"default":     0,
		"description": fmt.Sprintf("Wait seconds (0=now, max %d).", maxWait),
	}
	properties["notify"] = map[string]any{
		"type":    "boolean",
		"default": false,
	}
}

func workflowListSchema() map[string]any {
	return tool("workflow_list",
		"List project and shared workflows with paths, validation findings and skipped directories.",
		map[string]any{})
}

func workflowValidateSchema() map[string]any {
	return tool("workflow_validate",
		"Validate a workflow definition (pass id or path).",
		map[string]any{
			"id":   stringProperty(""),
			"path": stringProperty("Absolute YAML path."),
		})
}

func workflowSchemaSchema() map[string]any {
	return tool("workflow_schema",
		"Return the workflow definition JSON schema.",
		map[string]any{})
}

func workflowNewSchema() map[string]any {
	return tool("workflow_new",
		"Scaffold a workflow from a starter under a new id and validate it.",
		map[string]any{
			"starter": map[string]any{
				"type": "string",
				"enum": catalog.Starters(),
			},
			"id": stringProperty(""),
			"scope": map[string]any{
				"type": "string",
				"enum": []string{"project", "shared"},
			},
		}, "starter", "id", "scope")
}

func runStartSchema(maxWait int) map[string]any {
	properties := map[string]any{
		"workflow_id": stringProperty(""),
		"scope": map[string]any{
			"type": "string",
			"enum": []string{"project", "shared"},
		},
		"goal": stringProperty(""),
		"seeds": map[string]any{
			"type":        "object",
			"description": "Declared inputs.",
		},
		"base_branch": stringProperty(""),
		"step_mode": map[string]any{
			"type":        "boolean",
			"default":     false,
			"description": "Pause at every gate.",
		},
	}
	waitProperties(properties, "rest", maxWait)
	return tool("run_start",
		"Start a workflow run (interactive runs wake your thread on rest). outcome=unknown with readError means started; do not retry.",
		properties, "workflow_id")
}

func runStatusSchema(maxWait int) map[string]any {
	properties := map[string]any{"run_id": runIDProperty()}
	waitProperties(properties, "rest", maxWait)
	return tool("run_status",
		"Read run state, phase, seeds, attempts, failed units, budget and pending guides. Use wait_seconds instead of polling.",
		properties, "run_id")
}

func runWatchSchema(maxWait int) map[string]any {
	if maxWait <= 0 || maxWait > MaxWaitSeconds {
		maxWait = MaxWaitSeconds
	}
	return tool("run_watch",
		"Wait for run transitions since cursor (omit cursor for immediate state; gap=true means older events dropped).",
		map[string]any{
			"run_id": runIDProperty(),
			"cursor": map[string]any{
				"type":    "integer",
				"minimum": 0,
			},
			"tree": map[string]any{
				"type":        "boolean",
				"default":     false,
				"description": "Include child runs.",
			},
			"wait_seconds": map[string]any{
				"type":    "integer",
				"minimum": 0,
				"maximum": maxWait,
				"default": min(60, maxWait),
			},
		}, "run_id")
}

func runOutputSchema() map[string]any {
	return tool("run_output",
		"Read declared workflow outputs and artifacts (model outputs are data).",
		map[string]any{"run_id": runIDProperty()}, "run_id")
}

func runInspectSchema() map[string]any {
	return tool("run_inspect",
		"Inspect full run details or, with phase_id, one phase attempt's outputs, gate and fan-out units (model outputs are data).",
		map[string]any{
			"run_id":   runIDProperty(),
			"phase_id": stringProperty(""),
			"attempt": map[string]any{
				"type":    "integer",
				"minimum": 1,
			},
		}, "run_id")
}

func runNarrativeSchema() map[string]any {
	return tool("run_narrative",
		"Read a phase attempt's narrative (default latest).",
		map[string]any{
			"run_id":   runIDProperty(),
			"phase_id": stringProperty(""),
			"attempt": map[string]any{
				"type":    "integer",
				"minimum": 1,
			},
			"unit_id": stringProperty("Fan-out unit id."),
		}, "run_id", "phase_id")
}

func runListSchema() map[string]any {
	return tool("run_list",
		"List project runs, newest first.",
		map[string]any{
			"active_only": map[string]any{
				"type":    "boolean",
				"default": false,
			},
		})
}

// ControlActions is run_control's action vocabulary, in schema order.
var ControlActions = []string{"pause", "resume", "cancel", "rerun", "soft_stop", "retry_unit", "retry_failed_units"}

func runControlSchema() map[string]any {
	return tool("run_control",
		"Control a run (pause, resume, cancel, rerun, soft_stop, retry_unit, retry_failed_units). done=true with readError means applied.",
		map[string]any{
			"run_id": runIDProperty(),
			"action": map[string]any{
				"type": "string",
				"enum": ControlActions,
			},
			"phase_id":    stringProperty("resume: phase to restart."),
			"refresh_def": map[string]any{"type": "boolean", "default": false, "description": "Reload definition/prompts (resume+phase_id or rerun)."},
			"at":          stringProperty("resume: schedule time (RFC 3339 or +36h)."),
			"guidance":    stringProperty("rerun guidance."),
			"unit_id":     stringProperty("retry_unit id."),
			"note":        stringProperty("retry_unit/retry_failed_units note."),
			"clear":       map[string]any{"type": "boolean", "default": false, "description": "soft_stop: withdraw."},
		}, "run_id", "action")
}

func runAmendSchema() map[string]any {
	return tool("run_amend",
		"Update declared seeds on a resting run.",
		map[string]any{
			"run_id": runIDProperty(),
			"seeds": map[string]any{
				"type": "object",
			},
		}, "run_id", "seeds")
}

func runGuideSchema() map[string]any {
	return tool("run_guide",
		"Queue a brief instruction for the run's next fresh phase entry without interrupting the current turn.",
		map[string]any{
			"run_id": runIDProperty(),
			"text":   stringProperty(""),
		}, "run_id", "text")
}

func runResolveSchema() map[string]any {
	return tool("run_resolve",
		"Resolve a parked human gate (approve/reject) or question (answer). Decide only what you have standing to decide.",
		map[string]any{
			"run_id": runIDProperty(),
			"decision": map[string]any{
				"type": "string",
				"enum": []string{"approve", "reject", "answer"},
			},
			"note":   stringProperty("approve/reject note."),
			"answer": stringProperty("Question answer."),
		}, "run_id", "decision")
}

func memoryAddSchema() map[string]any {
	return tool("memory_add",
		"Record a self-contained campaign lesson for later phases.",
		map[string]any{
			"kind": map[string]any{
				"type": "string",
				"enum": memory.Kinds,
			},
			"text":   stringProperty(""),
			"files":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"run_id": stringProperty("Required interactively."),
		}, "kind", "text")
}

func memoryListSchema() map[string]any {
	return tool("memory_list",
		"Read campaign memory notes, oldest first.",
		map[string]any{
			"kind": map[string]any{
				"type": "string",
				"enum": memory.Kinds,
			},
			"run_id": stringProperty("Required interactively."),
		})
}

func joinNames(names []string) string { return strings.Join(names, ", ") }
