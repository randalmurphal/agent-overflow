package claudetui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// hookRelay is the per-session loopback endpoint the __claude-hook subcommand
// posts every hook payload to. It is a privileged boundary: loopback-only,
// authenticated by a per-session capability token. Observe-mode events are
// reconstructed into envelopes and returned immediately (fire-and-forget so the
// agent loop never stalls); AskUserQuestion blocks until the human answers in
// AO. See docs/architecture/claude-tui-provider.md §The hook relay.
type hookRelay struct {
	token     string
	feed      func(json.RawMessage)
	onSession func(sessionID, cwd, version string)
	onError   func(error)

	ln     net.Listener
	server *http.Server

	mu      sync.Mutex
	pending map[string]*pendingQuestion
	closed  bool
	seq     int
}

// pendingQuestion holds the state of one in-flight AskUserQuestion awaiting the
// human's answer. answer carries the hook stdout JSON back to the blocked HTTP
// handler.
type pendingQuestion struct {
	questions []provider.UserInputQuestion
	toolInput json.RawMessage
	answer    chan json.RawMessage
}

// answerTimeout bounds how long the relay holds a hook open for a human answer
// before falling through to the native TUI prompt (the take-control escape
// hatch). Generous because the spike confirmed Claude imposes no hook-timeout
// clamp; the human gets a real window.
const answerTimeout = 10 * time.Minute

// hookAuthHeader carries the per-session capability token.
const hookAuthHeader = "X-AO-Hook-Token"

var errRelayClosed = errors.New("claudetui: hook relay closed")

// newHookRelay binds a loopback listener and mints a capability token. feed
// enqueues reconstructed envelopes through the session's parser; onSession
// records identity from SessionStart.
func newHookRelay(feed func(json.RawMessage), onSession func(string, string, string), onError func(error)) (*hookRelay, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("claudetui hook relay: bind loopback: %w", err)
	}
	token, err := mintToken()
	if err != nil {
		ln.Close()
		return nil, err
	}
	h := &hookRelay{
		token:     token,
		feed:      feed,
		onSession: onSession,
		onError:   onError,
		ln:        ln,
		pending:   map[string]*pendingQuestion{},
	}
	h.server = &http.Server{
		Handler:           http.HandlerFunc(h.handle),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return h, nil
}

func (h *hookRelay) url() string       { return "http://" + h.ln.Addr().String() }
func (h *hookRelay) authToken() string { return h.token }

func (h *hookRelay) start() {
	go func() {
		if err := h.server.Serve(h.ln); err != nil && err != http.ErrServerClosed {
			h.onError(fmt.Errorf("claudetui hook relay serve: %w", err))
		}
	}()
}

// close shuts the relay down and unblocks any pending AskUserQuestion handlers
// so their hooks fall through rather than hang.
func (h *hookRelay) close() error {
	h.mu.Lock()
	h.closed = true
	for id, p := range h.pending {
		close(p.answer)
		delete(h.pending, id)
	}
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return h.server.Shutdown(ctx)
}

func (h *hookRelay) handle(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r.RemoteAddr) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(hookAuthHeader)), []byte(h.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	p, err := parseHookPayload(raw)
	if err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	h.dispatch(w, r, p)
}

// dispatch routes a hook payload. Observe-mode events reconstruct an envelope,
// enqueue it, and return an empty 200 at once. AskUserQuestion blocks.
func (h *hookRelay) dispatch(w http.ResponseWriter, r *http.Request, p hookPayload) {
	switch p.HookEventName {
	case "PostToolUse":
		h.feed(postToolUseEnvelope(p))
		writeEmptyOK(w)
	case "PostToolUseFailure":
		h.feed(postToolUseFailureEnvelope(p))
		writeEmptyOK(w)
	case "SessionStart":
		h.onSession(p.SessionID, p.CWD, "")
		writeEmptyOK(w)
	case "PostCompact":
		// v1 emits the boundary with the hook's trigger so the timeline marks
		// the compaction; wire-side preTokens enrichment is a later phase.
		h.feed(compactBoundaryEnvelope(p))
		writeEmptyOK(w)
	case "PreCompact":
		// Advisory only — PreCompact fires before the "enough messages" check,
		// so it is not proof a compaction happened. PostCompact / the wire own
		// the boundary.
		writeEmptyOK(w)
	case "PreToolUse":
		if p.ToolName == "AskUserQuestion" {
			h.handleAskUserQuestion(w, r, p)
			return
		}
		// Full-access auto-allows every other tool; nothing to gate.
		writeEmptyOK(w)
	default:
		writeEmptyOK(w)
	}
}

