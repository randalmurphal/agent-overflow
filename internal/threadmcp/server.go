// Package threadmcp owns the thread-scoped HTTP transport shared by built-in MCP tools.
package threadmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/loopback"
	"github.com/google/uuid"
)

const mcpProtocolVersion = "2025-03-26"

// A 1 MiB script can expand sixfold in JSON (for example HTML escapes).
// Tool argument validation still enforces the smaller decoded limits.
const maxMCPRequestBytes = 8 << 20

type Server[T comparable] struct {
	name         string
	instructions string
	// instructionsFunc, when set, replaces the fixed instructions string on
	// every initialize. A server whose instructions depend on state the
	// caller recomputes per handshake (ao-thread-tools follows the paired
	// computer list) sets it; the others keep passing a constant.
	instructionsFunc func(T) string
	// disabledErr is the refusal a tools/call gets while this server or
	// this thread is switched off. Nil means the generic uncoded message;
	// a server with a documented refusal code supplies its own.
	disabledErr error
	tools       func(T) []map[string]any
	call        func(http.ResponseWriter, context.Context, Request, T)
	enabled     atomic.Bool

	mu            sync.Mutex
	closed        bool
	server        *http.Server
	listener      net.Listener
	baseURL       string
	threadToToken map[string]string
	tokenToThread map[string]string
	tokenToAccess map[string]T
	threadEnabled map[string]bool
}

func New[T comparable](name, instructions string, tools func(T) []map[string]any, call func(http.ResponseWriter, context.Context, Request, T)) *Server[T] {
	s := &Server[T]{name: name, instructions: instructions, tools: tools, call: call, threadToToken: make(map[string]string), tokenToThread: make(map[string]string), tokenToAccess: make(map[string]T), threadEnabled: make(map[string]bool)}
	s.enabled.Store(true)
	return s
}

func (s *Server[T]) SetEnabled(enabled bool) { s.enabled.Store(enabled) }

// SetInstructionsFunc makes the initialize instructions a function of the
// calling thread's access value. Call it before the first registration; it is
// not safe to change once threads are serving.
func (s *Server[T]) SetInstructionsFunc(fn func(T) string) { s.instructionsFunc = fn }

// SetDisabledError names the error a tools/call is refused with while the
// server or the thread is off, so a documented public code reaches the model
// instead of the generic message. Call it before the first registration.
func (s *Server[T]) SetDisabledError(err error) { s.disabledErr = err }

func (s *Server[T]) RegisterThread(threadID string, access T) (map[string]any, error) {
	if strings.TrimSpace(threadID) == "" {
		return nil, fmt.Errorf("MCP: thread is required")
	}
	if err := s.ensureStarted(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.threadToToken[threadID]
	delete(s.tokenToAccess, old)
	delete(s.tokenToThread, old)
	if s.closed {
		return nil, fmt.Errorf("MCP server is closed")
	}
	token := uuid.NewString()
	s.threadToToken[threadID] = token
	s.tokenToAccess[token] = access
	s.tokenToThread[token] = threadID
	if _, ok := s.threadEnabled[threadID]; !ok {
		s.threadEnabled[threadID] = true
	}
	return map[string]any{s.name: map[string]any{"url": s.baseURL + "/mcp/" + token}}, nil
}

func (s *Server[T]) UnregisterThread(threadID string) {
	s.mu.Lock()
	token := s.threadToToken[threadID]
	delete(s.threadToToken, threadID)
	delete(s.threadEnabled, threadID)
	if token != "" {
		delete(s.tokenToAccess, token)
		delete(s.tokenToThread, token)
	}
	s.mu.Unlock()

}

// RevokeThread retires only the named registration, never a replacement. The
// thread toggle survives a provider restart; UnregisterThread also forgets it.
func (s *Server[T]) RevokeThread(threadID string, expected T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token := s.threadToToken[threadID]
	if access, ok := s.tokenToAccess[token]; !ok || access != expected {
		return
	}
	delete(s.threadToToken, threadID)
	delete(s.tokenToAccess, token)
	delete(s.tokenToThread, token)
}

func (s *Server[T]) SetThreadEnabled(threadID string, enabled bool) {
	s.mu.Lock()
	s.threadEnabled[strings.TrimSpace(threadID)] = enabled
	s.mu.Unlock()
}

func (s *Server[T]) ThreadEnabled(threadID string) bool {
	s.mu.Lock()
	enabled, ok := s.threadEnabled[strings.TrimSpace(threadID)]
	s.mu.Unlock()
	return !ok || enabled
}

func (s *Server[T]) HasThread(threadID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.threadToToken[threadID]
	return ok
}
func (s *Server[T]) Close() error {
	s.mu.Lock()
	s.closed = true
	server, listener := s.server, s.listener
	s.server, s.listener, s.baseURL = nil, nil, ""
	s.threadToToken = make(map[string]string)
	s.tokenToThread = make(map[string]string)
	s.tokenToAccess = make(map[string]T)
	s.threadEnabled = make(map[string]bool)
	s.mu.Unlock()
	if server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(ctx)
	}
	if listener != nil {
		return listener.Close()
	}
	return nil
}

