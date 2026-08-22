package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ApprovalRequest is sent when a provider needs user permission.
type ApprovalRequest struct {
	RequestID string `json:"requestId"`
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId,omitempty"`
	ToolUseID string `json:"toolUseId,omitempty"`
	// ParentToolUseID is the launch tool_use of the SUBAGENT that is asking,
	// or empty when the main agent asks. It is what nests the approval's
	// timeline row under the agent's card and lights the card's "needs
	// approval" pill; the prompt itself is still presented by the thread's
	// normal approval UI. Claude carries the asking agent's `agent_id` on
	// `can_use_tool` (== its task id), which the parser resolves through its
	// task_id ↔ tool_use_id map; triage falls back to the requested tool's
	// own persisted row scope when the parser could not resolve it.
	ParentToolUseID string          `json:"parentToolUseId,omitempty"`
	ToolName        string          `json:"toolName"`
	Description     string          `json:"description"`
	Input           json.RawMessage `json:"input"`
	Title           string          `json:"title"`
	// Structured approval fields.
	Kind        string             `json:"kind,omitempty"`        // "command"|"file-read"|"file-change"|"permission"|"mcp-elicitation"
	Permissions *PermissionProfile `json:"permissions,omitempty"` // populated for permission kind
	// Elicitation is populated for the mcp-elicitation kind. Carries the high-
	// level mode discriminator and the shape the frontend needs to render the
	// dialog. The schema for form mode is passed through as raw JSON — the
	// frontend owns its interpretation so this package doesn't have to mirror
	// the full Codex elicitation schema taxonomy in Go types.
	Elicitation *ElicitationRequest `json:"elicitation,omitempty"`
	// PermissionSuggestions carries the Claude SDK's optional `permission_suggestions`
	// array from the CanUseTool control_request. The payload is a JSON array of
	// PermissionUpdate objects; the shape is provider-specific so it flows
	// through the pipeline as opaque JSON for the frontend to interpret.
	PermissionSuggestions json.RawMessage `json:"permissionSuggestions,omitempty"`
}

// ApprovalEvent is the frontend-facing channel payload for approval overlay
// changes. The request ID remains the provider-native identifier because the
// response binding routes back through it unchanged.
type ApprovalEvent struct {
	Action    string           `json:"action"` // "request" | "resolve" | "fail"
	ThreadID  string           `json:"threadId,omitempty"`
	Request   *ApprovalRequest `json:"request,omitempty"`
	RequestID string           `json:"requestId,omitempty"`
	Decision  string           `json:"decision,omitempty"` // approved|declined|amended|lost|failed
	Detail    string           `json:"detail,omitempty"`
	// RequestedAt is the wire-event timestamp (millis since epoch) for
	// action="request"; lets the frontend bump cached thread activity
	// using the same clock the backend wrote to threads.updated_at via
	// MarkThreadActivity, instead of drifting on Date.now().
	RequestedAt int64 `json:"requestedAt,omitempty"`
}

// UserInputRequest is sent when a provider needs structured user input.
// It is deliberately separate from ApprovalRequest: user-input prompts are
// answer collection, not permission grants, and the frontend renders them
// through a different composer flow.
type UserInputRequest struct {
	RequestID string              `json:"requestId"`
	ThreadID  string              `json:"threadId"`
	TurnID    string              `json:"turnId,omitempty"`
	ToolUseID string              `json:"toolUseId,omitempty"`
	ToolName  string              `json:"toolName"`
	Title     string              `json:"title"`
	Questions []UserInputQuestion `json:"questions"`
}

