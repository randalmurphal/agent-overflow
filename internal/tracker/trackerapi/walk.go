package trackerapi

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// A walk is a read that composes one answer from several requests: the
// pages of a listing, the batches of a bulk read, the client reads one
// provider read is made of. The token selects what a tracker shows, so
// every answer of a walk must be drawn under one credential read of each
// client it calls, or the answer mixes two accounts' views (one
// account's first batch beside another's second). A request made under
// a walk's context is checked by the transport itself when its answer
// settles: an answer of a client whose CredentialMark differs from the
// first answer of that client in the walk fails, and the walk runs again
// from its start under the credential the transport holds now, once. A
// second change fails it with an error matching ErrCredentialChanged,
// which asking again resolves. An answer that carried no credential (a
// request off the base origin) speaks for no account and is admitted.
//
// Every answer is checked, an error answer included: a not-found is a
// fact a read reports (a missing issue), so one drawn under another
// credential moves the walk as a success would. The walk remembers that
// it moved, and runs again whatever its read then returned: a read that
// keeps a request's failure in a partial result (a board it could not
// judge) cannot hide the move.
//
// A walk is a read: a write sent under a walk's context is refused
// before it is sent, so running a walk again never sends a write twice.
// A walk inside another joins it, so the outer one starts again whole.

// walkKey is the context key of the walker of the walk a request is part
// of.
type walkKey struct{}

// walker is the credential read each client of a walk answered its
// first request under, and whether any later answer or joined value was
// drawn under another.
type walker struct {
	mu    sync.Mutex
	marks []CredentialMark
	buf   [2]CredentialMark
	moved bool
}

// errWalkMoved is an answer of a walk drawn under another credential
// read of its client than the walk's first answer of that client.
var errWalkMoved = errors.New("trackerapi: an answer of a read of several requests was drawn under another credential than an earlier answer of it")

// admit records mark, an answer's or a cached value's, in the walk, and
// reports whether it agrees with the walk's first mark of its client;
// one that does not moves the walk for good. The zero mark agrees with
// every walk.
func (w *walker) admit(mark CredentialMark) bool {
	if mark.client == nil {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, m := range w.marks {
		if m.client == mark.client {
			if m != mark {
				w.moved = true
			}
			return m == mark
		}
	}
	if w.marks == nil {
		w.marks = w.buf[:0]
	}
	w.marks = append(w.marks, mark)
	return true
}

// hasMoved reports whether the walk admitted a mark that disagreed.
func (w *walker) hasMoved() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.moved
}

// walkOf is the walker of ctx's walk, nil outside one.
func walkOf(ctx context.Context) *walker {
	w, _ := ctx.Value(walkKey{}).(*walker)
	return w
}

// Walk runs read as one walk (see above): every answer of the requests
// read makes under the context it is handed is drawn under one
// credential read of each client. When one is not, what read returned,
// a result or an error, is dropped and read runs again from the start,
// once; a second change fails the walk with an error matching
// ErrCredentialChanged. read must keep what it composes in what it
// returns, so that a second run starts from nothing. Inside another
// walk, Walk joins it.
func Walk[T any](ctx context.Context, read func(context.Context) (T, error)) (T, error) {
	if walkOf(ctx) != nil {
		return read(ctx)
	}
	for attempt := 0; ; attempt++ {
		w := &walker{}
		out, err := read(context.WithValue(ctx, walkKey{}, w))
		if !w.hasMoved() {
			return out, err
		}
		if attempt > 0 {
			var zero T
			return zero, fmt.Errorf("trackerapi: the credential changed twice during one read: %w", ErrCredentialChanged)
		}
	}
}

// WalkOnce runs read as one walk that cannot start again, because read
// hands what it reads over as it reads (a page at a time, or a download
// to a writer): an answer drawn under another credential read fails it
// with an error matching ErrCredentialChanged, whatever read returned,
// after read has handed over the earlier answers. Inside another walk,
// WalkOnce joins it, and that walk starts again whole.
func WalkOnce(ctx context.Context, read func(context.Context) error) error {
	if walkOf(ctx) != nil {
		return read(ctx)
	}
	w := &walker{}
	err := read(context.WithValue(ctx, walkKey{}, w))
	if w.hasMoved() {
		return fmt.Errorf("trackerapi: the credential changed during a read that handed over what it read: %w", ErrCredentialChanged)
	}
	return err
}

// JoinWalk admits a value drawn under mark that reached a walk without
// an answer of its own, such as a cached answer, into ctx's walk: nil
// outside a walk, and inside one when mark agrees with the walk's first
// mark of its client, which mark becomes when the walk has none.
// Otherwise it is the error that runs the walk again, which the cache
// returns, or reads as a miss so that the request it sends moves the
// walk.
func JoinWalk(ctx context.Context, mark CredentialMark) error {
	if w := walkOf(ctx); w == nil || w.admit(mark) {
		return nil
	}
	return errWalkMoved
}

// OutsideWalk is ctx with no walk, for a read one caller starts and
// others share: each caller joins its own walk with what the read was
// drawn under (JoinWalk).
func OutsideWalk(ctx context.Context) context.Context {
	if walkOf(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, walkKey{}, (*walker)(nil))
}

// walked checks a, the answer that settled a call, an error answer
// included, against the walk ctx is part of, and clears the body of an
// answer it refuses, which nothing has scrubbed yet.
func (c *Client) walked(ctx context.Context, a *answer) error {
	w := walkOf(ctx)
	if w == nil {
		return nil
	}
	ref := c.refOf(a.sent)
	if w.admit(ref.mark()) {
		return nil
	}
	clear(a.body)
	return fmt.Errorf("trackerapi: %s: %w", c.name, errWalkMoved)
}

// refuseInWalk refuses a write sent under a walk's context.
func (c *Client) refuseInWalk(ctx context.Context, k *call) error {
	if k.read || walkOf(ctx) == nil {
		return nil
	}
	return InvalidInput(fmt.Errorf("trackerapi: %s: a %s is a write, and a write is never part of a read that may run again", c.name, k.method))
}
