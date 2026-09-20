package threadtools

import (
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
)

// The tool schemas. Descriptions carry the mechanics of each parameter, so
// an agent never has to guess what a tool does or how to drive it, and
// every result carries its own recovery hint in the result itself.
//
// Both shapes are built from the same code: shape.Paired() decides whether
// the computer parameters exist at all. No tool is pairing-only, so no
// tool ever appears or disappears; only the computer parameters do.

// ToolNames is the thirteen tools, in the order Tools returns them.
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
}

// Tools returns the schemas for one shape.
func (s *Server) Tools(shape Shape) []map[string]any {
	return []map[string]any{
		searchSchema(shape),
		showSchema(shape),
		itemSchema(shape),
		optionsSchema(shape),
		spawnSchema(shape),
		sendSchema(shape),
		askSchema(shape),
		replySchema(),
		statusSchema(),
		cancelSchema(shape),
		updateSchema(shape),
		groupSchema(shape),
		remindSchema(),
	}
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
func computerIDProperty(shape Shape, properties map[string]any, what string) {
	if !shape.Paired() {
		return
	}
	properties["computer_id"] = map[string]any{
		"type":        "string",
		"description": "Computer id from thread_options or from a result row, when you already know which computer holds " + what + ". Omit to resolve the id across this computer and every paired one; naming it skips that fan-out and turns an unreachable computer into an immediate error instead of an incomplete answer.",
	}
}

func waitProperties(properties map[string]any, defaultWait int, what string) {
	properties["wait_seconds"] = map[string]any{
		"type":        "integer",
		"minimum":     0,
		"maximum":     MaxWaitSeconds,
		"default":     defaultWait,
		"description": fmt.Sprintf("How long THIS CALL waits for %s, not how long the work may run. Zero returns as soon as the request is accepted. A wait that ends before the answer returns outcome backgrounded (still working) or blocked (the thread is waiting on the user for an approval or a question), arms notify, and the answer then arrives in this thread as a message at your next turn boundary. Maximum %d.", what, MaxWaitSeconds),
	}
	properties["notify"] = map[string]any{
		"type":        "boolean",
		"default":     false,
		"description": "Deliver the answer as a message in this thread if this call did not carry it. Any positive wait that ends unsettled turns this on by itself; set it with wait_seconds 0 for fire-and-forget work whose answer you still want.",
	}
}

func searchSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"query": map[string]any{
			"type":        "string",
			"description": "FTS5 match syntax over settled user text, assistant text, tool call summaries and thread titles: words are ANDed, \"quoted phrases\" match exactly, OR works, and a trailing * is a prefix match. Tool outputs, diffs and thinking are NOT indexed; find their thread here and read them with thread_show include or thread_item. Omit to list recent threads by last activity instead of searching.",
		},
		"thread_id": map[string]any{
			"type":        "string",
			"description": "Search inside one thread only. Full id, or an unambiguous prefix of at least 6 characters.",
		},
		"kind": map[string]any{
			"type":        "string",
			"enum":        []string{"user", "assistant", "tool", "title"},
			"description": "Restrict a query to one row kind: user messages, assistant messages, tool call summaries, or thread titles.",
		},
		"project_id": map[string]any{"type": "string", "description": "Only threads of this project. Ids come from thread_options or from a result row."},
		"provider":   map[string]any{"type": "string", "description": "Only threads running this provider, for example claude or codex."},
		"state": map[string]any{
			"type":        "string",
			"enum":        AllStates,
			"description": "Only threads in this state right now. running is a turn in flight; pending-approval and awaiting-input mean the thread is blocked on the user; plan-ready, error, interrupted and setup-failed are states the sidebar shows after a turn ended.",
		},
		"archived":      map[string]any{"type": "boolean", "description": "true lists only archived threads, false only unarchived ones. Omitted, a query searches both and a listing shows unarchived threads only, as the sidebar does."},
		"spawned_by_me": map[string]any{"type": "boolean", "description": "Only threads this thread spawned, forks included. A thread it merely sent to or asked is not listed; thread_status lists those requests."},
		"since":         map[string]any{"type": "string", "description": "RFC 3339 timestamp with an offset. Only threads active since then."},
		"limit": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     MaxSearchLimit,
			"description": fmt.Sprintf("Rows per computer. Default %d with a query, %d without.", DefaultSearchLimit, DefaultListLimit),
		},
		"cursor": map[string]any{"type": "string", "description": "Opaque cursor from a previous thread_search result, passed back unchanged to continue past limit. Omit to start over."},
	}
	description := "Find threads, or list them. With query it is a full-text search over settled message text, tool call summaries and titles; without one it lists recent threads by last activity. Either way each row carries the thread id, title, project, provider, model, current state, last activity, branch, group, pin tier, archived and unread flags, and with a query the matched item id and a snippet. Open a hit with thread_show around its item id. Scratch threads are never listed; workflow threads are. Check indexing before treating an empty result as conclusive: a computer still building its index covers only what it has indexed so far."
	if shape.Paired() {
		properties["computers"] = map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"minItems":    1,
			"description": "Computer ids to search. Omit for this computer plus every paired one. [\"local\"] means only this computer. Ids come from thread_options.",
		}
		description += " Rows are grouped per computer in that computer's own ranking, because relevance is not comparable across separate indexes, and limit applies per computer. A computer that is offline or too old contributes an errors row naming it and the reason; the other computers' rows still return, so check errors as well."
	}
	return tool("thread_search", description+dataNotice, properties)
}

func showSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"thread_id": map[string]any{
			"type":        "string",
			"description": "The thread to read. Full id, or an unambiguous prefix of at least 6 characters. Take it from a thread_search row; never guess one.",
		},
		"window": map[string]any{
			"type":        "string",
			"enum":        []string{WindowTail, WindowHead, WindowSince, WindowAround, WindowAll},
			"default":     WindowTail,
			"description": "Which part of the thread to read. tail is the last turns (the default) and head the first, both sized by turns. since reads everything after item_id, or after the since timestamp. around reads the turns surrounding item_id, sized by context, which is how a thread_search hit is opened. all starts at the beginning and pages to the end.",
		},
		"turns": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     MaxTurns,
			"default":     DefaultTailTurns,
			"description": "How many turns a tail or head window covers.",
		},
		"context": map[string]any{
			"type":        "integer",
			"minimum":     0,
			"maximum":     MaxTurns,
			"default":     1,
			"description": "How many turns on each side of item_id an around window covers.",
		},
		"item_id": map[string]any{
			"type":        "string",
			"description": "Anchor item for the since and around windows. Item ids are unique only inside their thread, so this is read together with thread_id. A thread_search hit carries both.",
		},
		"since": map[string]any{
			"type":        "string",
			"description": "RFC 3339 timestamp with an offset, an alternative anchor for the since window when you have no item id.",
		},
		"include": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string", "enum": []string{IncludeThinking, IncludeToolOutputs, IncludeDiffs, IncludeSubagents, IncludeAll}},
			"minItems":    1,
			"description": "Extra content to render beside what people said. Default is user and assistant text only, with each tool call collapsed to one line stating its item id and the size it holds. thinking adds the assistant's reasoning, tool_outputs the outputs, diffs the diffs, subagents the subagent runs, and all everything stored for the thread.",
		},
		"max_bytes": map[string]any{
			"type":        "integer",
			"minimum":     MinShowBytes,
			"maximum":     MaxShowBytes,
			"default":     DefaultShowBytes,
			"description": fmt.Sprintf("Inline transcript budget for this call. Default %d. A result that stops short is never a dead end: it returns done: false and an opaque cursor that remembers the window and where it stopped. Raise it only as far as the provider will still carry one tool result; past that use to_file.", DefaultShowBytes),
		},
		"to_file": map[string]any{
			"type":        "boolean",
			"default":     false,
			"description": "Render the whole requested window to a file instead of inline, ignoring max_bytes, and return its path, size and digest for your own read and search tools. Included items are written whole, never clipped. This is how you read a whole large thread. A thread on another computer renders there and the file is copied across.",
		},
		"cursor": map[string]any{
			"type":        "string",
			"description": "Opaque cursor from a previous thread_show result, passed back unchanged to continue the same window from where it stopped. A head or around window keeps its bounds; all walks to the end. The page is a snapshot of the items that existed when the cursor was minted, so a thread that keeps streaming never shifts a page under you. When cursor is set, window and its sizing parameters are already fixed and must be omitted.",
		},
	}
	computerIDProperty(shape, properties, "the thread")
	description := "Read one thread's transcript. Output is plain text, turn-delimited, each row prefixed with its role and item id. The default window is the last " + fmt.Sprintf("%d", DefaultTailTurns) + " turns of what people said. A single item too large for the transcript is shown clipped with its size and its item id: read the rest with thread_item. Any thread by id, hidden workflow threads included."
	return tool("thread_show", description+dataNotice, properties, "thread_id")
}

func itemSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"thread_id": map[string]any{"type": "string", "description": "The thread the item belongs to. Item ids are unique only inside a thread, so this is required beside item_id."},
		"item_id":   map[string]any{"type": "string", "description": "The item to read, from a thread_show row or a thread_search hit."},
		"offset": map[string]any{
			"type":        "integer",
			"description": fmt.Sprintf("Absolute byte offset to read from, with max_bytes. A negative offset reads from the end (-%d reads the last %d bytes) and clamps to the start when the item is shorter. A range that would split a UTF-8 character is widened to whole characters. An offset at or past the end is an empty read reporting the size, not an error.", DefaultItemBytes, DefaultItemBytes),
		},
		"max_bytes": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     MaxItemBytes,
			"default":     DefaultItemBytes,
			"description": fmt.Sprintf("Bytes to return for an offset read. Default %d.", DefaultItemBytes),
		},
		"lines": map[string]any{
			"type":        "string",
			"description": "A 1-based inclusive line range instead of a byte range, written \"first-last\" (for example \"120-180\"), \"first-\" to read to the end, or \"120\" for one line.",
		},
		"query": map[string]any{
			"type":        "string",
			"description": fmt.Sprintf("Literal, case-sensitive text to find inside the item. Returns up to %d matches with each match's byte offset and a little context, and a cursor for the rest. Find the part you want this way, then read exactly that range with offset.", MaxItemMatches),
		},
		"cursor": map[string]any{"type": "string", "description": "Opaque cursor from a previous thread_item result, passed back unchanged to continue the same query past its matches."},
	}
	computerIDProperty(shape, properties, "the thread")
	description := "Read inside one item once you know which one: the whole payload of a tool output, a diff, a subagent run or a long message, in ranges. At most one selector per call: offset with max_bytes, or lines, or query; none reads from the start, max_bytes at a time. This is what makes a multi-megabyte tool call inspectable without ever pulling it whole; the item renders on the computer that holds it and only the requested bytes cross."
	return tool("thread_item", description+dataNotice, properties, "thread_id", "item_id")
}

func optionsSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"provider":   map[string]any{"type": "string", "description": "Only this provider's models, to narrow a large answer."},
		"project_id": map[string]any{"type": "string", "description": "Only this project's workspaces and thread groups."},
	}
	description := "What a spawn can choose from, rendered from the catalogs this app keeps: the providers, each provider's models with their reasoning efforts and context windows, the runtime modes with a one-line meaning each, and the projects with their workspaces and thread groups. Your own computer comes first and states your current provider, model, effort, mode and runtime mode as the defaults a spawn inherits. Call it when you want something other than your own setup, or to look up a group or project id."
	if shape.Paired() {
		properties["computer_id"] = map[string]any{"type": "string", "description": "Only this computer's row. Omit for this computer and every paired one."}
		description += " Each paired computer also reports whether it is reachable right now, its operating system, and its projects' workspace paths as that computer sees them, which are not paths on this computer."
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
	provider.RuntimeReadOnly:         "is only for work that needs nothing but reading, and blocks much more than writes: Claude Code refuses every shell command, so no git, build, test or search command runs, and Codex runs commands in a read-only sandbox. It never asks a person, so unattended reading keeps moving. Never pick it for caution: a thread that turns out to need a command or a write fails at it",
	provider.RuntimeApprovalRequired: "asks the user before every tool use",
	provider.RuntimeAutoAcceptEdits:  "applies file edits in the workspace without asking but still asks for shell commands",
	provider.RuntimeAuto:             "lets a model-based reviewer approve or deny each sensitive tool use instead of the user",
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

func runtimeModeSentence() string {
	parts := make([]string, 0, len(provider.AllRuntimeModes))
	for _, mode := range provider.AllRuntimeModes {
		if meaning := runtimeModeMeanings[mode]; meaning != "" {
			parts = append(parts, string(mode)+" "+meaning)
		}
	}
	return strings.Join(parts, "; ")
}
