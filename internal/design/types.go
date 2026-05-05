package design

// Tool name constants for the design-mode wire surface. The same
// names appear in the system prompt and in both providers' MCP tool
// list — keep them centralized so a rename moves all three together.
const (
	// ToolGetDiagnostics returns runtime diagnostics captured from the
	// design iframe (console errors, window errors, unhandled
	// rejections) since the agent's last call. Both providers expose
	// this via real MCP — Codex through its HTTP MCP server, Claude
	// through --mcp-config.
	ToolGetDiagnostics = "get_design_diagnostics"

	// ToolReadScreenshot captures the live design iframe and returns
	// a PNG. Round-trips through the frontend.
	ToolReadScreenshot = "read_screenshot"
)

// DiagnosticSeverity classifies a captured runtime event.
type DiagnosticSeverity string

const (
	SeverityError DiagnosticSeverity = "error"
	SeverityWarn  DiagnosticSeverity = "warn"
	SeverityInfo  DiagnosticSeverity = "info"
)

// Diagnostic is one captured runtime event from the sandboxed iframe.
// Tokens are monotonic per thread; agents pass `since_token` to drain
// only what they haven't seen.
type Diagnostic struct {
	Token     int64              `json:"token"`
	Severity  DiagnosticSeverity `json:"severity"`
	Message   string             `json:"message"`
	Source    string             `json:"source,omitempty"`
	Line      int                `json:"line,omitempty"`
	Column    int                `json:"column,omitempty"`
	Stack     string             `json:"stack,omitempty"`
	URL       string             `json:"url,omitempty"`
	CreatedAt int64              `json:"createdAt"`
}

// DiagnosticBatch is the wire payload from the iframe-injected capture
// script forwarded by the frontend over WebSocket.
type DiagnosticBatch struct {
	ThreadID    string       `json:"threadId"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// SliderChange is one knob update inside a feedback batch.
type SliderChange struct {
	ID    string  `json:"id"`
	Value float64 `json:"value"`
}

// FeedbackBatch carries accumulated user feedback for one round trip.
// The frontend serialises this and sends it as a regular user message;
// the agent reads it as input on the next turn.
type FeedbackBatch struct {
	SliderChanges []SliderChange `json:"sliderChanges,omitempty"`
	Notes         string         `json:"notes,omitempty"`
}

// ClarificationChoice is one selectable answer within a clarification
// question.
type ClarificationChoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// ClarificationQuestion is a single multiple-choice clarification.
type ClarificationQuestion struct {
	ID       string                `json:"id"`
	Prompt   string                `json:"prompt"`
	Choices  []ClarificationChoice `json:"choices"`
	Multiple bool                  `json:"multiple,omitempty"`
}

// ClarificationRequest is emitted by the agent as a structured
// assistant-text payload when it needs the user to commit to a design
// direction before continuing.
type ClarificationRequest struct {
	RequestID string                  `json:"requestId"`
	ThreadID  string                  `json:"threadId"`
	Intro     string                  `json:"intro,omitempty"`
	Questions []ClarificationQuestion `json:"questions"`
}

// SliderControl is one agent-emitted slider exposed in the feedback
// panel after a design iteration lands.
type SliderControl struct {
	ID    string  `json:"id"`
	Label string  `json:"label"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Step  float64 `json:"step,omitempty"`
	Value float64 `json:"value"`
}

// ExposeControls is the agent → frontend signal that the user can now
// tweak these knobs. Updates here replace the previous control set.
type ExposeControls struct {
	ThreadID string          `json:"threadId"`
	Controls []SliderControl `json:"controls"`
}

// OptionChosen is the user → agent message issued when the user picks
// one of the components the agent placed in `options/{setId}/`.
type OptionChosen struct {
	SetID    string `json:"setId"`
	OptionID string `json:"optionId"`
	Path     string `json:"path"`
}

// ScreenshotRequest is the backend → frontend signal that the agent
// has called `read_screenshot` and the frontend should capture the
// live iframe.
type ScreenshotRequest struct {
	ThreadID  string `json:"threadId"`
	RequestID string `json:"requestId"`
}

// ScreenshotResult is the frontend → backend reply carrying the
// captured PNG bytes (base64-encoded on the wire).
type ScreenshotResult struct {
	RequestID string `json:"requestId"`
	PNGBase64 string `json:"pngBase64"`
}
