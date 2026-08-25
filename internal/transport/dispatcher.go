package transport

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"reflect"
	"sort"
	"sync"
)

// Dispatcher owns the registered RPC method set. The set is built once
// at startup by walking a receiver's exported methods via reflect, with
// optional skip-list filtering so methods marked //wails:ignore in source
// can stay unreachable at the wire level.
//
// Method IDs are FNV-1a 32-bit hashes of "main.App.<MethodName>". This
// matches the algorithm Wails uses internally (see pkg/application/
// bindings.go and internal/hash/fnv.go in the Wails source) so generated
// bindings calling $Call.ByID(<num>, ...args) route correctly through
// the same numeric ID without translation.
type Dispatcher struct {
	mu     sync.RWMutex
	byID   map[uint32]*Method
	byName map[string]*Method
}

// Method is the descriptor for a single registered RPC method. The
// receiver and reflect.Value are kept live so Invoke can re-enter the
// receiver without re-reflecting on each call.
type Method struct {
	Name         string
	FQN          string
	ID           uint32
	NeedsContext bool
	IsVariadic   bool
	// LocalOnly refuses the method for non-loopback peers regardless of
	// the LocalOnlyMethods name set. Set via RegisterOptions.LocalOnly.
	LocalOnly   bool
	fn          reflect.Value
	inputTypes  []reflect.Type
	outputTypes []reflect.Type
	// hasError records whether the last return value implements error.
	// Static per signature — precomputed at Register like NeedsContext /
	// IsVariadic so processResults doesn't re-derive it per invocation.
	hasError bool
}

// RegisterOptions controls Register's filtering behavior.
type RegisterOptions struct {
	// Package is the Go package path used to compute each method's FQN
	// (e.g. "main"). Empty defaults to "main" because the production App
	// lives in package main.
	Package string

	// TypeName overrides the receiver type name used in the FQN. Empty
	// means "use the receiver's reflect.Type.Name()".
	TypeName string

	// Skip excludes methods by name. Used to honor //wails:ignore — the
	// Wails generator doesn't emit bindings for ignored methods, so the
	// runtime dispatcher must also refuse to expose them.
	Skip map[string]bool

	// AllowList, when non-nil, restricts registration to the listed
	// names. Used by methodgen to lock the dispatcher to exactly the
	// methods the binding generator emitted.
	AllowList map[string]bool

	// LocalOnly marks every method on this receiver as loopback-only,
	// independent of the name-keyed LocalOnlyMethods set (which is
	// integrity-tested against the generated App method list and so can
	// only hold App methods). The harness receiver registers with this
	// set: its entire surface is state injection and must never be
	// reachable from a non-loopback peer.
	LocalOnly bool
}

var ctxType = reflect.TypeOf((*context.Context)(nil)).Elem()
var errType = reflect.TypeOf((*error)(nil)).Elem()

// NewDispatcher returns an empty dispatcher. Call Register to populate
// it from a receiver before serving traffic.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		byID:   make(map[uint32]*Method),
		byName: make(map[string]*Method),
	}
}

