package transport

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

// ScopedRPCPath is the one-shot HTTP RPC route the `ao` CLI speaks. The
// WebSocket wire is built for a long-lived client that also consumes pushed
// events; a CLI process makes one call and exits, so it gets a POST that reuses
// the same dispatcher, the same frame shapes, and the same method table — never
// a second API.
//
// The route accepts SCOPED tokens only. The server's own session token is not
// honoured here, so this surface can never be wider than ScopedTokenMethods
// however it is reached. Loopback only, for the same reason the design file
// route is: the credentials it accepts belong to local provider processes.
const ScopedRPCPath = "/rpc"

// maxScopedRPCBody bounds one CLI request. Scoped calls carry ids, a goal, and
// a seed object — kilobytes. The cap keeps a wedged client from making the
// backend allocate on its behalf.
const maxScopedRPCBody = 1 << 20

const bearerPrefix = "Bearer "

// handleScopedRPC authenticates a scoped token, authorizes the requested method
// against the token's scope, and dispatches it with the scope on the context.
//
// HTTP status carries transport-level outcomes only (bad verb, unreadable body,
// unauthenticated). Everything the dispatcher can answer — including the
// authorization refusals — comes back 200 with a ServerFrame error envelope, so
// the CLI has exactly one place to look for a machine-readable code.
func (s *Server) handleScopedRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// A scoped token belongs to a provider process on this machine. Refusing
	// non-loopback peers outright means a LAN bind never widens this surface,
	// and the refusal is a 404 so the route stays unfingerprintable.
	if !remoteAddrIsLoopback(r.RemoteAddr) {
		http.NotFound(w, r)
		return
	}
	tokens := s.cfg.ScopedTokens
	if tokens == nil {
		http.NotFound(w, r)
		return
	}
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, bearerPrefix) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	scope, ok := tokens.ResolveScopedToken(strings.TrimSpace(strings.TrimPrefix(authorization, bearerPrefix)))
	if !ok {
		// Covers an unknown token and one whose session has stopped: a revoked
		// credential is indistinguishable from a forged one, by design.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var frame ClientFrame
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxScopedRPCBody))
	if err := decoder.Decode(&frame); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if frame.Type != "" && frame.Type != frameTypeRPC {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	writeScopedRPCResult(w, s.invokeScoped(r, scope, frame))
}

// invokeScoped resolves, authorizes, and calls one method. Split out so the
// authorization order is readable in one screen: resolve by name against the
// same dispatcher the WebSocket uses, then filter by scope, then dispatch.
func (s *Server) invokeScoped(r *http.Request, scope CallerScope, frame ClientFrame) ServerFrame {
	response := ServerFrame{Type: frameTypeRPC, ID: frame.ID}
	// Method NAME only. Numeric ids exist so generated bindings can skip a
	// string lookup; a CLI has no generated bindings, and accepting ids here
	// would mean the allow-list had to be keyed twice.
	if frame.Method == "" {
		response.Error = &FrameError{Code: ErrCodeBadParams, Message: "method name is required"}
		return response
	}
	if frameErr := AuthorizeScopedMethod(scope, frame.Method); frameErr != nil {
		if frameErr.Code == ErrCodeMethodNotFound {
			log.Printf("transport: refused %s for a %s scoped token (not a workflow CLI method)", frame.Method, scope.Kind)
		}
		response.Error = frameErr
		return response
	}
	method, frameErr := s.cfg.Dispatcher.ResolveForOrigin(0, frame.Method, true)
	if frameErr != nil {
		response.Error = frameErr
		return response
	}
	ctx := WithCallerScope(r.Context(), scope)
	// isLoopback is true by construction (non-loopback peers were refused
	// above), so the CLI receives the method's real error text — it is the
	// only diagnostic a headless caller has.
	result, frameErr := s.cfg.Dispatcher.InvokeForOrigin(ctx, method, frame.Params, true)
	if frameErr != nil {
		response.Error = frameErr
		return response
	}
	response.Result = result
	return response
}

func writeScopedRPCResult(w http.ResponseWriter, response ServerFrame) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	WriteSecurityHeaders(w.Header())
	if err := json.NewEncoder(w).Encode(response); err != nil {
		// The response is already being written; the log is the only remaining
		// visible error channel at this boundary.
		log.Printf("transport: encode scoped RPC response: %v", err)
	}
}
