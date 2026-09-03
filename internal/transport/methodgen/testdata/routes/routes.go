// Package routes is the methodgen fixture for the Route column: the two
// inferred routes, the case and context variations that must not change
// the answer, and an explicit directive overriding an inference. Under
// testdata/, so the go tool never builds it.
package routes

import (
	"context"

	"agent-overflow/internal/gitapp"
)

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

// WorkspaceRef stands in for the alias internal/app declares over
// gitapp.WorkspaceRef; the inference reads the type's NAME.
type WorkspaceRef struct{}

// Workspaced infers `workspace` from a first parameter typed with the
// package-local alias.
//
//ao:scope files:read
func (e *Eta) Workspaced(ws WorkspaceRef, branch string) {}

// WorkspacedQualified infers the same from the qualified spelling.
//
//ao:scope files:read
func (e *Eta) WorkspacedQualified(ctx context.Context, ws gitapp.WorkspaceRef) {}
