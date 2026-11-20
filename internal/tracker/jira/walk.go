package jira

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"agent-overflow/internal/tracker/trackerapi"
)

// A walk is a read that composes one answer from several requests: the
// pages of a listing, a search to its end, the batches of a bulk read, an
// issue with the rest of its changelog. Every answer of a walk must be
// drawn under the credential read its first was, or the answer mixes two
// accounts' views (one account's first page and another's continued
// offset). Client.call checks each answer of a walk against its first;
// walk runs the read again from the start, once, when one differs, and
// fails it when the credential changes again.

// walkKey is the context key of the walker of the walk a request is part
// of.
type walkKey struct{}

// walker is the credential read of a walk's first answer.
type walker struct {
	mu   sync.Mutex
	seen bool
	mark trackerapi.CredentialMark
}

// errWalkMoved is an answer of a walk drawn under another credential read
// than the walk's first.
var errWalkMoved = errors.New("jira: an answer of a read of several requests was drawn under another credential than the first")

// see records the credential read of an answer of the walk, and is
// errWalkMoved when it is not the first answer's.
func (w *walker) see(mark trackerapi.CredentialMark) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case !w.seen:
		w.seen, w.mark = true, mark
	case mark != w.mark:
		return errWalkMoved
	}
	return nil
}

// walkOf is the walker of ctx's walk, nil outside one.
func walkOf(ctx context.Context) *walker {
	w, _ := ctx.Value(walkKey{}).(*walker)
	return w
}

// walk runs read as one walk: every answer of the requests it makes
// under the context it is handed must be drawn under one credential read
// (see Client.call). When one is not, what read composed is dropped and
// read runs again from the start under the credential the transport
// holds now, once; a second change fails the walk with an error matching
// trackerapi.ErrCredentialChanged, which asking again resolves. A walk
// inside another joins it, so the outer one starts again whole.
func walk[T any](ctx context.Context, read func(context.Context) (T, error)) (T, error) {
	if walkOf(ctx) != nil {
		return read(ctx)
	}
	for attempt := 0; ; attempt++ {
		out, err := read(context.WithValue(ctx, walkKey{}, &walker{}))
		if !errors.Is(err, errWalkMoved) {
			return out, err
		}
		if attempt > 0 {
			var zero T
			return zero, fmt.Errorf("jira: the credential changed twice during one read: %w", trackerapi.ErrCredentialChanged)
		}
	}
}