// ensureStarted binds the loopback listener on first thread
// registration. It cannot defer the bind until a tool is called: the
// endpoint URL rides the provider CLI's argv at spawn, so the listener
// has to exist before the process starts.
func (s *Server[T]) ensureStarted() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("MCP server is closed")
	}
	if s.server != nil {
		return nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("MCP: listen: %w", err)
	}
	server := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 35 * time.Second, WriteTimeout: 35 * time.Minute, IdleTimeout: 60 * time.Second}
	s.server, s.listener = server, listener
	s.baseURL = "http://" + listener.Addr().String()
	go func() { _ = server.Serve(listener) }()
	return nil
}

func (s *Server[T]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// An OPTIONS preflight lands here too, and answering it with 405
		// and no CORS headers is what the content-type check below relies
		// on: the browser stops before it sends the real request.
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !validMCPRequest(w, r) {
		return
	}
	threadID, access, ok := s.accessForPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMCPRequestBytes)
	defer r.Body.Close()
	var req Request
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&req); err != nil {
		WriteError(w, req.ID, http.StatusBadRequest, -32700, "invalid JSON")
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		WriteError(w, req.ID, http.StatusBadRequest, -32700, "invalid JSON")
		return
	}
	switch req.Method {
	case "initialize":
		WriteResult(w, req.ID, map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": s.name, "version": "1.0.0"}, "instructions": s.instructionsFor(access)})
	case "notifications/initialized":
		w.WriteHeader(http.StatusNoContent)
	case "tools/list":
		tools := []map[string]any{}
		if s.enabled.Load() && s.ThreadEnabled(threadID) {
			if available := s.tools(access); available != nil {
				tools = available
			}
		}
		WriteResult(w, req.ID, map[string]any{"tools": tools})
	case "tools/call":
		if !s.enabled.Load() || !s.ThreadEnabled(threadID) {
			WriteToolError(w, req.ID, s.disabledError())
			return
		}
		s.serveCall(w, r, req, access)
	default:
		WriteError(w, req.ID, http.StatusOK, -32601, "method not found")
	}
}

func (s *Server[T]) instructionsFor(access T) string {
	if s.instructionsFunc != nil {
		return s.instructionsFunc(access)
	}
	return s.instructions
}

func (s *Server[T]) disabledError() error {
	if s.disabledErr != nil {
		return s.disabledErr
	}
	return fmt.Errorf("MCP tools are disabled")
}

// callKeepaliveInterval paces the comment lines a streamed call emits while
// its handler runs. Claude Code's HTTP client abandons a call whose response
// has not started after six minutes, whatever timeouts its configuration
// names (verified 2026-09-19, claude 2.1.261, with the server `timeout`,
// MCP_TOOL_TIMEOUT and CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT all raised); a
// response that is already streaming with a comment every fifteen seconds
// survives the full call ceiling. Codex reads the same stream.
var callKeepaliveInterval = 15 * time.Second

// serveCall runs the tool handler. A client that accepts text/event-stream
// (both provider CLIs do) gets the response as one event stream: headers and
// a keepalive comment before the handler starts, a comment every
// callKeepaliveInterval while it runs, and the JSON-RPC response the handler
// wrote as the final message event. Any other client gets the handler's JSON
// body unchanged.
//
// Work the handler deferred with AfterResponse runs here, after the last
// byte of the response has been written and flushed. On the streamed path
// the handler's body reaches the socket in finish and not before, so a
// handler that ran its own after-response work inline would run it while
// its answer was still in a buffer.
func (s *Server[T]) serveCall(w http.ResponseWriter, r *http.Request, req Request, access T) {
	ctx, hook := withAfterResponse(r.Context())
	flusher, ok := w.(http.Flusher)
	if !ok || !acceptsEventStream(r.Header.Get("Accept")) {
		tracked := &trackedWriter{ResponseWriter: w}
		s.call(tracked, ctx, req, access)
		if ok {
			flusher.Flush()
		}
		hook.run(tracked.err == nil && r.Context().Err() == nil)
		return
	}
	stream := newCallStream(w, flusher, r.Context())
	s.call(stream, ctx, req, access)
	err := stream.finish(req.ID)
	hook.run(err == nil && r.Context().Err() == nil)
}

