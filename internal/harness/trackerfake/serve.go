package trackerfake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
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
	}
	return linearService{}
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
	entry   Request
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

	e.mu.Lock()
	a, held := e.decide(svc, x, readErr)
	entry := e.record(x, a, held != nil)
	e.mu.Unlock()
	e.notify(entry)
	if held != nil {
		select {
		case <-held.done:
		case <-r.Context().Done():
		}
		e.mu.Lock()
		switch {
		case held.drop || r.Context().Err() != nil:
			a = answer{abort: true}
			x.entry.Detail = "the hold ended with the connection closed and nothing applied"
		default:
			a = e.redecide(svc, x)
		}
		entry = e.complete(entry, x, a)
		e.mu.Unlock()
		e.notify(entry)
	}
	write(w, a)
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

// redecide answers a request a released hold kept, against the state
// as it is now, with no fault applied again. Callers hold mu.
func (e *Engine) redecide(svc service, x *exchange) answer {
	x.entry.Route, x.entry.Detail = "", ""
	if e.closed {
		return answer{abort: true}
	}
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
	}
	a := e.run(svc, x, m)
	if a.unhandled != "" {
		return a, nil
	}
	x.entry.Detail = fmt.Sprintf("applied and answered %d; the answer was dropped", a.status)
	return answer{abort: true}, nil
}

// run runs a route's handler, giving an unhandled answer the tracker's
// shape.
func (e *Engine) run(svc service, x *exchange, m match) answer {
	a := m.run()
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
	return e.log.add(x.entry.scrubbed(e.secrets(x)))
}

// complete replaces a held request's entry with how it was answered.
// Callers hold mu.
func (e *Engine) complete(entry Request, x *exchange, a answer) Request {
	x.entry.Held = false
	x.entry.Fault = FaultHold
	e.settle(x, a)
	out := x.entry.scrubbed(e.secrets(x))
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
// the entry.
func (x *exchange) recordBody() {
	if len(x.body) == 0 {
		return
	}
	mediaType, params, _ := mime.ParseMediaType(x.r.Header.Get("Content-Type"))
	switch {
	case mediaType == "multipart/form-data":
		x.entry.Files = multipartNames(x.body, params["boundary"])
	case len(x.body) <= maxLoggedBody && json.Valid(x.body):
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
