// Package acp implements a provider-neutral, multiplexed Agent Client Protocol
// (ACP v1) client over JSON-RPC 2.0 NDJSON stdio. One Conn hosts one ACP server
// subprocess and demultiplexes many Session conversations by sessionId.
// Provider-specific extensions live behind the Flavor hook interface.
package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const ProtocolVersion = 1

// Standard ACP / JSON-RPC error codes.
const (
	ErrCodeParseError     = -32700
	ErrCodeInvalidRequest = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternalError  = -32603
	ErrCodeAuthRequired   = -32000
	ErrCodeResourceNotFnd = -32002
)

// ErrNoActiveTurn is returned by Session.Steer when no turn is currently active
// on the session, signalling the caller to start a fresh turn instead.
var ErrNoActiveTurn = errors.New("acp: no active turn to steer")

// RPCError represents a JSON-RPC 2.0 error returned by an ACP agent.
type RPCError struct {
	Method  string
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	if e.Method != "" {
		return fmt.Sprintf("acp: %s: %s (code %d)", e.Method, e.Message, e.Code)
	}
	return fmt.Sprintf("acp: %s (code %d)", e.Message, e.Code)
}

func IsMethodNotFound(err error) bool {
	var rpcErr *RPCError
	return errors.As(err, &rpcErr) && rpcErr.Code == ErrCodeMethodNotFound
}

func IsAuthRequired(err error) bool {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	if rpcErr.Code == ErrCodeAuthRequired {
		return true
	}
	msg := strings.ToLower(rpcErr.Message)
	return strings.Contains(msg, "authentication required") || strings.Contains(msg, "not authenticated")
}

func IsSessionNotFound(err error) bool {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	if rpcErr.Code == ErrCodeResourceNotFnd {
		return true
	}
	msg := strings.ToLower(rpcErr.Message)
	return strings.Contains(msg, "session not found") ||
		strings.Contains(msg, "failed to read session") ||
		strings.Contains(msg, "unknown session")
}

// ClientCapabilities is sent on initialize.
type ClientCapabilities struct {
	FS       *FSCapabilities `json:"fs,omitempty"`
	Terminal bool            `json:"terminal,omitempty"`
}

type FSCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
	ClientInfo         ClientInfo         `json:"clientInfo"`
}

type InitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AgentInfo         AgentInfo         `json:"agentInfo"`
	AuthMethods       []AuthMethod      `json:"authMethods,omitempty"`
}

type AgentInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

type MCPCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

type SessionCapabilities struct {
	Resume                *json.RawMessage `json:"resume,omitempty"`
	Close                 *json.RawMessage `json:"close,omitempty"`
	Load                  *json.RawMessage `json:"load,omitempty"`
	List                  *json.RawMessage `json:"list,omitempty"`
	Fork                  *json.RawMessage `json:"fork,omitempty"`
	AdditionalDirectories *json.RawMessage `json:"additionalDirectories,omitempty"`
}

type AuthCapabilities struct {
	Logout *json.RawMessage `json:"logout,omitempty"`
}

// AgentCapabilities tolerates both camelCase and snake_case keys emitted by
// ACP servers (e.g. agy emits both loadSession and load_session).
type AgentCapabilities struct {
	LoadSession         bool                `json:"loadSession"`
	PromptCapabilities  PromptCapabilities  `json:"promptCapabilities"`
	SessionCapabilities SessionCapabilities `json:"sessionCapabilities"`
	MCPCapabilities     MCPCapabilities     `json:"mcpCapabilities"`
	Auth                AuthCapabilities    `json:"auth"`
	Meta                json.RawMessage     `json:"_meta,omitempty"`
}

