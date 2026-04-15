package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strconv"
	"time"

	"agent-overflow/internal/provider"
)

// Session manages a Claude Code CLI subprocess.
type Session struct {
	proc      *provider.Process
	threadID  string
	sessionID string
	model     string
	onEvent   func(provider.ProviderEvent)
	cancel    context.CancelFunc
}

// Config for creating a Claude session.
type Config struct {
	Binary         string   // default: "claude"
	Model          string
	WorkDir        string
	Resume         string   // session ID to resume, empty for new
	SystemPrompt   string
	AllowedTools   []string
	PermissionMode string // "default", "acceptEdits", "bypassPermissions"
	MaxTurns       int
}

// NewSession spawns a Claude CLI process and starts the stdout reader goroutine.
// The init event arrives after the first Send() call, not on spawn.
func NewSession(ctx context.Context, threadID string, cfg Config, onEvent func(provider.ProviderEvent)) (*Session, error) {
	binary := cfg.Binary
	if binary == "" {
		binary = "claude"
	}

	args := buildArgs(cfg)

	childCtx, cancel := context.WithCancel(ctx)

	proc, err := provider.Spawn(childCtx, provider.SpawnConfig{
		Binary: binary,
		Args:   args,
		Dir:    cfg.WorkDir,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claude: spawn: %w", err)
	}

	s := &Session{
		proc:     proc,
		threadID: threadID,
		model:    cfg.Model,
		onEvent:  onEvent,
		cancel:   cancel,
	}

	go s.readLoop()

	return s, nil
}

// buildArgs constructs CLI flags from Config.
func buildArgs(cfg Config) []string {
	args := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}

	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Resume != "" {
		args = append(args, "--resume", cfg.Resume)
	}
	if cfg.SystemPrompt != "" {
		args = append(args, "--system-prompt", cfg.SystemPrompt)
	}
	if cfg.PermissionMode != "" && cfg.PermissionMode != "default" {
		args = append(args, "--permission-mode", cfg.PermissionMode)
	}
	if cfg.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(cfg.MaxTurns))
	}
	for _, tool := range cfg.AllowedTools {
		args = append(args, "--allowedTools", tool)
	}

	return args
}

// Send sends a user message. The message is written as a JSON object to stdin.
func (s *Session) Send(ctx context.Context, content string) error {
	msg := map[string]any{
		"type": "user",
		"message": map[string]string{
			"role":    "user",
			"content": content,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("claude: marshal user message: %w", err)
	}
	return s.proc.WriteLine(data)
}

// Interrupt sends a control interrupt to the CLI.
func (s *Session) Interrupt(ctx context.Context) error {
	msg := map[string]any{
		"type": "control",
		"control": map[string]string{
			"type": "interrupt",
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("claude: marshal interrupt: %w", err)
	}
	return s.proc.WriteLine(data)
}

// RespondToApproval sends a tool-use approval decision back to the CLI.
func (s *Session) RespondToApproval(ctx context.Context, resp provider.ApprovalResponse) error {
	var behavior map[string]any
	if resp.Decision == "allow" || resp.Decision == "allow_session" {
		behavior = map[string]any{"behavior": "allow"}
	} else {
		behavior = map[string]any{"behavior": "deny", "message": "User denied"}
	}
	msg := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": resp.RequestID,
			"response":   behavior,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("claude: marshal approval response: %w", err)
	}
	return s.proc.WriteLine(data)
}

// SessionID returns the provider's session identifier.
// Only valid after the init event has been received.
func (s *Session) SessionID() string {
	return s.sessionID
}

// Close shuts down the CLI process gracefully.
// Closes stdin first for graceful shutdown, then cancels the context as fallback.
func (s *Session) Close() error {
	err := s.proc.Close()
	s.cancel()
	return err
}

// readLoop reads stdout NDJSON lines and dispatches them as ProviderEvents.
func (s *Session) readLoop() {
	defer func() {
		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventSessionStatus,
			ThreadID:  s.threadID,
			Content:   "disconnected",
			Timestamp: time.Now(),
		})
	}()

	for {
		line, err := s.proc.ReadLine()
		if err != nil {
			if err != io.EOF {
				s.onEvent(provider.ProviderEvent{
					Kind:      provider.EventError,
					ThreadID:  s.threadID,
					Content:   fmt.Sprintf("claude: read error: %v", err),
					Timestamp: time.Now(),
				})
			}
			return
		}

		events, err := ParseLine(s.threadID, line)
		if err != nil {
			log.Printf("claude: parse error: %v (line: %s)", err, string(line[:min(len(line), 200)]))
			continue
		}

		for _, evt := range events {
			if evt.Kind == provider.EventInit && evt.Meta != nil {
				var info provider.SessionInfo
				if json.Unmarshal(evt.Meta, &info) == nil && info.SessionID != "" {
					s.sessionID = info.SessionID
				}
			}
			s.onEvent(evt)
		}
	}
}