// Register walks every exported method on receiver and adds it to the
// dispatcher, computing the FNV-1a ID against "<package>.<typeName>.<methodName>".
// Methods listed in opts.Skip or in the framework-internal exclusion set
// are filtered out; if opts.AllowList is non-nil, methods outside the
// allow list are also filtered.
//
// Returns the slice of methods actually registered, in registration
// order. Useful for tests that want to assert on the visible surface.
func (d *Dispatcher) Register(receiver any, opts RegisterOptions) ([]*Method, error) {
	if receiver == nil {
		return nil, fmt.Errorf("transport: nil receiver")
	}
	rv := reflect.ValueOf(receiver)
	rt := rv.Type()
	if rt.Kind() != reflect.Ptr {
		return nil, fmt.Errorf("transport: receiver must be a pointer, got %s", rt.Kind())
	}

	pkg := opts.Package
	if pkg == "" {
		pkg = "main"
	}
	typeName := opts.TypeName
	if typeName == "" {
		typeName = rt.Elem().Name()
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	registered := make([]*Method, 0, rt.NumMethod())
	for i := range rt.NumMethod() {
		methodInfo := rt.Method(i)
		name := methodInfo.Name
		if InternalServiceMethods[name] {
			continue
		}
		if opts.Skip != nil && opts.Skip[name] {
			continue
		}
		if opts.AllowList != nil && !opts.AllowList[name] {
			continue
		}

		fqn := fmt.Sprintf("%s.%s.%s", pkg, typeName, name)
		methodValue := rv.Method(i)
		methodType := methodValue.Type()

		inputTypes := make([]reflect.Type, methodType.NumIn())
		needsCtx := false
		for j := range methodType.NumIn() {
			inputTypes[j] = methodType.In(j)
			if j == 0 && inputTypes[j].AssignableTo(ctxType) {
				needsCtx = true
			}
		}
		outputTypes := make([]reflect.Type, methodType.NumOut())
		for j := range methodType.NumOut() {
			outputTypes[j] = methodType.Out(j)
		}

		m := &Method{
			Name:         name,
			FQN:          fqn,
			ID:           fnvHash(fqn),
			NeedsContext: needsCtx,
			IsVariadic:   methodType.IsVariadic(),
			LocalOnly:    opts.LocalOnly,
			fn:           methodValue,
			inputTypes:   inputTypes,
			outputTypes:  outputTypes,
			hasError:     len(outputTypes) > 0 && outputTypes[len(outputTypes)-1].Implements(errType),
		}

		if existing, ok := d.byID[m.ID]; ok {
			return nil, fmt.Errorf("transport: hash collision between %s and %s on id %d",
				existing.FQN, m.FQN, m.ID)
		}
		// Name-based dispatch shares ONE namespace across every
		// registered receiver (Resolve falls back to name when the
		// frame carries no ID), so a duplicate name would silently
		// shadow the earlier receiver's method. Refuse it the same way
		// an ID collision is refused — see AGENTS.md § "Additional
		// receivers": a second receiver must use a distinctive prefix.
		if existing, ok := d.byName[name]; ok {
			return nil, fmt.Errorf("transport: name collision between %s and %s on name %q",
				existing.FQN, m.FQN, name)
		}
		d.byID[m.ID] = m
		d.byName[name] = m
		registered = append(registered, m)
	}
	return registered, nil
}

// fnvHash matches Wails' internal/hash.Fnv: FNV-1a 32-bit over the FQN
// string. Verified against the generated frontend bindings:
// fnvHash("main.App.ArchiveProject") == 1352159878.
func fnvHash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// newCorrelationID returns a short opaque token used to pair a
// wire-visible "method failed (id: <id>)" message with the full error
// text logged server-side. 8 bytes -> 64 bits of randomness, base64url
// encoded to 11 chars. Long enough to make collision-with-an-old-log
// effectively impossible inside any reasonable retention window, short
// enough that a user can read it off a toast and grep their logs.
//
// On rand.Read failure (essentially impossible — only happens if the
// kernel RNG is wedged) the ID falls back to a literal "noid" sentinel
// so the wire path still produces a well-formed message.
func newCorrelationID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "noid"
	}
	return base64.RawURLEncoding.EncodeToString(buf[:])
}

// LookupID resolves a method by FNV-1a numeric ID. Returns (m, true)
// on hit, (nil, false) on miss.
func (d *Dispatcher) LookupID(id uint32) (*Method, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	m, ok := d.byID[id]
	return m, ok
}

// LookupName resolves a method by exported name. Returns (m, true)
// on hit, (nil, false) on miss.
func (d *Dispatcher) LookupName(name string) (*Method, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	m, ok := d.byName[name]
	return m, ok
}

// Resolve is the dispatcher's entry point used by the connection
// handler: prefer ID, fall back to name, return a wire-ready
// FrameError when neither resolves. Wire message is generic to avoid
// leaking which IDs/names were probed.
//
// Equivalent to ResolveForOrigin(id, name, true) — i.e. assumes the
// caller is loopback. Connection handlers that know whether the peer
// is loopback should call ResolveForOrigin directly so LocalOnlyMethods
// gets enforced.
func (d *Dispatcher) Resolve(id uint32, name string) (*Method, *FrameError) {
	return d.ResolveForOrigin(id, name, true)
}

// ResolveForOrigin is the origin-aware variant of Resolve. When
// isLoopback is false and the resolved method appears in
// LocalOnlyMethods, the dispatcher refuses with ErrCodeMethodNotFound —
// indistinguishable from a probe of an unregistered method, so a LAN
// scanner can't fingerprint which methods are privileged.
//
// The check happens AFTER lookup so a non-loopback peer probing for a
// privileged method gets the same shape of error whether the method
// exists or not. Without that, an attacker could enumerate the
// privileged surface by comparing response codes.
func (d *Dispatcher) ResolveForOrigin(id uint32, name string, isLoopback bool) (*Method, *FrameError) {
	var method *Method
	if id != 0 {
		if m, ok := d.LookupID(id); ok {
			method = m
		}
	}
	if method == nil && name != "" {
		if m, ok := d.LookupName(name); ok {
			method = m
		}
	}
	if method == nil {
		return nil, &FrameError{
			Code:    ErrCodeMethodNotFound,
			Message: "method not registered",
		}
	}
	if !isLoopback && (LocalOnlyMethods[method.Name] || method.LocalOnly) {
		// Wire shape matches an unrelated method-not-found rather than a
		// distinct "forbidden" code. A probing client can't tell whether
		// the method exists and is privileged, or simply doesn't exist —
		// keeping the privileged surface unenumerable from the LAN.
		//
		// Server-side log gives the operator visibility: when a LAN
		// peer probes a privileged method the wire shape stays
		// indistinguishable from any other miss, but the log records
		// the actual method name so an admin can recognise probing.
		log.Printf("transport: refused %s for non-loopback peer (LAN-only method)", method.Name)
		return nil, &FrameError{
			Code:    ErrCodeMethodNotFound,
			Message: "method not registered",
		}
	}
	return method, nil
}

