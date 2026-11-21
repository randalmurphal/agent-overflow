package trackerfake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
)

// FaultKind is what a fault control does to the requests it matches.
type FaultKind string

// The fault controls. FaultOffline is never armed with AddFault; it marks
// the log entries of an origin SetOffline took down.
const (
	// FaultRateLimit answers as the tracker answers a rate limit: Jira
	// with its 429, rate headers and Retry-After; Linear with its 429,
	// RATELIMITED error, budget headers and Retry-After; Tempo with a
	// bare 429, as Tempo documents no headers or body for it.
	FaultRateLimit FaultKind = "rate_limit"
	// FaultRefuse answers Status, Header and Body in place of the route.
	FaultRefuse FaultKind = "refuse"
	// FaultHold keeps a request unanswered until Release or ClearFaults
	// names its origin and route, then answers it as usual, or closes its
	// connection with nothing applied when the release drops it.
	FaultHold FaultKind = "hold"
	// FaultFailAfterApply runs the route, so a write lands, and closes
	// the connection with no answer: a write of unknown outcome.
	FaultFailAfterApply FaultKind = "fail_after_apply"
	// FaultOffline marks a request an offline origin dropped.
	FaultOffline FaultKind = "offline"
)

// Fault is one fault control on an origin's route. A request matches
// the first armed fault, in the order armed, whose Origin is the one it
// reached and whose Route is its route, or empty for every route. Faults
// apply after the credential is checked and the route matched, so an
// unhandled request is unhandled whatever is armed.
type Fault struct {
	Origin Origin `json:"origin"`
	// Route is a route name (the Route of a logged request, such as
	// "GET rest/api/3/issue/{issue}") or a Linear operation name; empty
	// matches every route of the origin.
	Route string    `json:"route,omitempty"`
	Kind  FaultKind `json:"kind"`
	// Times is how many matching requests the fault takes; zero takes
	// every one until ClearFaults or Reset.
	Times int `json:"times,omitempty"`
	// Status, Header and Body are a refusal's answer: a status of 400 to
	// 599 and a JSON body, Content-Type set by the engine.
	Status int               `json:"status,omitempty"`
	Header map[string]string `json:"header,omitempty"`
	Body   json.RawMessage   `json:"body,omitempty"`
	// RetryAfter is a rate limit's Retry-After, in whole seconds, one or
	// more; Tempo's answer carries none.
	RetryAfter int `json:"retryAfter,omitempty"`
}

type armedFault struct {
	Fault
	// hold is the gate of a FaultHold.
	hold *hold
}

// hold is a held route's gate: done closes on release; drop, set before,
// closes the held requests' connections instead of answering them.
type hold struct {
	origin Origin
	route  string
	done   chan struct{}
	drop   bool
}

type faultState struct {
	offline map[Origin]bool
	armed   []*armedFault
	// holds are the gates not yet released, including those of holds
	// whose Times ran out while requests wait on them.
	holds []*hold
}

func newFaultState() faultState {
	return faultState{offline: map[Origin]bool{}}
}

// disarm clears every control. The holds' gates are endWorld's to close.
func (f *faultState) disarm() {
	clear(f.offline)
	f.armed = nil
}

// endWorld ends the world the requests in flight arrived in: it advances
// the generation and closes every hold's gate, so each held request
// wakes and is dropped with nothing applied (resume), even one a release
// already let go. A hold fault still armed keeps holding later requests,
// on a new gate. Reset, Close and a Seed that replaces a tenant call it.
// Callers hold mu.
func (e *Engine) endWorld() {
	e.gen++
	for _, h := range e.faults.holds {
		close(h.done)
	}
	e.faults.holds = nil
	for _, a := range e.faults.armed {
		if a.hold != nil {
			a.hold = &hold{origin: a.Origin, route: a.Route, done: make(chan struct{})}
			e.faults.holds = append(e.faults.holds, a.hold)
		}
	}
}