// UserInputEvent is the frontend-facing channel payload for structured
// user-input prompt changes.
type UserInputEvent struct {
	Action    string            `json:"action"` // "request" | "resolve" | "fail"
	ThreadID  string            `json:"threadId,omitempty"`
	Request   *UserInputRequest `json:"request,omitempty"`
	RequestID string            `json:"requestId,omitempty"`
	Decision  string            `json:"decision,omitempty"` // answered|declined|lost|failed
	Detail    string            `json:"detail,omitempty"`
	// RequestedAt mirrors ApprovalEvent.RequestedAt — wire-event
	// timestamp for action="request" so the frontend's cached activity
	// matches the persisted threads.updated_at written by triage.
	RequestedAt int64 `json:"requestedAt,omitempty"`
}

// PendingInteractiveRequests is the app-runtime snapshot of still-open
// prompts for one thread. The provider process remains the authority: this is
// only for hydrating UI panes that missed the original live event.
type PendingInteractiveRequests struct {
	Approvals  []ApprovalRequest  `json:"approvals"`
	UserInputs []UserInputRequest `json:"userInputs"`
}

// ElicitationRequest is the frontend-facing shape for an MCP elicitation
// request, extracted from the raw provider payload. Only one of
// (RequestedSchema) or (URL + ElicitationID) is populated depending on Mode.
// Wire contract lives in the upstream codex repo at
// codex-rs/app-server-protocol/schema/typescript/v2/McpServerElicitationRequestParams.ts.
type ElicitationRequest struct {
	Mode       string `json:"mode"`                 // "form" or "url"
	Message    string `json:"message"`              // human-readable prompt shown to the user
	ServerName string `json:"serverName,omitempty"` // name of the MCP server issuing the request

	// Form mode only.
	RequestedSchema json.RawMessage `json:"requestedSchema,omitempty"`

	// URL mode only.
	URL           string `json:"url,omitempty"`
	ElicitationID string `json:"elicitationId,omitempty"`
}

// ElicitationResolution carries the MCP elicitation response fields.
type ElicitationResolution struct {
	Action  string          `json:"action"`
	Content json.RawMessage `json:"content,omitempty"`
	Meta    json.RawMessage `json:"meta,omitempty"`
}

// ApprovalResponse is sent back to the provider.
type ApprovalResponse struct {
	RequestID   string                 `json:"requestId"`
	Decision    string                 `json:"decision"`              // Codex-native: "accept", "acceptForSession", "decline", "cancel"
	Permissions *PermissionProfile     `json:"permissions,omitempty"` // for granted permissions
	Scope       string                 `json:"scope,omitempty"`       // "turn"|"session" for permissions
	Elicitation *ElicitationResolution `json:"elicitation,omitempty"` // for MCP elicitation responses
	// UpdatedInput replaces the original tool input when an approval is granted.
	// Only meaningful for allow decisions; ignored on deny. Opaque JSON — the
	// shape mirrors the tool's input schema, which is provider-specific.
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
	// UpdatedPermissions mirrors the Claude SDK's `updatedPermissions` field on
	// allow decisions: a JSON array of PermissionUpdate objects used to broaden
	// or narrow the session's permission scope. Ignored on deny.
	UpdatedPermissions json.RawMessage `json:"updatedPermissions,omitempty"`
}

// UserInputResponse is sent back to the provider for a structured user-input
// request.
type UserInputResponse struct {
	RequestID string                     `json:"requestId"`
	Decision  string                     `json:"decision"` // accept; empty is treated as accept
	Answers   map[string]UserInputAnswer `json:"answers,omitempty"`
}

// ErrStaleInteractiveRequest marks provider errors for approval/user-input
// callbacks that no longer have a live provider request behind them.
var ErrStaleInteractiveRequest = errors.New("provider: stale interactive request")

// ErrInvalidUserInputDecision marks a user-input response whose decision
// value is not a recognized accept or decline synonym.
var ErrInvalidUserInputDecision = errors.New("provider: invalid user-input decision")

func NormalizeUserInputDecision(decision string) (string, error) {
	switch decision {
	case "", "accept", "allow":
		return "accept", nil
	case "deny", "decline", "cancel":
		return "decline", nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidUserInputDecision, decision)
	}
}