// handleAskUserQuestion surfaces the question to AO and blocks until the human
// answers (or the window elapses, falling through to the native TUI prompt).
func (h *hookRelay) handleAskUserQuestion(w http.ResponseWriter, r *http.Request, p hookPayload) {
	requestID := h.allocID()
	pend := &pendingQuestion{
		questions: normalizedQuestions(p.ToolInput),
		toolInput: p.ToolInput,
		answer:    make(chan json.RawMessage, 1),
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		writeEmptyOK(w) // session tearing down — let the TUI handle it
		return
	}
	h.pending[requestID] = pend
	h.mu.Unlock()

	// Surface EventUserInputRequest through the shared parser, identical to the
	// headless can_use_tool path.
	h.feed(askUserQuestionControlRequest(requestID, p))

	ctx, cancel := context.WithTimeout(r.Context(), answerTimeout)
	defer cancel()
	select {
	case out, ok := <-pend.answer:
		if !ok || len(out) == 0 {
			writeEmptyOK(w) // relay closed / fall-through
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	case <-ctx.Done():
		h.discard(requestID)
		writeEmptyOK(w) // window elapsed — native TUI prompt takes over
	}
}

// respond delivers the human's answer to the blocked AskUserQuestion handler,
// projecting selections into the updatedInput Claude Code's tool consumes.
// Returns ErrAlreadyResolved-style failure if no such request is pending.
func (h *hookRelay) respond(requestID string, answers map[string]provider.UserInputAnswer) error {
	h.mu.Lock()
	pend, ok := h.pending[requestID]
	if ok {
		delete(h.pending, requestID)
	}
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("claudetui: no pending user-input request %q", requestID)
	}

	projected := claude.AskUserQuestionAnswers(pend.questions, answers)
	updated, err := mergeAnswersIntoInput(pend.toolInput, projected)
	if err != nil {
		return err
	}
	select {
	case pend.answer <- askUserQuestionHookOutput(updated):
		return nil
	default:
		return errRelayClosed
	}
}

func (h *hookRelay) discard(requestID string) {
	h.mu.Lock()
	delete(h.pending, requestID)
	h.mu.Unlock()
}

func (h *hookRelay) allocID() string {
	h.mu.Lock()
	h.seq++
	id := h.seq
	h.mu.Unlock()
	return fmt.Sprintf("aoq_%d", id)
}

// --- helpers --------------------------------------------------------------

// normalizedQuestions parses and normalizes the AskUserQuestion questions the
// SAME way parse_control does, so the answer-projection keys match what the
// frontend was shown.
func normalizedQuestions(toolInput json.RawMessage) []provider.UserInputQuestion {
	var payload struct {
		Questions []provider.UserInputQuestion `json:"questions"`
	}
	if json.Unmarshal(toolInput, &payload) != nil {
		return nil
	}
	return provider.NormalizeUserInputQuestions(payload.Questions)
}

// mergeAnswersIntoInput echoes the original tool_input back intact (the spike
// found a partial updatedInput makes the TUI re-prompt) with the projected
// answers added.
func mergeAnswersIntoInput(toolInput json.RawMessage, answers map[string]string) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if len(toolInput) > 0 {
		if err := json.Unmarshal(toolInput, &fields); err != nil {
			return nil, fmt.Errorf("claudetui: decode tool_input: %w", err)
		}
	}
	fields["answers"] = mustMarshal(answers)
	return mustMarshal(fields), nil
}

// askUserQuestionHookOutput wraps the updatedInput in the PreToolUse
// hookSpecificOutput that answers the tool with permissionDecision allow.
func askUserQuestionHookOutput(updatedInput json.RawMessage) json.RawMessage {
	return mustMarshal(map[string]any{
		"hookSpecificOutput": map[string]json.RawMessage{
			"hookEventName":            mustMarshal("PreToolUse"),
			"permissionDecision":       mustMarshal("allow"),
			"permissionDecisionReason": mustMarshal("Answered in Agent Overflow"),
			"updatedInput":             updatedInput,
		},
	})
}

func writeEmptyOK(w http.ResponseWriter) {
	w.WriteHeader(http.StatusOK)
}

func mintToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("claudetui: mint hook token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}