// SetOffline takes an origin down (or brings it back): while down, every
// request to it is recorded with Fault "offline" and its connection is
// closed without an answer. State is kept.
func (e *Engine) SetOffline(o Origin, offline bool) error {
	if err := o.check(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if offline {
		e.faults.offline[o] = true
	} else {
		delete(e.faults.offline, o)
	}
	return nil
}

// AddFault arms f after every fault already armed.
func (e *Engine) AddFault(f Fault) error {
	if err := f.check(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	armed := &armedFault{Fault: f}
	armed.Body = bytes.Clone(f.Body)
	if f.Kind == FaultHold {
		armed.hold = &hold{origin: f.Origin, route: f.Route, done: make(chan struct{})}
		e.faults.holds = append(e.faults.holds, armed.hold)
	}
	e.faults.armed = append(e.faults.armed, armed)
	return nil
}

// checkRoute refuses a route the origin does not serve. A Linear route
// is an operation name, since the executor serves any operation the
// schema admits; it must be a GraphQL name.
func checkRoute(o Origin, route string) error {
	switch {
	case route == "":
		return nil
	case o.Tracker == TrackerLinear:
		for i, r := range route {
			letter := r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
			if !letter && (i == 0 || r < '0' || r > '9') {
				return fmt.Errorf("trackerfake: fault: %q is not a GraphQL operation name", route)
			}
		}
		return nil
	case !slices.Contains(serviceFor(o).routeNames(), route):
		return fmt.Errorf("trackerfake: fault: %s has no route %q", o, route)
	}
	return nil
}

func (f Fault) check() error {
	if err := f.Origin.check(); err != nil {
		return err
	}
	if err := checkRoute(f.Origin, f.Route); err != nil {
		return err
	}
	if f.Times < 0 {
		return fmt.Errorf("trackerfake: fault: times %d is negative", f.Times)
	}
	refusal := f.Status != 0 || f.Header != nil || len(f.Body) > 0
	switch f.Kind {
	case FaultRefuse:
		if f.Status < 400 || f.Status > 599 {
			return fmt.Errorf("trackerfake: fault: a refusal's status %d is not 400 to 599", f.Status)
		}
		if len(f.Body) > 0 && !json.Valid(f.Body) {
			return fmt.Errorf("trackerfake: fault: a refusal's body is not JSON")
		}
		for name := range f.Header {
			if http.CanonicalHeaderKey(name) == "Content-Type" {
				return fmt.Errorf("trackerfake: fault: Content-Type is the engine's to set")
			}
		}
		if f.RetryAfter != 0 {
			return fmt.Errorf("trackerfake: fault: a refusal takes no retryAfter; a refusal Header can carry Retry-After")
		}
		return nil
	case FaultRateLimit:
		if f.RetryAfter < 1 {
			return fmt.Errorf("trackerfake: fault: a rate limit's retryAfter %d is not one second or more", f.RetryAfter)
		}
	case FaultHold, FaultFailAfterApply:
		if f.RetryAfter != 0 {
			return fmt.Errorf("trackerfake: fault: %s takes no retryAfter", f.Kind)
		}
	case FaultOffline:
		return fmt.Errorf("trackerfake: fault: %s is not armed as a fault; SetOffline takes an origin down", f.Kind)
	default:
		return fmt.Errorf("trackerfake: fault: unknown kind %q", f.Kind)
	}
	if refusal {
		return fmt.Errorf("trackerfake: fault: %s takes no status, header or body", f.Kind)
	}
	return nil
}

// match takes the first armed fault matching o and route, counting it
// down. Callers hold mu.
func (f *faultState) match(o Origin, route string) *armedFault {
	for i, a := range f.armed {
		if a.Origin != o || (a.Route != "" && a.Route != route) {
			continue
		}
		if a.Times > 0 {
			a.Times--
			if a.Times == 0 {
				f.armed = slices.Delete(f.armed, i, i+1)
			}
		}
		return a
	}
	return nil
}

// Release ends the holds armed on exactly o and route: their faults stop
// matching, and the requests they hold are answered, or with drop have
// their connections closed with nothing applied.
func (e *Engine) Release(o Origin, route string, drop bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	found := false
	e.faults.holds = slices.DeleteFunc(e.faults.holds, func(h *hold) bool {
		if h.origin != o || h.route != route {
			return false
		}
		found = true
		h.drop = drop
		close(h.done)
		return true
	})
	if !found {
		return fmt.Errorf("trackerfake: release: no hold on %s route %q", o, route)
	}
	e.faults.armed = slices.DeleteFunc(e.faults.armed, func(a *armedFault) bool {
		return a.Kind == FaultHold && a.Origin == o && a.Route == route
	})
	return nil
}

// ClearFaults disarms every fault armed on exactly o and route, answering
// the requests its holds keep.
func (e *Engine) ClearFaults(o Origin, route string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.faults.armed = slices.DeleteFunc(e.faults.armed, func(a *armedFault) bool {
		return a.Origin == o && a.Route == route
	})
	e.faults.holds = slices.DeleteFunc(e.faults.holds, func(h *hold) bool {
		if h.origin != o || h.route != route {
			return false
		}
		close(h.done)
		return true
	})
}