// Invoke unmarshals params, calls the method, and returns the JSON-
// serialised result. The error return distinguishes:
//
//   - ErrCodeMethodNotFound: caller used the wrong ID/name (handled by Resolve).
//   - ErrCodeBadParams:      wire input failed to decode for a declared type.
//   - ErrCodeMethodError:    the method itself returned a non-nil error.
//   - ErrCodeTemporarilyUnavailable: the method hit a retryable deadline.
//   - ErrCodeInternal:       reflection panicked or marshaling failed.
//
// Wire messages for ErrCodeMethodError and ErrCodeInternal are
// deliberately generic — full prose (file paths, internal state, panic
// details) is logged server-side. A LAN-attached attacker can probe the
// wire shape but cannot harvest project-internal strings.
// Invoke is the legacy entry point. It keeps the original redaction
// semantics (method-returned error text NOT exposed) so the existing
// info-disclosure regression tests stay representative of LAN-peer
// behaviour. Production code on the connection path uses
// InvokeForOrigin directly with the per-conn loopback flag.
func (d *Dispatcher) Invoke(ctx context.Context, m *Method, params []json.RawMessage) (result json.RawMessage, frameErr *FrameError) {
	return d.InvokeForOrigin(ctx, m, params, false)
}

// InvokeForOrigin runs the dispatch with the caller's origin known.
// `isLoopback` is the per-connection loopback flag and drives error
// exposure: a loopback peer is the same machine as the backend, so
// leaking a method-returned error string adds no information beyond
// what the server-side log already contains, while a LAN peer must
// continue to see the redacted "method failed (id: <cid>)" envelope.
func (d *Dispatcher) InvokeForOrigin(ctx context.Context, m *Method, params []json.RawMessage, isLoopback bool) (result json.RawMessage, frameErr *FrameError) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("transport: panic in %s: %v", m.FQN, r)
			result = nil
			frameErr = &FrameError{
				Code:    ErrCodeInternal,
				Message: "internal error",
			}
		}
	}()

	if len(params) > MaxRPCParams {
		return nil, &FrameError{
			Code:    ErrCodeBadParams,
			Message: "too many parameters",
		}
	}

	args, fe := d.buildArgs(ctx, m, params)
	if fe != nil {
		return nil, fe
	}

	// Variadic methods need CallSlice so the trailing slice argument
	// is treated as the variadic slot in one piece, not iterated. Plain
	// Call would expect each variadic element as a separate Value.
	var results []reflect.Value
	if m.IsVariadic {
		results = m.fn.CallSlice(args)
	} else {
		results = m.fn.Call(args)
	}
	return d.processResults(m, results, isLoopback)
}