func (c *AgentCapabilities) UnmarshalJSON(data []byte) error {
	var raw struct {
		LoadSession             *bool                `json:"loadSession"`
		LoadSessionSnake        *bool                `json:"load_session"`
		PromptCapabilities      *PromptCapabilities  `json:"promptCapabilities"`
		PromptCapabilitiesSnake *PromptCapabilities  `json:"prompt_capabilities"`
		SessionCapabilities     *SessionCapabilities `json:"sessionCapabilities"`
		SessionCapsSnake        *SessionCapabilities `json:"session_capabilities"`
		MCPCapabilities         *MCPCapabilities     `json:"mcpCapabilities"`
		MCPCapabilitiesSnake    *MCPCapabilities     `json:"mcp_capabilities"`
		Auth                    *AuthCapabilities    `json:"auth"`
		Meta                    json.RawMessage      `json:"_meta"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.LoadSession != nil {
		c.LoadSession = *raw.LoadSession
	} else if raw.LoadSessionSnake != nil {
		c.LoadSession = *raw.LoadSessionSnake
	}
	if raw.PromptCapabilities != nil {
		c.PromptCapabilities = *raw.PromptCapabilities
	} else if raw.PromptCapabilitiesSnake != nil {
		c.PromptCapabilities = *raw.PromptCapabilitiesSnake
	}
	if raw.SessionCapabilities != nil {
		c.SessionCapabilities = *raw.SessionCapabilities
	} else if raw.SessionCapsSnake != nil {
		c.SessionCapabilities = *raw.SessionCapsSnake
	}
	if raw.MCPCapabilities != nil {
		c.MCPCapabilities = *raw.MCPCapabilities
	} else if raw.MCPCapabilitiesSnake != nil {
		c.MCPCapabilities = *raw.MCPCapabilitiesSnake
	}
	if raw.Auth != nil {
		c.Auth = *raw.Auth
	}
	c.Meta = raw.Meta
	return nil
}

// HTTPHeader is one name/value header pair on an HTTP/SSE MCP server entry.
type HTTPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// EnvVar is one name/value environment variable on a stdio MCP server entry.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MCPServerConfig follows the ACP v1 mcpServers array entry shape.
type MCPServerConfig struct {
	Type    string       `json:"type,omitempty"` // "http", "sse", or "" (stdio)
	Name    string       `json:"name"`
	URL     string       `json:"url,omitempty"`
	Headers []HTTPHeader `json:"headers,omitempty"`
	Command string       `json:"command,omitempty"`
	Args    []string     `json:"args,omitempty"`
	Env     []EnvVar     `json:"env,omitempty"`
}

// ContentBlock is one element in session/prompt's prompt array or in chunks.
type ContentBlock struct {
	Type     string            `json:"type"` // "text", "image", "audio", "resource", "resource_link"
	Text     string            `json:"text,omitempty"`
	Data     string            `json:"data,omitempty"`
	MimeType string            `json:"mimeType,omitempty"`
	URI      string            `json:"uri,omitempty"`
	Name     string            `json:"name,omitempty"`
	Size     int64             `json:"size,omitempty"`
	Resource *EmbeddedResource `json:"resource,omitempty"`
}

type EmbeddedResource struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

type SessionNewParams struct {
	CWD                   string            `json:"cwd"`
	MCPServers            []MCPServerConfig `json:"mcpServers"`
	AdditionalDirectories []string          `json:"additionalDirectories,omitempty"`
	Meta                  map[string]any    `json:"_meta,omitempty"`
}

type SessionResumeParams struct {
	SessionID             string            `json:"sessionId"`
	CWD                   string            `json:"cwd"`
	MCPServers            []MCPServerConfig `json:"mcpServers"`
	AdditionalDirectories []string          `json:"additionalDirectories,omitempty"`
	Meta                  map[string]any    `json:"_meta,omitempty"`
}

type SessionLoadParams struct {
	SessionID             string            `json:"sessionId"`
	CWD                   string            `json:"cwd"`
	MCPServers            []MCPServerConfig `json:"mcpServers"`
	AdditionalDirectories []string          `json:"additionalDirectories,omitempty"`
	Meta                  map[string]any    `json:"_meta,omitempty"`
}

type SessionOpenResult struct {
	SessionID     string          `json:"sessionId"`
	Models        *ModelsState    `json:"models,omitempty"`
	Modes         *SessionModes   `json:"modes,omitempty"`
	ConfigOptions []ConfigOption  `json:"configOptions,omitempty"`
	Meta          json.RawMessage `json:"_meta,omitempty"`
}

type ModelsState struct {
	CurrentModelID  string       `json:"currentModelId"`
	AvailableModels []ModelEntry `json:"availableModels,omitempty"`
}

type ModelEntry struct {
	ModelID     string `json:"modelId"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type SessionModes struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []SessionMode `json:"availableModes,omitempty"`
}

type SessionMode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type ConfigOption struct {
	ID           string               `json:"id"`
	Name         string               `json:"name"`
	Description  string               `json:"description,omitempty"`
	Category     string               `json:"category,omitempty"`
	Type         string               `json:"type"`
	CurrentValue string               `json:"currentValue"`
	Options      []ConfigSelectOption `json:"options,omitempty"`
}

type ConfigSelectOption struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type SessionListParams struct {
	CWD    string `json:"cwd,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type SessionListResult struct {
	Sessions   []SessionSummary `json:"sessions"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

type SessionSummary struct {
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Title     string `json:"title,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type SessionCloseParams struct {
	SessionID string `json:"sessionId"`
}

type SessionPromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

type Usage struct {
	InputTokens       int `json:"inputTokens"`
	OutputTokens      int `json:"outputTokens"`
	CachedReadTokens  int `json:"cachedReadTokens"`
	CachedWriteTokens int `json:"cachedWriteTokens"`
	ThoughtTokens     int `json:"thoughtTokens"`
	TotalTokens       int `json:"totalTokens"`
}

type SessionPromptResult struct {
	StopReason string          `json:"stopReason"`
	Usage      *Usage          `json:"usage,omitempty"`
	Meta       json.RawMessage `json:"_meta,omitempty"`
}

type SessionCancelParams struct {
	SessionID string `json:"sessionId"`
}

type SessionSetModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

type SessionSetConfigOptionParams struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Value     string `json:"value"`
}

type SessionSetConfigOptionResult struct {
	ConfigOptions []ConfigOption `json:"configOptions,omitempty"`
}

type SessionSetModelParams struct {
	SessionID string `json:"sessionId"`
	ModelID   string `json:"modelId"`
}

// SessionUpdateParams is the params payload of a session/update notification.
type SessionUpdateParams struct {
	SessionID string        `json:"sessionId"`
	Update    SessionUpdate `json:"update"`
}

// SessionUpdate is the polymorphic update object inside session/update.
// Content stays json.RawMessage because chunk updates send a single object
// while tool updates send an array of content/diff items.
type SessionUpdate struct {
	SessionUpdate     string             `json:"sessionUpdate"`
	Content           json.RawMessage    `json:"content,omitempty"`
	ToolCallID        string             `json:"toolCallId,omitempty"`
	Name              string             `json:"name,omitempty"`
	Args              json.RawMessage    `json:"args,omitempty"`
	Status            string             `json:"status,omitempty"`
	Kind              string             `json:"kind,omitempty"`
	Title             string             `json:"title,omitempty"`
	Locations         []ToolLocation     `json:"locations,omitempty"`
	RawInput          json.RawMessage    `json:"rawInput,omitempty"`
	RawOutput         json.RawMessage    `json:"rawOutput,omitempty"`
	Entries           []PlanEntry        `json:"entries,omitempty"`
	Used              int                `json:"used,omitempty"`
	Size              int                `json:"size,omitempty"`
	AvailableCommands []AvailableCommand `json:"availableCommands,omitempty"`
	ConfigOptions     []ConfigOption     `json:"configOptions,omitempty"`
	CurrentModeID     string             `json:"currentModeId,omitempty"`
	UpdatedAt         string             `json:"updatedAt,omitempty"`
	Meta              json.RawMessage    `json:"_meta,omitempty"`
}

type ToolLocation struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
}

// ToolContentItem is one element of a tool_call / tool_call_update content array.
type ToolContentItem struct {
	Type    string        `json:"type"` // "diff", "content", "terminal"
	Path    string        `json:"path,omitempty"`
	OldText string        `json:"oldText,omitempty"`
	NewText string        `json:"newText,omitempty"`
	Content *ContentBlock `json:"content,omitempty"`
	ID      string        `json:"terminalId,omitempty"`
}

type PlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority,omitempty"`
	Status   string `json:"status"` // "pending", "in_progress", "completed"
}

type AvailableCommand struct {
	Name        string                `json:"name"`
	Description string                `json:"description,omitempty"`
	Input       *AvailableCommandHint `json:"input,omitempty"`
}

type AvailableCommandHint struct {
	Hint string `json:"hint,omitempty"`
}

// ToolCallUpdate is the structured view of a tool_call, tool_call_update, or
// permission toolCall object.
type ToolCallUpdate struct {
	ToolCallID string            `json:"toolCallId"`
	Name       string            `json:"name,omitempty"`
	Args       json.RawMessage   `json:"args,omitempty"`
	Status     string            `json:"status,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Title      string            `json:"title,omitempty"`
	Locations  []ToolLocation    `json:"locations,omitempty"`
	RawInput   json.RawMessage   `json:"rawInput,omitempty"`
	RawOutput  json.RawMessage   `json:"rawOutput,omitempty"`
	Content    []ToolContentItem `json:"content,omitempty"`
	Meta       json.RawMessage   `json:"_meta,omitempty"`
}

// EffectiveInput returns RawInput if non-empty, else Args.
func (t *ToolCallUpdate) EffectiveInput() json.RawMessage {
	if len(t.RawInput) > 0 && string(t.RawInput) != "null" {
		return t.RawInput
	}
	if len(t.Args) > 0 && string(t.Args) != "null" {
		return t.Args
	}
	return nil
}

// RequestPermissionParams is sent by the agent as a server-initiated JSON-RPC request.
type RequestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  ToolCallUpdate     `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

type PermissionOption struct {
	OptionID string          `json:"optionId"`
	Name     string          `json:"name"`
	Kind     string          `json:"kind"` // "allow_once", "allow_always", "reject_once", "reject_always"
	Meta     json.RawMessage `json:"_meta,omitempty"`
}

type RequestPermissionResult struct {
	Outcome PermissionOutcome `json:"outcome"`
}

type PermissionOutcome struct {
	Outcome  string `json:"outcome"` // "selected" | "cancelled"
	OptionID string `json:"optionId,omitempty"`
}

func (u *SessionUpdate) decodeContentBlock() (ContentBlock, bool) {
	if len(u.Content) == 0 {
		return ContentBlock{}, false
	}
	var block ContentBlock
	if json.Unmarshal(u.Content, &block) != nil {
		return ContentBlock{}, false
	}
	return block, block.Type != "" || block.Text != ""
}

func (u *SessionUpdate) toToolCallUpdate() ToolCallUpdate {
	var items []ToolContentItem
	if len(u.Content) > 0 {
		_ = json.Unmarshal(u.Content, &items)
	}
	return ToolCallUpdate{
		ToolCallID: u.ToolCallID,
		Name:       u.Name,
		Args:       u.Args,
		Status:     u.Status,
		Kind:       u.Kind,
		Title:      u.Title,
		Locations:  u.Locations,
		RawInput:   u.RawInput,
		RawOutput:  u.RawOutput,
		Content:    items,
		Meta:       u.Meta,
	}
}

// redactOutboundACPFrame masks MCP HTTP header and env values in outbound
// session/new, session/resume, and session/load frames before raw event logging.
func redactOutboundACPFrame(direction string, data []byte) []byte {
	if direction != "out" || len(data) == 0 {
		return data
	}
	var envelope map[string]any
	if err := json.Unmarshal(data, &envelope); err != nil {
		return data
	}
	method, _ := envelope["method"].(string)
	switch method {
	case "session/new", "session/resume", "session/load":
	default:
		return data
	}
	params, ok := envelope["params"].(map[string]any)
	if !ok {
		return data
	}
	servers, ok := params["mcpServers"].([]any)
	if !ok || len(servers) == 0 {
		return data
	}
	mutated := false
	for _, entry := range servers {
		srv, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if headers, ok := srv["headers"].([]any); ok {
			for _, h := range headers {
				hm, ok := h.(map[string]any)
				if !ok {
					continue
				}
				if _, has := hm["value"]; has {
					hm["value"] = "[redacted]"
					mutated = true
				}
			}
		}
		if envList, ok := srv["env"].([]any); ok {
			for _, e := range envList {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				if _, has := em["value"]; has {
					em["value"] = "[redacted]"
					mutated = true
				}
			}
		}
	}
	if !mutated {
		return data
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return data
	}
	return out
}
