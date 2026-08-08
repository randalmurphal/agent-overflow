package rollout

import (
	"encoding/json"
	"strings"

	"agent-overflow/internal/itemmeta"
)

// Rollout envelope types (RolloutItem's serde tag). The enum is open — see
// envelope — so these name what we RECOGNISE, not what can appear.
const (
	typeSessionMeta   = "session_meta"
	typeTurnContext   = "turn_context"
	typeCompacted     = "compacted"
	typeEventMsg      = "event_msg"
	typeResponseItem  = "response_item"
	typeInterAgent    = "inter_agent_communication"
	typeInterAgentMet = "inter_agent_communication_metadata"
)

// MetaImportUnavailableKey is the ProviderEvent.Meta key this package stamps
// when an item's detail existed once but cannot be recovered from the rollout.
// The import writer strips it from the stored provider meta and re-stamps it
// through itemmeta, so the event key and the persisted key are the same key by
// definition — naming itemmeta's constant is what keeps the two from drifting
// into separate spellings. itemmeta is stdlib-only, which is what lets a
// provider package reference it without acquiring a path to store.
const MetaImportUnavailableKey = itemmeta.ImportUnavailableKey

// MetaImportUnavailableExecDetail is the only reason this package produces: an
// exec / patch / MCP / web-search end record named a call outside the imported
// range, so its output and exit status are unknown.
const MetaImportUnavailableExecDetail = "exec-detail"

// Warning codes this package emits.
const (
	WarnRolloutMissing   = "codex-rollout-missing"
	WarnRolloutOutside   = "codex-rollout-outside-home"
	WarnCorruptLines     = "codex-corrupt-lines"
	WarnUnknownTypes     = "codex-unknown-types"
	WarnUnmatchedEnd     = "codex-unmatched-tool-end"
	WarnUnresolvedTool   = "codex-unresolved-tool-call"
	WarnMissingSessionID = "codex-session-meta-missing"
)

