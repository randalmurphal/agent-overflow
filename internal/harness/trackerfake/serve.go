package trackerfake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"runtime/debug"
	"strconv"
)

// service is one tracker's side of the request pipeline. Every method is
// called with the engine's lock held.
type service interface {
	// authenticate checks the credential x presents and sets x.tenant;
	// a refused credential is answered as the tracker answers it.
	authenticate(e *Engine, x *exchange) *answer
	// route matches x to a route of x.tenant; a request it does not
	// implement is a match with unhandled set.
	route(e *Engine, x *exchange) match
	// rateLimited is the tracker's answer to a request over its limit.
	rateLimited(e *Engine, x *exchange, retryAfter int) answer
	// unhandledAnswer carries detail in the tracker's error shape.
	unhandledAnswer(detail string) answer
	// routeNames are every route the tracker implements, for faults.
	routeNames() []string
}

func serviceFor(o Origin) service {
	switch o.Tracker {
	case TrackerJira:
		return jiraService{site: o.Site}
	case TrackerTempo:
		return tempoService{}
	case TrackerLinear:
		return linearService{}
	}
	panic(fmt.Sprintf("trackerfake: no service for tracker %q; Origin.check refuses it", o.Tracker))
}

// exchange is one request on its way through the pipeline.
type exchange struct {
	origin Origin
	r      *http.Request
	body   []byte
	// tenant is the Jira site or Linear workspace the credential names.
	tenant string
	// secrets are the credentials the request presented, scrubbed from
	// what is logged.
	secrets []string
	// gen is the engine generation the request arrived in.
	gen uint64
	// undo puts back what the running handler changed; nil when no
	// handler is running.
	undo func()
	// entry is the request's log entry as the pipeline fills it in;
	// logEntry makes what the log keeps of it.
	entry Request
}

// rollback puts back what the running handler changed, if one is running.
func (x *exchange) rollback() {
	if x.undo != nil {
		x.undo()
		x.undo = nil
	}
}

// match is a route and its handler, or why the request is unhandled.
type match struct {
	name      string
	run       func() answer
	unhandled string
}

// answer is what a request gets: a status, headers and a body; or no
// answer at all (abort), the connection closed.
type answer struct {
	status    int
	header    http.Header
	body      []byte
	unhandled string
	abort     bool
}

// jsonAnswer is status with v encoded as JSON.
func jsonAnswer(status int, v any) answer {
	body, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("trackerfake: encode an answer: %v", err))
	}
	return answer{status: status, header: http.Header{"Content-Type": {"application/json;charset=UTF-8"}}, body: body}
}

// emptyAnswer is status with no body.
func emptyAnswer(status int) answer { return answer{status: status} }

// unhandledf is a handler's answer to a request it does not implement.
func unhandledf(format string, args ...any) answer {
	return answer{unhandled: fmt.Sprintf(format, args...)}
}

// serve runs one request through the pipeline: offline, credential,
// route, fault, handler, log, answer. A held request is logged when it
// arrives and again when it is answered.
func (e *Engine) serve(o Origin, svc service, w http.ResponseWriter, r *http.Request) {
	x := &exchange{origin: o, r: r, entry: Request{Origin: o, Method: r.Method, Path: r.URL.Path}}
	if q := r.URL.Query(); len(q) > 0 {
		x.entry.Query = q
	}
	body, readErr := readBody(r)
	x.body = body
	x.recordBody()

	var a answer
	var held *hold
	var entry Request
	e.locked(func() {
		x.gen = e.gen
		a, held = e.guarded(svc, x, func() (answer, *hold) { return e.decide(svc, x, readErr) })
		entry = e.record(x, a, held != nil)
	})
	e.notify(entry)
	if held != nil {
		select {
		case <-held.done:
		case <-r.Context().Done():
		}
		e.locked(func() {
			a, _ = e.guarded(svc, x, func() (answer, *hold) { return e.resume(svc, x, held), nil })
			entry = e.complete(entry, x, a)
		})
		e.notify(entry)
	}
	write(w, a)
}

