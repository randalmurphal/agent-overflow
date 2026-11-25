// Package trackerfake is a stateful fake of Jira Cloud (the platform REST
// API and the Jira Software APIs), Tempo and Linear, for app tests, the
// backend harness and browser tests. It owns the fake trackers' state,
// their answers, the request log and the fault controls; it does not
// wire itself into the app or the harness, and imports no tracker client.
//
// One Engine is one fake world: any number of Jira sites, each with an
// optional Tempo account, and any number of Linear workspaces. Handler
// serves one origin (a Jira site, the Tempo API, the Linear API), so a
// caller puts each on its own loopback listener; Serve does that with
// http.Servers it owns. State is seeded from one Fixture and
// read back through Snapshot, both plain JSON values; reads answer from
// it and writes change it, stamped by the engine's clock.
//
// A request the fake does not implement (route, method, query parameter,
// body member, JQL form, GraphQL operation or variable) is unhandled: it
// is answered 501 with a message that names it and recorded with
// Unhandled set, never answered with a plausible default. See AGENTS.md
// beside this file for the answer rules and how to add a route.
package trackerfake

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Tracker names the kind of origin a request reached.
type Tracker string

// The trackers the engine fakes.
const (
	TrackerJira   Tracker = "jira"
	TrackerTempo  Tracker = "tempo"
	TrackerLinear Tracker = "linear"
)

// Origin is one served origin: a Jira site by its host (Site, such as
// example.atlassian.net), or the Tempo or Linear API, each one origin for
// every tenant (Site empty), as the real services are.
type Origin struct {
	Tracker Tracker `json:"tracker"`
	Site    string  `json:"site,omitempty"`
}

func (o Origin) String() string {
	if o.Site == "" {
		return string(o.Tracker)
	}
	return string(o.Tracker) + ":" + o.Site
}

// check refuses an origin no handler serves.
func (o Origin) check() error {
	switch o.Tracker {
	case TrackerJira:
		if o.Site == "" {
			return errors.New("trackerfake: a Jira origin names its site")
		}
	case TrackerTempo, TrackerLinear:
		if o.Site != "" {
			return fmt.Errorf("trackerfake: the %s origin serves every tenant and names no site", o.Tracker)
		}
	default:
		return fmt.Errorf("trackerfake: unknown tracker %q", o.Tracker)
	}
	return nil
}

// Options wires an Engine to its host.
type Options struct {
	// Now is the clock writes are stamped with; nil is time.Now. The
	// engine's own clock never moves back: a reading earlier than the
	// last one stamps with the last one.
	Now func() time.Time
	// OnRequest observes every recorded request after it is recorded,
	// without the engine's lock held. A held request is reported when it
	// arrives and again when it is answered.
	OnRequest func(Request)
}

// maxRequestBody bounds what the engine reads of a request body; an
// upload of more is unhandled.
const maxRequestBody = 32 << 20

// Engine is one fake world. It is safe for concurrent use: every request
// is answered under one lock, except while a held request waits.
type Engine struct {
	opts Options

	mu     sync.Mutex
	closed bool
	// gen counts the worlds the engine has held; endWorld starts a new
	// one.
	gen uint64
	// last is the latest clock reading handed out.
	last time.Time
	// ids generates the numeric IDs of new records; it keeps counting
	// across Reset so no ID repeats within an engine's life.
	ids    int64
	jira   map[string]*jiraSite
	linear map[string]*linearWorkspace
	faults faultState
	log    requestLog
}

// New builds an empty engine. Every request is unhandled until a fixture
// seeds the tenant it names.
func New(opts Options) *Engine {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Engine{
		opts: opts, ids: firstGeneratedID,
		jira: map[string]*jiraSite{}, linear: map[string]*linearWorkspace{},
		faults: newFaultState(),
	}
}

// firstGeneratedID is the first ID the engine generates; seeded IDs above
// it move it up.
const firstGeneratedID = 1_000_000

// now is the engine's clock: Options.Now, never earlier than a reading
// already handed out. Callers hold mu.
func (e *Engine) now() time.Time {
	t := e.opts.Now().UTC().Truncate(time.Millisecond)
	if t.Before(e.last) {
		return e.last
	}
	e.last = t
	return t
}

// nextID is a new numeric ID. Callers hold mu.
func (e *Engine) nextID() int64 {
	e.ids++
	return e.ids
}

// reserveID keeps generated IDs above id. Callers hold mu.
func (e *Engine) reserveID(id int64) {
	e.ids = max(e.ids, id)
}

// Seed adds the fixture's tenants, replacing any already seeded under the
// same site or workspace key; a request held when a tenant is replaced
// ends with its connection closed and nothing applied. A fixture the
// engine cannot hold (a reference to something it lacks, an ID twice) is
// refused whole and changes nothing.
func (e *Engine) Seed(f Fixture) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errClosed
	}
	return e.seed(f)
}

// Reset drops every tenant, fault, held request and logged request. A
// held request ends with its connection closed and nothing applied, even
// one a release already let go. IDs keep counting.
func (e *Engine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	clear(e.jira)
	clear(e.linear)
	e.faults.disarm()
	e.endWorld()
	e.log.reset()
}

// Close ends every held request, as Reset does, and answers every later
// request by closing its connection. Close may be called more than once.
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	e.faults.disarm()
	e.endWorld()
}

var errClosed = errors.New("trackerfake: the engine is closed")

// Handler serves one origin. A Jira handler answers for its site once a
// fixture seeds it; the Tempo and Linear handlers answer every tenant,
// each request for the one its credential names.
func (e *Engine) Handler(o Origin) (http.Handler, error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	var svc service
	switch o.Tracker {
	case TrackerJira:
		svc = jiraService{site: o.Site}
	case TrackerTempo:
		svc = tempoService{}
	case TrackerLinear:
		svc = linearService{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.serve(o, svc, w, r)
	}), nil
}

// errBodyCutShort is a request body that stopped before its end: its
// client left or closed the connection. As at any HTTP server, the
// tracker never sees the request whole, so it applies nothing and nobody
// is left to answer.
var errBodyCutShort = errors.New("the request body stopped before its end")

// readBody reads r's body up to maxRequestBody; over it is an error, and
// a body that stops early is errBodyCutShort.
func readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errBodyCutShort, err)
	}
	if len(body) > maxRequestBody {
		return nil, fmt.Errorf("the request body is over %d bytes", maxRequestBody)
	}
	return body, nil
}
