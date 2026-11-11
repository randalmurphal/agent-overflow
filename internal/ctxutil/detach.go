package ctxutil

import "context"

// Detach returns a context with ctx's values that is cancelled when
// lifetime ends rather than when ctx does, for work that must outlive the
// caller who started it but not the owner that runs it. The release must
// be called when the work ends.
func Detach(ctx, lifetime context.Context) (context.Context, context.CancelFunc) {
	detached, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(lifetime, cancel)
	return detached, func() {
		stop()
		cancel()
	}
}