// locked runs f under mu: the one place the pipeline takes the lock,
// released by defer whatever f does.
func (e *Engine) locked(f func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f()
}

// guarded runs f, the part of the pipeline that runs tracker code. A
// panic there is the fake's own defect: what the handler changed is
// rolled back, the request is answered unhandled with the panic and its
// stack, and the engine keeps serving. Callers hold mu.
func (e *Engine) guarded(svc service, x *exchange, f func() (answer, *hold)) (a answer, h *hold) {
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		x.rollback()
		a, h = e.unhandled(svc, x, fmt.Sprintf("trackerfake panicked: %v\n%s", p, debug.Stack())), nil
	}()
	return f()
}

// decide answers x, or names the hold it waits on. Callers hold mu.
func (e *Engine) decide(svc service, x *exchange, readErr error) (answer, *hold) {
	switch {
	case e.closed:
		x.entry.Route = "closed"
		return answer{abort: true}, nil
	case e.faults.offline[x.origin]:
		x.entry.Route, x.entry.Fault = "offline", FaultOffline
		x.entry.Detail = "the origin is offline: connection closed"
		return answer{abort: true}, nil
	case readErr != nil:
		return e.unhandled(svc, x, readErr.Error()), nil
	}
	return e.dispatch(svc, x, true)
}

// resume answers a request its hold let go. It is dropped, its
// connection closed with nothing applied, when the hold dropped it, its
// client left, or the world it arrived in is gone: the engine closed, or
// a Reset or a Seed that replaced a tenant started a new generation.
// Otherwise it is answered against the state as it is now, with no fault
// applied again. Callers hold mu.
func (e *Engine) resume(svc service, x *exchange, h *hold) answer {
	switch {
	case h.drop || x.r.Context().Err() != nil:
		x.entry.Detail = "the hold ended with the connection closed and nothing applied"
		return answer{abort: true}
	case e.closed || e.gen != x.gen:
		x.entry.Detail = "the engine was reset, reseeded or closed while the request was held: connection closed, nothing applied"
		return answer{abort: true}
	}
	x.entry.Route, x.entry.Detail = "", ""
	a, _ := e.dispatch(svc, x, false)
	return a
}

// dispatch checks the credential, matches the route and applies the
// first matching fault (when faults is set) or the handler. Callers hold
// mu.
func (e *Engine) dispatch(svc service, x *exchange, faults bool) (answer, *hold) {
	if refused := svc.authenticate(e, x); refused != nil {
		x.entry.Route = "unauthorized"
		return *refused, nil
	}
	m := svc.route(e, x)
	if m.unhandled != "" {
		return e.unhandled(svc, x, m.unhandled), nil
	}
	x.entry.Route = m.name
	f := (*armedFault)(nil)
	if faults {
		f = e.faults.match(x.origin, m.name)
	}
	if f == nil {
		return e.run(svc, x, m), nil
	}
	x.entry.Fault = f.Kind
	switch f.Kind {
	case FaultRateLimit:
		return svc.rateLimited(e, x, f.RetryAfter), nil
	case FaultRefuse:
		return refusalAnswer(f.Fault), nil
	case FaultHold:
		return answer{}, f.hold
	case FaultFailAfterApply:
		a := e.run(svc, x, m)
		if a.unhandled != "" {
			return a, nil
		}
		x.entry.Detail = fmt.Sprintf("applied and answered %d; the answer was dropped", a.status)
		return answer{abort: true}, nil
	case FaultOffline:
		// Fault.check refuses to arm it.
	}
	return e.unhandled(svc, x, fmt.Sprintf("trackerfake: an armed fault of kind %q", f.Kind)), nil
}

