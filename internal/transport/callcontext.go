package transport

import (
	"context"
	"time"
)

// A bound method's context depends on its declaration. A caller-bound
// method (//ao:callerbound) receives its caller's context, which ends when
// the caller's connection or request does. Every other method receives
// the caller's values on a context that ends only when the server stops,
// so its caller leaving never affects it.

// serverLifetimeKey carries the server's lifetime on a remote call.
type serverLifetimeKey struct{}

// withServerLifetime marks ctx as a remote caller's context, whose
// non-caller-bound methods run until lifetime ends.
func withServerLifetime(ctx, lifetime context.Context) context.Context {
	return context.WithValue(ctx, serverLifetimeKey{}, lifetime)
}

// methodContext is the context m runs on for a call arriving on ctx.
func methodContext(ctx context.Context, m *Method) context.Context {
	lifetime, ok := ctx.Value(serverLifetimeKey{}).(context.Context)
	if !ok || m.CallerBound {
		return ctx
	}
	return serverBoundContext{lifetime: lifetime, values: context.WithoutCancel(ctx)}
}

// serverBoundContext has its caller's values and its server's lifetime.
// It allocates nothing per call and registers nothing on either parent.
type serverBoundContext struct {
	lifetime context.Context
	values   context.Context
}

func (c serverBoundContext) Deadline() (time.Time, bool) { return c.lifetime.Deadline() }
func (c serverBoundContext) Done() <-chan struct{}       { return c.lifetime.Done() }
func (c serverBoundContext) Err() error                  { return c.lifetime.Err() }
func (c serverBoundContext) Value(key any) any           { return c.values.Value(key) }

// AfterFunc lets a context derived from this one register on the server's
// lifetime instead of starting a goroutine to watch it.
func (c serverBoundContext) AfterFunc(f func()) func() bool {
	return context.AfterFunc(c.lifetime, f)
}