// contentText flattens the several shapes Codex uses for message and tool
// content into readable text.
//
// The shapes, all observed in real rollouts: a bare JSON string; an array of
// content blocks `{"type":"input_text"|"output_text"|"text","text":"..."}`;
// and blocks with no text at all (`{"type":"encrypted_content",...}`), which
// contribute nothing. present=false means the field was absent or carried no
// readable text — distinct from an authoritative empty string.
func contentText(raw json.RawMessage) (text string, present bool) {
	if len(raw) == 0 {
		return "", false
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return asString, true
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	var b strings.Builder
	for _, block := range blocks {
		part, ok := rawString(block, "text")
		if !ok || part == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(part)
	}
	if b.Len() == 0 {
		return "", false
	}
	return b.String(), true
}

// rawString reads a string field out of an already-decoded object.
func rawString(obj map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := obj[key]
	if !ok || len(raw) == 0 {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// sessionMetaPayload is the `session_meta` wire shape.
//
// `id` is this file's own thread id and the ONLY field that identifies which
// meta line belongs to this file: a forked rollout embeds the SOURCE session's
// meta as a second line, with its own id, cwd and git block. `session_id` is a
// backward-compatibility alias Codex fills from `id` when older readers need
// it, and on a forked file it can name the PARENT — so it must never be used
// to decide which line to accept.
type sessionMetaPayload struct {
	ID             string          `json:"id"`
	SessionID      string          `json:"session_id"`
	ForkedFromID   string          `json:"forked_from_id"`
	ParentThreadID string          `json:"parent_thread_id"`
	Cwd            string          `json:"cwd"`
	Originator     string          `json:"originator"`
	CLIVersion     string          `json:"cli_version"`
	ThreadSource   string          `json:"thread_source"`
	ModelProvider  string          `json:"model_provider"`
	Timestamp      string          `json:"timestamp"`
	Source         json.RawMessage `json:"source"`
	Git            *struct {
		Branch        string `json:"branch"`
		CommitHash    string `json:"commit_hash"`
		RepositoryURL string `json:"repository_url"`
	} `json:"git"`
}

// turnContextPayload is the per-turn config snapshot Codex persists once per
// real user turn. Its `turn_id` matches the `task_started` that follows it.
type turnContextPayload struct {
	TurnID string `json:"turn_id"`
	Cwd    string `json:"cwd"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// compactedPayload is the durable compaction record. `replacement_history` is
// the rewritten model context and is deliberately NOT imported: it is a
// re-statement of history AO already has rows for, and writing it would
// duplicate the whole pre-compaction transcript.
type compactedPayload struct {
	Message  string `json:"message"`
	WindowID string `json:"window_id"`
}

type taskStartedPayload struct {
	TurnID              string `json:"turn_id"`
	StartedAt           int64  `json:"started_at"`
	ModelContextWindow  int    `json:"model_context_window"`
	CollaborationModeKd string `json:"collaboration_mode_kind"`
}

type taskCompletePayload struct {
	TurnID           string `json:"turn_id"`
	LastAgentMessage string `json:"last_agent_message"`
	StartedAt        int64  `json:"started_at"`
	CompletedAt      int64  `json:"completed_at"`
	DurationMS       int64  `json:"duration_ms"`
}

type turnAbortedPayload struct {
	TurnID      string `json:"turn_id"`
	Reason      string `json:"reason"`
	CompletedAt int64  `json:"completed_at"`
}

type userMessagePayload struct {
	Message string `json:"message"`
}

type agentMessagePayload struct {
	Message string `json:"message"`
	Phase   string `json:"phase"`
}

type agentReasoningPayload struct {
	Text string `json:"text"`
}

// tokenUsageWire is one Codex usage snapshot. `input_tokens` INCLUDES
// `cached_input_tokens`; the normalized split happens in turns.go.
type tokenUsageWire struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	CacheWriteInputTokens int `json:"cache_write_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	TotalTokens           int `json:"total_tokens"`
}

type tokenCountPayload struct {
	Info *struct {
		TotalTokenUsage    *tokenUsageWire `json:"total_token_usage"`
		LastTokenUsage     *tokenUsageWire `json:"last_token_usage"`
		ModelContextWindow int             `json:"model_context_window"`
	} `json:"info"`
}

// responseCallPayload covers the four call-shaped response items:
// function_call, custom_tool_call, tool_search_call and web_search_call.
// Only one of Arguments / Input is populated per shape.
type responseCallPayload struct {
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Status    string          `json:"status"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
	Passthru  *struct {
		TurnID string `json:"turn_id"`
	} `json:"internal_chat_message_metadata_passthrough"`
}

// responseOutputPayload covers function_call_output, custom_tool_call_output
// and tool_search_output.
type responseOutputPayload struct {
	ID      string          `json:"id"`
	CallID  string          `json:"call_id"`
	Status  string          `json:"status"`
	Output  json.RawMessage `json:"output"`
	Tools   json.RawMessage `json:"tools"`
	Success *bool           `json:"success"`
}

// reasoningPayload is a model reasoning item. `summary` (and, for
// open-weight models, `content`) hold readable text; `encrypted_content` is
// opaque. An item with only encrypted content is skipped entirely — see
// convertReasoning.
type reasoningPayload struct {
	ID               string            `json:"id"`
	Summary          []json.RawMessage `json:"summary"`
	Content          []json.RawMessage `json:"content"`
	EncryptedContent string            `json:"encrypted_content"`
}

// messagePayload is a response_item/message: the model-context mirror of the
// conversation, including developer/system injections a user never typed.
type messagePayload struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// interAgentPayload is a collab message between agent threads. In 0.146 files
// the same record also arrives as response_item/agent_message.
type interAgentPayload struct {
	Author      string          `json:"author"`
	Recipient   string          `json:"recipient"`
	Content     json.RawMessage `json:"content"`
	TriggerTurn *bool           `json:"trigger_turn"`
}

type patchApplyEndPayload struct {
	CallID  string                     `json:"call_id"`
	TurnID  string                     `json:"turn_id"`
	Stdout  string                     `json:"stdout"`
	Stderr  string                     `json:"stderr"`
	Success *bool                      `json:"success"`
	Changes map[string]json.RawMessage `json:"changes"`
}

// execCommandEndPayload is only present in rollouts written before Codex
// stopped persisting the event (codex-rs/rollout/src/policy.rs no longer
// lists ExecCommandEnd). It carries call_id, so it is matched exactly like
// every other end event — there is no command-string guessing anywhere here.
type execCommandEndPayload struct {
	CallID           string   `json:"call_id"`
	TurnID           string   `json:"turn_id"`
	Command          []string `json:"command"`
	Cwd              string   `json:"cwd"`
	ExitCode         *int     `json:"exit_code"`
	Stdout           string   `json:"stdout"`
	Stderr           string   `json:"stderr"`
	AggregatedOutput string   `json:"aggregated_output"`
	Source           string   `json:"source"`
}

type mcpToolCallEndPayload struct {
	CallID     string `json:"call_id"`
	Invocation *struct {
		Server    string          `json:"server"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"invocation"`
	Result map[string]json.RawMessage `json:"result"`
}

type webSearchEndPayload struct {
	CallID string `json:"call_id"`
	Query  string `json:"query"`
}

// subAgentActivityPayload is the MultiAgentV2 child lifecycle record.
// `event_id` is the call_id of the spawning tool call — the wire's own
// linkage between a child's activity and the parent row it belongs under.
type subAgentActivityPayload struct {
	EventID       string `json:"event_id"`
	OccurredAtMS  int64  `json:"occurred_at_ms"`
	AgentThreadID string `json:"agent_thread_id"`
	AgentPath     string `json:"agent_path"`
	Kind          string `json:"kind"`
}

type threadGoalPayload struct {
	Goal *struct {
		Objective string `json:"objective"`
		Status    string `json:"status"`
	} `json:"goal"`
}

type reviewModePayload struct {
	TurnID          string `json:"turn_id"`
	UserFacingHint  string `json:"user_facing_hint"`
	ReviewOutputRaw json.RawMessage
}

type itemCompletedPayload struct {
	ThreadID string          `json:"thread_id"`
	TurnID   string          `json:"turn_id"`
	Item     json.RawMessage `json:"item"`
}

type turnItemHeader struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Text string `json:"text"`
}

// collabEndPayload covers the four MultiAgentV1 collab end events
// (`collab_agent_spawn_end`, `collab_agent_interaction_end`,
// `collab_waiting_end`, `collab_close_end`). All four carry the `call_id` of
// the `spawn_agent` / `send_message` / `wait` / `close_agent` function call
// they finish, so they route through the same call_id matching every other
// end event uses. Only older rollouts carry them — current Codex no longer
// persists them — which is exactly why the parser must keep reading them.
type collabEndPayload struct {
	CallID                string          `json:"call_id"`
	NewThreadID           string          `json:"new_thread_id"`
	NewAgentNickname      string          `json:"new_agent_nickname"`
	NewAgentRole          string          `json:"new_agent_role"`
	ReceiverThreadID      string          `json:"receiver_thread_id"`
	ReceiverAgentNickname string          `json:"receiver_agent_nickname"`
	ReceiverAgentRole     string          `json:"receiver_agent_role"`
	Prompt                string          `json:"prompt"`
	Model                 string          `json:"model"`
	ReasoningEffort       string          `json:"reasoning_effort"`
	Status                json.RawMessage `json:"status"`
	AgentStatuses         []struct {
		ThreadID      string          `json:"thread_id"`
		AgentNickname string          `json:"agent_nickname"`
		AgentRole     string          `json:"agent_role"`
		Status        json.RawMessage `json:"status"`
	} `json:"agent_statuses"`
}

// errorPayload is Codex's user-facing error record.
type errorPayload struct {
	Message string `json:"message"`
	Info    string `json:"codex_error_info"`
}
