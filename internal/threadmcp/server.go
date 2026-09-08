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
	tools        func(T) []map[string]any
	call         func(http.ResponseWriter, context.Context, Request, T)
	enabled      atomic.Bool

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
func (s *Server[T]) RegisteredThreadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.threadToToken)
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
		WriteResult(w, req.ID, map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": s.name, "version": "1.0.0"}, "instructions": s.instructions})
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
			WriteToolError(w, req.ID, fmt.Errorf("MCP tools are disabled"))
			return
		}
		s.call(w, r.Context(), req, access)
	default:
		WriteError(w, req.ID, http.StatusOK, -32601, "method not found")
	}
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
