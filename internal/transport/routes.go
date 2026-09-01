package transport

// MethodRoute names WHICH attached backend a bound method's call belongs
// to. It is the vocabulary the `//ao:route` source annotation is written
// in, the vocabulary `methodgen` validates against, and the vocabulary
// the generated table carries (docs/specs/remote-access.md §10, "Routing
// an RPC").
//
// Scope answers "may this caller do it"; route answers "on which
// machine". They are independent: a method routed to the thread's
// backend is still gated there by its own scope, and a `home` route
// carries no authority of its own.
//
// This package does not ROUTE anything. One connection is one backend,
// and every RPC that arrives on it is for the backend that answered it.
// The column exists for the client, which holds one `WSClient` per
// attached backend and has to pick one before it writes a frame; the
// generated TS mirror (`frontend/src/lib/transport/methodRoutes.ts`) is
// what it reads. Keeping the declaration here, beside the scope it ships
// with, is what makes the two one decision on one screen.
type MethodRoute string

const (
	// RouteThread sends the call to the backend that owns the thread
	// named by the method's first non-context parameter. Inferred by
	// `methodgen` from a parameter named threadID.
	RouteThread MethodRoute = "thread"
	// RouteProject sends the call to the backend that owns the project
	// named by the method's first non-context parameter. Inferred by
	// `methodgen` from a parameter named projectID.
	RouteProject MethodRoute = "project"
	// RouteHome sends the call to the backend that served the page: host
	// actions, this machine's settings, this backend's own access admin,
	// its provider accounts, its update supervisor, its ui_state bucket.
	RouteHome MethodRoute = "home"
	// RouteSelected sends the call to the backend the composer is
	// pointed at — the creation-shaped calls, and the reads that take a
	// workspace path rather than an entity id, which only mean anything
	// against the machine that holds that path.
	RouteSelected MethodRoute = "selected"
	// RouteAll fans the call out to every attached backend and lets the
	// client merge the answers. The list calls behind the unified
	// sidebar and the cross-thread views. Nothing merges on the Go side
	// (root AGENTS.md, "Go is triage + pipe").
	RouteAll MethodRoute = "all"
)

// MethodRoutes is every declared route, entity-addressed first, then the
// three that name a backend by role.
var MethodRoutes = []MethodRoute{
	RouteThread, RouteProject, RouteHome, RouteSelected, RouteAll,
}

// Valid reports whether r is a declared route. `methodgen` refuses an
// annotation naming anything else and refuses to emit a table with an
// unrouted method in it, so a valid generated table cannot carry an
// invalid route; this answers the question for hand-built values.
func (r MethodRoute) Valid() bool {
	for _, known := range MethodRoutes {
		if known == r {
			return true
		}
	}
	return false
}
