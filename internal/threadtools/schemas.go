package threadtools

import (
	"fmt"

	"agent-overflow/internal/provider"
)

// The tool schemas. Descriptions carry the mechanics of each parameter, so
// an agent never has to guess what a tool does or how to drive it, and
// every result carries its own recovery hint in the result itself.
//
// Both shapes are built from the same code: shape.Paired() decides whether
// the computer parameters exist at all. No tool is pairing-only, so
// pairing never adds or removes a tool; only the computer parameters
// change. A shape removes thread_spawn for a session at the sub-thread
// limit (SpawnShape.CanSpawn), and thread_watch and thread_new_assistant,
// the assistants' own tools, while assistants are turned off.

// ToolNames is the seventeen tools, in the order Tools returns them; a
// session at the sub-thread limit is listed all but thread_spawn, and one
// with assistants off all but the last two.
var ToolNames = []string{
	"thread_search",
	"thread_show",
	"thread_item",
	"thread_options",
	"thread_spawn",
	"thread_send",
	"thread_ask",
	"thread_reply",
	"thread_status",
	"thread_cancel",
	"thread_update",
	"thread_group",
	"thread_remind",
	"thread_attention",
	"thread_loop",
	"thread_watch",
	"thread_new_assistant",
}

// Tools returns the schemas for one shape. A session at the sub-thread
// limit has no thread_spawn at all.
func (s *Server) Tools(shape Shape) []map[string]any {
	tools := []map[string]any{
		searchSchema(shape),
		showSchema(shape),
		itemSchema(shape),
		optionsSchema(shape),
	}
	if shape.Spawn.CanSpawn() {
		tools = append(tools, spawnSchema(shape))
	}
	tools = append(tools,
		sendSchema(shape),
		askSchema(shape),
		replySchema(),
		statusSchema(shape),
		cancelSchema(shape),
		updateSchema(shape),
		groupSchema(shape),
		remindSchema(),
		attentionSchema(),
		loopSchema(s.wakeFloor),
	)
	if shape.AssistantsOff {
		return tools
	}
	return append(tools, watchSchema(s.wakeFloor), newAssistantSchema())
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

// computerIDProperty is the one-computer selector. Absent in the
// single-computer shape, where it could only ever name this computer.
func computerIDProperty(shape Shape, properties map[string]any, _ string) {
	if !shape.Paired() {
		return
	}
	properties["computer_id"] = map[string]any{"type": "string"}
}

func waitProperties(properties map[string]any, defaultWait, maxWait int, _ string) {
	if maxWait <= 0 || maxWait > MaxWaitSeconds {
		maxWait = MaxWaitSeconds
	}
	if defaultWait > maxWait {
		defaultWait = maxWait
	}
	properties["wait_seconds"] = map[string]any{
		"type":    "integer",
		"minimum": 0,
		"maximum": maxWait,
		"default": defaultWait,
	}
	properties["notify"] = map[string]any{
		"type":        "boolean",
		"default":     false,
		"description": "Message final output.",
	}
}

func searchSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"query": map[string]any{
			"type":        "string",
			"description": "Full-text query (AND/OR/NOT/\"phrase\"/prefix*); omit to list recent.",
		},
		"thread_id":     map[string]any{"type": "string"},
		"kind":          map[string]any{"type": "string", "enum": []string{"user", "assistant", "tool", "title"}},
		"project_id":    map[string]any{"type": "string"},
		"provider":      map[string]any{"type": "string"},
		"state":         map[string]any{"type": "string", "enum": AllStates},
		"archived":      map[string]any{"type": "boolean", "description": "Omit: both for query, active for list."},
		"spawned_by_me": map[string]any{"type": "boolean"},
		"agents":        map[string]any{"type": "boolean", "description": "Assistants only."},
		"group":         map[string]any{"type": "string"},
		"since":         map[string]any{"type": "string", "description": "RFC 3339 cutoff."},
		"limit": map[string]any{
			"type":    "integer",
			"minimum": 1,
			"maximum": MaxSearchLimit,
		},
		"cursor": map[string]any{"type": "string"},
	}
	if shape.AssistantsOff {
		delete(properties, "agents")
	}
	description := "Search messages, tool summaries and titles (with query), or list recent threads (without query). Check indexing"
	if shape.Paired() {
		properties["computers"] = map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"minItems":    1,
			"description": "[\"local\"] or computer ids; omit for all.",
		}
		description += " and errors"
	}
	description += " on empty results."
	return tool("thread_search", description+dataNotice, properties)
}

func showSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"thread_id": map[string]any{"type": "string"},
		"window": map[string]any{
			"type":    "string",
			"enum":    []string{WindowTail, WindowHead, WindowSince, WindowAround, WindowAll, WindowLastAnswer},
			"default": WindowTail,
		},
		"turns": map[string]any{
			"type":    "integer",
			"minimum": 1,
			"maximum": MaxTurns,
			"default": DefaultTailTurns,
		},
		"context": map[string]any{
			"type":        "integer",
			"minimum":     0,
			"maximum":     MaxTurns,
			"default":     1,
			"description": "Turns each side for around.",
		},
		"item_id": map[string]any{"type": "string", "description": "Anchor for since/around."},
		"since":   map[string]any{"type": "string", "description": "RFC 3339 anchor for since."},
		"include": map[string]any{
			"type":     "array",
			"items":    map[string]any{"type": "string", "enum": []string{IncludeToolCalls, IncludeThinking, IncludeToolOutputs, IncludeDiffs, IncludeSubagents, IncludeAll}},
			"minItems": 1,
		},
		"max_bytes": map[string]any{
			"type":    "integer",
			"minimum": MinShowBytes,
			"maximum": MaxShowBytes,
			"default": DefaultShowBytes,
		},
		"to_file": map[string]any{
			"type":        "boolean",
			"default":     false,
			"description": "Write full window to local file.",
		},
		"cursor": map[string]any{"type": "string"},
	}
	computerIDProperty(shape, properties, "the thread")
	description := fmt.Sprintf("Read a thread transcript (default: last %d turns; window=last_answer for final reply). Use thread_item for clipped items.", DefaultTailTurns)
	return tool("thread_show", description+dataNotice, properties, "thread_id")
}

func itemSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"thread_id": map[string]any{"type": "string"},
		"item_id":   map[string]any{"type": "string"},
		"offset":    map[string]any{"type": "integer", "description": "Byte offset (negative from end)."},
		"max_bytes": map[string]any{
			"type":    "integer",
			"minimum": 1,
			"maximum": MaxItemBytes,
			"default": DefaultItemBytes,
		},
		"lines":  map[string]any{"type": "string", "description": "1-based line range (\"a-b\", \"a-\", \"n\")."},
		"query":  map[string]any{"type": "string", "description": "Case-sensitive literal search."},
		"cursor": map[string]any{"type": "string"},
	}
	computerIDProperty(shape, properties, "the thread")
	description := "Read ranges or search inside one clipped item (pass at most one of offset, lines, query)."
	return tool("thread_item", description+dataNotice, properties, "thread_id", "item_id")
}

func optionsSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"provider":   map[string]any{"type": "string"},
		"project_id": map[string]any{"type": "string"},
		"what":       map[string]any{"type": "string", "enum": []string{OptionsModels, OptionsProjects, OptionsGroups}},
	}
	description := "List providers, models, efforts, runtime modes, profiles, projects, workspaces, groups and inherited defaults."
	if shape.Paired() {
		properties["computer_id"] = map[string]any{"type": "string"}
		description += " Includes paired computers."
	}
	return tool("thread_options", description, properties)
}

// runtimeModeEnum and runtimeModeMeanings are rendered from the canonical
// list, so a mode added to provider.AllRuntimeModes appears here without a
// second edit. A mode with no meaning below still lists, without prose.
func runtimeModeEnum() []string {
	modes := make([]string, 0, len(provider.AllRuntimeModes))
	for _, mode := range provider.AllRuntimeModes {
		modes = append(modes, string(mode))
	}
	return modes
}

var runtimeModeMeanings = map[provider.RuntimeMode]string{
	provider.RuntimeReadOnly:         "blocks writes and shell commands on Claude Code, read-only sandbox on Codex; use only for pure reading, never for caution",
	provider.RuntimeApprovalRequired: "asks user before every tool",
	provider.RuntimeAutoAcceptEdits:  "auto-accepts file edits, asks for shell commands",
	provider.RuntimeAuto:             "model reviewer approves/denies sensitive tools",
	provider.RuntimeFullAccess:       "runs everything without approvals",
}

// RuntimeModeOptions is what thread_options lists for the runtime modes.
func RuntimeModeOptions() []map[string]any {
	options := make([]map[string]any, 0, len(provider.AllRuntimeModes))
	for _, mode := range provider.AllRuntimeModes {
		options = append(options, map[string]any{"runtime_mode": string(mode), "meaning": runtimeModeMeanings[mode]})
	}
	return options
}