// run runs a route's handler in the transaction txn.go describes: an
// unhandled answer or a refusal (status 400 or more) is rolled back, as
// guarded rolls back a panic. Callers hold mu.
func (e *Engine) run(svc service, x *exchange, m match) answer {
	undo, err := e.save(x)
	if err != nil {
		return e.unhandled(svc, x, err.Error())
	}
	x.undo = undo
	a := m.run()
	if a.unhandled != "" || a.status >= http.StatusBadRequest {
		x.rollback()
	}
	x.undo = nil
	if a.unhandled != "" {
		return e.unhandled(svc, x, a.unhandled)
	}
	return a
}

// unhandled is the tracker's 501 for reason, with every credential in
// the reason replaced, as the log replaces them. Callers hold mu.
func (e *Engine) unhandled(svc service, x *exchange, reason string) answer {
	reason = scrubText(reason, e.secrets(x))
	a := svc.unhandledAnswer(reason)
	a.unhandled = reason
	return a
}

func refusalAnswer(f Fault) answer {
	a := answer{status: f.Status, header: http.Header{}, body: bytes.Clone(f.Body)}
	for k, v := range f.Header {
		a.header.Set(k, v)
	}
	if len(a.body) > 0 {
		a.header.Set("Content-Type", "application/json")
	}
	return a
}

// record logs x as answered by a, or as held. Callers hold mu.
func (e *Engine) record(x *exchange, a answer, held bool) Request {
	x.entry.At = e.now()
	x.entry.Held = held
	if held {
		x.entry.Fault = FaultHold
	}
	e.settle(x, a)
	return e.log.add(e.logEntry(x))
}

// complete replaces a held request's entry with how it was answered.
// Callers hold mu.
func (e *Engine) complete(entry Request, x *exchange, a answer) Request {
	x.entry.Held = false
	x.entry.Fault = FaultHold
	e.settle(x, a)
	out := e.logEntry(x)
	out.Seq, out.At = entry.Seq, entry.At
	e.log.update(out)
	return out
}

// settle sets the entry's tenant, status and unhandled detail from a.
func (e *Engine) settle(x *exchange, a answer) {
	x.entry.Tenant = x.tenant
	x.entry.Status = 0
	if !a.abort && !x.entry.Held {
		x.entry.Status = a.status
		if x.entry.Status == 0 {
			x.entry.Status = http.StatusOK
		}
	}
	x.entry.Unhandled = a.unhandled != ""
	if x.entry.Unhandled {
		x.entry.Route = ""
		x.entry.Detail = unhandledDetail(x, a.unhandled)
	}
}

func unhandledDetail(x *exchange, reason string) string {
	return fmt.Sprintf("trackerfake: unhandled %s request %s %s: %s; add it in internal/harness/trackerfake (see its AGENTS.md)",
		x.origin.Tracker, x.r.Method, x.r.URL.Path, reason)
}

// notify hands entry to OnRequest; callers do not hold mu.
func (e *Engine) notify(entry Request) {
	if e.opts.OnRequest != nil {
		e.opts.OnRequest(entry.clone())
	}
}

// recordBody puts a JSON body, or a multipart upload's file names, in
// the entry; logEntry bounds both.
func (x *exchange) recordBody() {
	if len(x.body) == 0 {
		return
	}
	mediaType, params, _ := mime.ParseMediaType(x.r.Header.Get("Content-Type"))
	switch {
	case mediaType == "multipart/form-data":
		x.entry.Files = multipartNames(x.body, params["boundary"])
	case json.Valid(x.body):
		x.entry.Body = json.RawMessage(bytes.TrimSpace(x.body))
	}
}

// write sends a, or closes the connection when a is no answer.
func write(w http.ResponseWriter, a answer) {
	if a.abort {
		panic(http.ErrAbortHandler)
	}
	for k, vs := range a.header {
		w.Header()[k] = vs
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if len(a.body) > 0 {
		w.Header().Set("Content-Length", strconv.Itoa(len(a.body)))
	}
	status := a.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(a.body)
}