// buildArgs decodes the json-encoded params array into reflect.Values
// suitable for method.Call. When the method's first parameter is a
// context.Context, the dispatcher's ctx is injected and parameter
// indexing on the wire stays zero-based — i.e. the wire never sees the
// ctx slot.
func (d *Dispatcher) buildArgs(ctx context.Context, m *Method, params []json.RawMessage) ([]reflect.Value, *FrameError) {
	expectedParams := len(m.inputTypes)
	if m.NeedsContext {
		expectedParams--
	}

	if m.IsVariadic {
		// Variadic arity: the wire MAY send fewer params than declared
		// (the variadic slot may be empty). A larger arity is also OK —
		// extra params get folded into the variadic slice in the
		// existing param assembly logic below.
		minParams := expectedParams - 1
		if len(params) < minParams {
			return nil, &FrameError{
				Code:    ErrCodeBadParams,
				Message: fmt.Sprintf("%s: variadic expects at least %d params, got %d", m.FQN, minParams, len(params)),
			}
		}
	} else if len(params) != expectedParams {
		return nil, &FrameError{
			Code:    ErrCodeBadParams,
			Message: fmt.Sprintf("%s: expected %d params, got %d", m.FQN, expectedParams, len(params)),
		}
	}

	args := make([]reflect.Value, 0, len(m.inputTypes))
	wireIdx := 0
	for declIdx, declType := range m.inputTypes {
		if declIdx == 0 && m.NeedsContext {
			args = append(args, reflect.ValueOf(ctx))
			continue
		}

		// Variadic slot: collect every remaining wire param as one
		// element each of the slice. We use reflect.Append into a fresh
		// slice of the variadic element type rather than allocating a
		// []json.RawMessage and decoding once because the element type
		// might not be a primitive (e.g. a struct). reflect.AppendSlice
		// handles arbitrary element kinds.
		if m.IsVariadic && declIdx == len(m.inputTypes)-1 {
			elemType := declType.Elem()
			slice := reflect.MakeSlice(declType, 0, len(params)-wireIdx)
			for ; wireIdx < len(params); wireIdx++ {
				ptr := reflect.New(elemType)
				if err := json.Unmarshal(params[wireIdx], ptr.Interface()); err != nil {
					log.Printf("transport: %s param %d (variadic): %v", m.FQN, wireIdx, err)
					return nil, &FrameError{
						Code:    ErrCodeBadParams,
						Message: fmt.Sprintf("bad parameter %d", wireIdx),
					}
				}
				slice = reflect.Append(slice, ptr.Elem())
			}
			args = append(args, slice)
			continue
		}

		ptr := reflect.New(declType)
		if err := json.Unmarshal(params[wireIdx], ptr.Interface()); err != nil {
			log.Printf("transport: %s param %d: %v", m.FQN, wireIdx, err)
			return nil, &FrameError{
				Code:    ErrCodeBadParams,
				Message: fmt.Sprintf("bad parameter %d", wireIdx),
			}
		}
		args = append(args, ptr.Elem())
		wireIdx++
	}
	return args, nil
}

// processResults converts the reflect.Value slice into a JSON-encoded
// result. Method signatures fall into four shapes:
//
//   - () : no output, returns null.
//   - (error) : nil error -> null. non-nil error -> Error frame.
//   - (T) : returns T encoded as JSON (rare in practice).
//   - (T, error) : returns T if error nil, else Error frame.
//
// Multi-return signatures (T1, T2) without a trailing error are
// returned as a JSON array. The frontend's generated bindings don't use
// those today; supporting it keeps methodgen unconstrained.
//
// Method-returned errors used to surface methodErr.Error() to the wire
// directly. That leaks filesystem paths and internal state — a method
// that wraps an os.PathError sends "/Users/<name>/secret/path: file not
// found" to a LAN-attached attacker. We now correlate logs with a short
// random ID and return a generic "method failed (id: <id>)" message so
// users can grep server logs for the full prose without surfacing it on
// the wire.
func (d *Dispatcher) processResults(m *Method, results []reflect.Value, exposeErrors bool) (json.RawMessage, *FrameError) {
	if m.hasError {
		errResult := results[len(results)-1]
		if !errResult.IsNil() {
			methodErr := errResult.Interface().(error)
			cid := newCorrelationID()
			log.Printf("transport: %s returned error (id: %s): %v", m.FQN, cid, methodErr)
			message := fmt.Sprintf("method failed (id: %s)", cid)
			if exposeErrors {
				// Loopback caller — same machine as the backend, so the
				// method error text leaks no information that isn't
				// already in the server log. Send it through so the
				// frontend can surface it inline (toast text, dev
				// console) without users having to grep `make dev`
				// output for the cid.
				message = methodErr.Error()
			}
			code := ErrCodeMethodError
			if errors.Is(methodErr, ErrTemporarilyUnavailable) {
				code = ErrCodeTemporarilyUnavailable
			}
			return nil, &FrameError{
				Code:    code,
				Message: message,
			}
		}
		results = results[:len(results)-1]
	}

	switch len(results) {
	case 0:
		return json.RawMessage(`null`), nil
	case 1:
		buf, err := json.Marshal(results[0].Interface())
		if err != nil {
			log.Printf("transport: %s: marshal result: %v", m.FQN, err)
			return nil, &FrameError{
				Code:    ErrCodeInternal,
				Message: "internal error",
			}
		}
		return buf, nil
	default:
		out := make([]any, len(results))
		for i, r := range results {
			out[i] = r.Interface()
		}
		buf, err := json.Marshal(out)
		if err != nil {
			log.Printf("transport: %s: marshal multi-result: %v", m.FQN, err)
			return nil, &FrameError{
				Code:    ErrCodeInternal,
				Message: "internal error",
			}
		}
		return buf, nil
	}
}

// Methods returns a snapshot of registered methods sorted by FQN.
// Stable for tests and for the methodgen integrity gate.
func (d *Dispatcher) Methods() []*Method {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]*Method, 0, len(d.byID))
	for _, m := range d.byID {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FQN < out[j].FQN })
	return out
}