// trackedWriter remembers whether the response actually went out. net/http
// buffers, so a write error is the only thing this side can observe about a
// client that is no longer there.
type trackedWriter struct {
	http.ResponseWriter
	err error
}

func (t *trackedWriter) Write(p []byte) (int, error) {
	n, err := t.ResponseWriter.Write(p)
	if err != nil && t.err == nil {
		t.err = err
	}
	return n, err
}

// afterResponse collects the work one call deferred until its response has
// been written.
type afterResponse struct {
	mu  sync.Mutex
	fns []func(bool)
}

type afterResponseKey struct{}

// withAfterResponse arms a call context to collect after-response work.
func withAfterResponse(ctx context.Context) (context.Context, *afterResponse) {
	hook := &afterResponse{}
	return context.WithValue(ctx, afterResponseKey{}, hook), hook
}

// AfterResponse registers work to run once this call's response has left the
// transport, in registration order.
//
// The hook is told whether the response was delivered: false means the final
// write failed or the client was already gone, so nothing may be recorded as
// read by a model that never received it. Outside a call, where there is no
// response to wait for, the work runs immediately and reports delivered,
// because deferring it would drop it.
func AfterResponse(ctx context.Context, fn func(delivered bool)) {
	if hook, ok := ctx.Value(afterResponseKey{}).(*afterResponse); ok {
		hook.add(fn)
		return
	}
	fn(true)
}

func (a *afterResponse) add(fn func(bool)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fns = append(a.fns, fn)
}

func (a *afterResponse) run(delivered bool) {
	a.mu.Lock()
	fns := a.fns
	a.fns = nil
	a.mu.Unlock()
	for _, fn := range fns {
		fn(delivered)
	}
}

// acceptsEventStream reports whether an Accept header lists
// text/event-stream. Both provider clients send it beside application/json;
// a wildcard alone does not opt in, so a plain JSON client keeps JSON.
func acceptsEventStream(accept string) bool {
	for _, item := range strings.Split(accept, ",") {
		mediaType, _, _ := strings.Cut(item, ";")
		if strings.EqualFold(strings.TrimSpace(mediaType), "text/event-stream") {
			return true
		}
	}
	return false
}

// callStream is the ResponseWriter a streamed tools/call handler writes to.
// The handler's status and headers are irrelevant once the stream has
// started (every handler answers 200 with a JSON-RPC body); its body is
// buffered and sent as the final event. Writes to the underlying connection
// are serialized so a keepalive never lands inside the final event.
type callStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
	header  http.Header
	body    bytes.Buffer
	mu      sync.Mutex
	stop    chan struct{}
	done    chan struct{}
}

func newCallStream(w http.ResponseWriter, flusher http.Flusher, ctx context.Context) *callStream {
	c := &callStream{w: w, flusher: flusher, header: make(http.Header), stop: make(chan struct{}), done: make(chan struct{})}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	c.keepalive()
	go c.run(ctx)
	return c
}

func (c *callStream) run(ctx context.Context) {
	defer close(c.done)
	ticker := time.NewTicker(callKeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ctx.Done():
			// The client is gone; the handler still runs to completion
			// under its own context and finish writes into the void.
			return
		case <-ticker.C:
			c.keepalive()
		}
	}
}

func (c *callStream) keepalive() {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = io.WriteString(c.w, ": keepalive\n\n")
	c.flusher.Flush()
}

func (c *callStream) Header() http.Header { return c.header }

func (c *callStream) WriteHeader(int) {}

func (c *callStream) Write(p []byte) (int, error) { return c.body.Write(p) }

