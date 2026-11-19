package browser

import (
	"context"

	"agent-overflow/internal/browser/threadlife"
)

// A pane action binds to its thread's browser lifetime before anything it
// does can wait (BindThread). Every tab it opens and every jar switch it
// makes is admitted only while that lifetime holds, in the Manager.mu hold
// where the change commits (lifeAdmitsLocked). CloseThread retires the
// lifetime in its first hold, before it closes anything: a tab admitted
// before is one it closes, and nothing is admitted after.

// ErrThreadClosed answers a pane action whose thread's pages were closed (a
// delete, an archive, a handoff) while it ran: it opened nothing after the
// close.
var ErrThreadClosed = threadlife.ErrRetired

type threadLifeKey struct{}

// BindThread binds ctx to the thread's current browser lifetime for one pane
// action and answers end, which the action calls once when it is done and
// which answers ErrThreadClosed when the thread's pages were closed while
// the action was bound. A caller binds before it reads the thread's row, so
// a close after that read is one the binding sees.
func (m *Manager) BindThread(ctx context.Context, threadID string) (context.Context, func() error) {
	binding := m.lives.Bind(threadlife.ThreadID(threadID))
	return context.WithValue(ctx, threadLifeKey{}, binding), binding.End
}

// lifeAdmitsLocked answers ErrThreadClosed when ctx is bound to a lifetime
// of threadID that was retired. The caller holds m.mu, which CloseThread
// holds as it retires the lifetime.
func (m *Manager) lifeAdmitsLocked(ctx context.Context, threadID string) error {
	binding, _ := ctx.Value(threadLifeKey{}).(*threadlife.Binding)
	return binding.Admits(threadlife.ThreadID(threadID))
}
