// Package routes is the methodgen fixture for the Route column: the two
// inferred routes, the case and context variations that must not change
// the answer, and an explicit directive overriding an inference. Under
// testdata/, so the go tool never builds it.
package routes

import "context"

// Eta is the fixture receiver.
type Eta struct{}

// Threaded infers `thread` from its first parameter.
//
//ao:scope threads:read
func (e *Eta) Threaded(threadID string) {}

// ThreadedCtx proves two things at once: the leading context is not the
// parameter a route is read from, and the id's Go spelling does not
// decide the answer.
//
//ao:scope threads:read
func (e *Eta) ThreadedCtx(ctx context.Context, threadId, note string) {}

// Projected infers `project`.
//
//ao:scope threads:read
func (e *Eta) Projected(projectID string) {}

// Overridden names a thread id and is still routed home, which is the
// browser-companion shape: the pane is a native view on the page's own
// machine whatever thread it is showing.
//
//ao:scope host
//ao:route home
func (e *Eta) Overridden(threadID string) {}

// Declared has no inferrable parameter and says so itself.
//
//ao:scope threads:read
//ao:route all
func (e *Eta) Declared() {}