// NormalizeApprovalDecision converts transport-specific approval response
// values ("allow", "accept", "decline", "deny", etc.) into the persisted item
// decision enum used by the chat rewrite.
func NormalizeApprovalDecision(resp ApprovalResponse) string {
	switch resp.Decision {
	case "allow", "allow_session", "accept", "acceptForSession":
		if len(resp.UpdatedInput) > 0 || len(resp.UpdatedPermissions) > 0 {
			return "amended"
		}
		return "approved"
	case "deny", "decline", "cancel":
		return "declined"
	default:
		return ""
	}
}

// UserInputQuestionOption is a selectable option in a user-input question.
// Preview, when non-empty, is markdown-rendered alongside the option list as
// a side-by-side mockup/code comparison aid. Preview is single-select only;
// Claude Code's tool spec ignores it on multi-select questions.
type UserInputQuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Preview     string `json:"preview,omitempty"`
}

// UserInputQuestion represents a question in a structured user-input request.
type UserInputQuestion struct {
	ID          string                    `json:"id"`
	Header      string                    `json:"header"`
	Question    string                    `json:"question"`
	Options     []UserInputQuestionOption `json:"options,omitempty"`
	MultiSelect bool                      `json:"multiSelect,omitempty"`
}

func NormalizeUserInputQuestions(questions []UserInputQuestion) []UserInputQuestion {
	normalized := make([]UserInputQuestion, 0, len(questions))
	seen := make(map[string]int, len(questions))
	for i, question := range questions {
		question.ID = normalizeUserInputQuestionID(question, i, seen)
		if strings.TrimSpace(question.Header) == "" {
			question.Header = fmt.Sprintf("Question %d", i+1)
		}
		normalized = append(normalized, question)
	}
	return normalized
}

func normalizeUserInputQuestionID(question UserInputQuestion, index int, seen map[string]int) string {
	candidates := []string{question.ID, question.Header, question.Question, fmt.Sprintf("q-%d", index)}
	id := fmt.Sprintf("q-%d", index)
	for _, candidate := range candidates {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" || isReservedUserInputQuestionID(trimmed) {
			continue
		}
		id = trimmed
		break
	}
	count := seen[id]
	seen[id] = count + 1
	if count == 0 {
		return id
	}
	return fmt.Sprintf("%s-%d", id, count+1)
}

func isReservedUserInputQuestionID(id string) bool {
	switch id {
	case "__proto__", "prototype", "constructor":
		return true
	default:
		return false
	}
}

// UserInputAnswer stores one or more selected answers for a question.
// It marshals as a string for single-select answers and a string array for
// multi-select answers to match the frontend contract.
type UserInputAnswer []string

// SingleUserInputAnswer constructs a single-answer response value.
func SingleUserInputAnswer(value string) UserInputAnswer {
	return UserInputAnswer{value}
}

// MarshalJSON emits a bare string for single answers and an array for
// multi-select answers.
func (a UserInputAnswer) MarshalJSON() ([]byte, error) {
	if len(a) == 1 {
		return json.Marshal(a[0])
	}
	return json.Marshal([]string(a))
}

// UnmarshalJSON accepts either a single string or an array of strings.
func (a *UserInputAnswer) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*a = UserInputAnswer{single}
		return nil
	}

	var multiple []string
	if err := json.Unmarshal(data, &multiple); err == nil {
		*a = UserInputAnswer(multiple)
		return nil
	}

	return fmt.Errorf("user input answer must be a string or []string")
}

// PermissionProfile describes requested or granted permissions.
type PermissionProfile struct {
	Network    *NetworkPermissions    `json:"network,omitempty"`
	FileSystem *FileSystemPermissions `json:"fileSystem,omitempty"`
}

// NetworkPermissions controls network access.
type NetworkPermissions struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// FileSystemPermissions controls filesystem access.
type FileSystemPermissions struct {
	Read  []string `json:"read,omitempty"`
	Write []string `json:"write,omitempty"`
}
