package transport

import (
	"context"
	"time"
)

// A bound method's context depends on its declaration. A caller-bound
// method (//ao:callerbound) receives its caller's context, which ends when
// the caller's connection or request does. Every other method receives
// the caller's values on a context that ends only when the server stops,
// so a disconnect never cuts a method part way through an effect. Such a
// method that waits on its caller's behalf before any effect selects on
// CallerGone for that wait.

// callerKey carries the transport's record of the remote caller.
type callerKey struct{}

type remoteCaller struct {
	// gone closes when the caller's connection or request ends.
	gone <-chan struct{}
	// lifetime ends at server shutdown.
	lifetime context.Context
}

// withRemoteCaller marks ctx as a remote caller's context: ctx ends when
// the caller leaves, lifetime when the server stops.
func withRemoteCaller(ctx, lifetime context.Context) context.Context {
	return context.WithValue(ctx, callerKey{}, remoteCaller{gone: ctx.Done(), lifetime: lifetime})
}

// CallerGone returns a channel that closes when the caller waiting on
// this call has gone. For a call that did not arrive over the transport it
// is ctx.Done().
func CallerGone(ctx context.Context) <-chan struct{} {
	if caller, ok := ctx.Value(callerKey{}).(remoteCaller); ok {
		return caller.gone
	}
	return ctx.Done()
}

// methodContext is the context m runs on for a call arriving on ctx.
func methodContext(ctx context.Context, m *Method) context.Context {
	caller, ok := ctx.Value(callerKey{}).(remoteCaller)
	if !ok || m.CallerBound {
		return ctx
	}
	return serverBoundContext{lifetime: caller.lifetime, values: context.WithoutCancel(ctx)}
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