// finish stops the keepalives and sends the buffered response as the final
// event. A handler that wrote nothing would leave the client waiting for a
// reply that never comes, so that becomes a JSON-RPC error instead.
//
// It reports the first write failure, which is what tells the caller the
// response never left this computer.
func (c *callStream) finish(id json.RawMessage) error {
	close(c.stop)
	<-c.done
	body := bytes.TrimSpace(c.body.Bytes())
	if len(body) == 0 {
		body, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": "tool produced no response"}})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var failure error
	send := func(_ int, err error) {
		if err != nil && failure == nil {
			failure = err
		}
	}
	send(io.WriteString(c.w, "event: message\n"))
	for _, line := range bytes.Split(body, []byte("\n")) {
		send(io.WriteString(c.w, "data: "))
		send(c.w.Write(line))
		send(io.WriteString(c.w, "\n"))
	}
	send(io.WriteString(c.w, "\n"))
	c.flusher.Flush()
	return failure
}

// validMCPRequest applies the request checks every request clears before
// any method dispatch — initialize, notifications, tools/list and
// tools/call alike — and writes the refusal itself when one fails.
//
// The only client of this endpoint is a provider CLI
// this app spawned, which pins what a genuine request looks like: it
// arrives from a loopback peer, carries no Origin, and declares JSON.
// Both real clients match (verified 2026-08-30): Claude Code's HTTP
// transport and the Codex app-server's rmcp adapter each set
// `content-type: application/json` on every POST and neither sets Origin
// — Codex goes further and rejects a user-configured Origin header
// outright (codex-rs/rmcp-client/src/http_headers.rs).
//
// The per-thread UUID in the path is the only other credential, and it
// rides provider argv, so it is readable by any process of the same
// user. Same-user is already the trust boundary; these checks are what
// keeps a document in a browser — which is not the same user's
// process — from reaching the endpoint.
func validMCPRequest(w http.ResponseWriter, r *http.Request) bool {
	// Peer verification off the accepting socket, matching the claudetui
	// gateway's check (isLoopback, internal/provider/claudetui/hookrelay.go
	// — that copy also accepts the literal "localhost", which an accepted
	// connection's RemoteAddr never carries). Go fills RemoteAddr from the
	// accepted socket, so a request header cannot set it.
	if !loopback.PeerAddress(r.RemoteAddr) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	// A local process sends no Origin. A document always sends one on a
	// POST, cross-origin or same-origin, so refusing the header refuses
	// the page without touching the provider CLI.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	// Requiring JSON before the body is decoded is more than hygiene. A
	// POST declaring text/plain is a CORS simple request: it is sent with
	// no preflight, so a page could invoke a tool the browser never asked
	// permission for — it could not read the reply, but the page
	// evaluation or workspace file read would already have run. JSON is
	// not a simple content type, so the browser must preflight first, and
	// the method check in handle refuses that preflight.
	if !jsonContentType(r.Header.Get("Content-Type")) {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

// jsonContentType reports whether a Content-Type header declares JSON.
// Parameters are allowed: both provider clients send the bare type, but
// a charset is legal and some MCP clients attach one.
func jsonContentType(value string) bool {
	mediaType, _, _ := strings.Cut(value, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "application/json")
}

func (s *Server[T]) accessForPath(path string) (string, T, bool) {
	var zero T
	token, ok := strings.CutPrefix(path, "/mcp/")
	if !ok || token == "" || strings.Contains(token, "/") {
		return "", zero, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	access, ok := s.tokenToAccess[token]
	return s.tokenToThread[token], access, ok
}

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type ToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// MCP envelopes may carry client metadata and protocol extensions. Only the
// tool's arguments use the closed schema enforced by DecodeArgs.
func DecodeToolCall(raw json.RawMessage) (ToolCall, error) {
	var call ToolCall
	if err := json.Unmarshal(raw, &call); err != nil || call.Name == "" {
		return ToolCall{}, fmt.Errorf("invalid tools/call params")
	}
	return call, nil
}

func DecodeArgs(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return fmt.Errorf("Tool arguments must be a JSON object with the fields listed in the tool schema.")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var mismatch *json.UnmarshalTypeError
		if errors.As(err, &mismatch) {
			kind := mismatch.Type.Kind()
			expected := kind.String()
			switch kind {
			case reflect.Int, reflect.Int64:
				expected = "an integer"
			case reflect.Float64:
				expected = "a number"
			case reflect.String:
				expected = "a string"
			case reflect.Bool:
				expected = "a boolean"
			case reflect.Slice, reflect.Array:
				expected = "an array"
			case reflect.Struct, reflect.Map:
				expected = "an object"
			}
			return fmt.Errorf("Argument %q must be %s. Check the tool schema.", mismatch.Field, expected)
		}
		if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			if len(field) > 128 {
				field = field[:128] + "…"
			}
			return fmt.Errorf("Unknown argument %s. Use only fields listed in the tool schema.", field)
		}
		return fmt.Errorf("Invalid argument JSON. Supply one object matching the tool schema.")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return fmt.Errorf("Tool arguments contain extra JSON. Supply exactly one object.")
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	}
	return fmt.Errorf("extra JSON value")
}

func WriteResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeRPC(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
func WriteError(w http.ResponseWriter, id json.RawMessage, status, code int, message string) {
	writeRPC(w, status, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}
func writeRPC(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func WriteToolError(w http.ResponseWriter, id json.RawMessage, err error) {
	message := err.Error()
	if code, safe, ok := errorsx.PublicDetails(err); ok {
		message = "[" + code + "] " + safe
	}
	WriteResult(w, id, map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": message}}})
}
func WriteToolJSON(w http.ResponseWriter, id json.RawMessage, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		WriteToolError(w, id, err)
		return
	}
	WriteResult(w, id, map[string]any{"content": []map[string]any{{"type": "text", "text": string(data)}}})
}
