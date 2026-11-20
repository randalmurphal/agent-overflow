package tempo

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strconv"
	"sync"

	"agent-overflow/internal/tracker/trackerapi"
)

// pageSize is the page every listing asks for. Tempo's default is 50 and
// its documented maximum 5000; 1000 worklogs stay well under the
// transport's 8 MiB body bound.
const pageSize = 1000

// page is Tempo's list envelope. next is a link to the following page and
// is empty on the last one.
type page[T any] struct {
	Metadata struct {
		Count  int    `json:"count"`
		Offset int    `json:"offset"`
		Next   string `json:"next"`
	} `json:"metadata"`
	Results []T `json:"results"`
}

// pages reads r page by page with offset and limit, handing each page's
// answer and results to fn, until Tempo names no next page. It reads the next page
// by offset rather than by following the link, so every request stays on
// the base URL. A page that names a next page but holds nothing fails the
// call instead of asking for the same offset again.
//
// Every page must be drawn under the credential read the first was: the
// token selects the tenant, so an offset continued under another token
// is another tenant's list. Inside a walk a page that is not is
// errWalkMoved, and the walk reads again from the start; outside one fn
// has seen the earlier pages, so the listing fails with an error
// matching trackerapi.ErrCredentialChanged.
func pages[T any](ctx context.Context, h *trackerapi.Client, r trackerapi.Request, fn func(*trackerapi.Response, []T) error) error {
	w := walkOf(ctx)
	if w == nil {
		err := pages(context.WithValue(ctx, walkKey{}, &walker{}), h, r, fn)
		if errors.Is(err, errWalkMoved) {
			return fmt.Errorf("tempo: the credential changed between the pages of %s, which were handed over: %w", r.Path, trackerapi.ErrCredentialChanged)
		}
		return err
	}
	offset := 0
	for {
		q := maps.Clone(r.Query)
		if q == nil {
			q = url.Values{}
		}
		q.Set("offset", strconv.Itoa(offset))
		q.Set("limit", strconv.Itoa(pageSize))
		req := r
		req.Query = q
		var p page[T]
		resp, err := h.JSON(ctx, req, &p)
		if err != nil {
			return err
		}
		if err := w.see(resp.CredentialMark()); err != nil {
			return err
		}
		if err := fn(resp, p.Results); err != nil {
			return err
		}
		if p.Metadata.Next == "" {
			return nil
		}
		if len(p.Results) == 0 {
			return fmt.Errorf("the page at offset %d of %s named a next page but held no results", offset, r.Path)
		}
		offset += len(p.Results)
	}
}

// walkKey is the context key of the walker of the walk a page is part of.
type walkKey struct{}

// walker is the credential read of a walk's first page.
type walker struct {
	mu   sync.Mutex
	seen bool
	mark trackerapi.CredentialMark
}

// errWalkMoved is a page of a walk drawn under another credential read
// than the walk's first.
var errWalkMoved = errors.New("tempo: a page was drawn under another credential than the first")

// see records the credential read of a page, and is errWalkMoved when it
// is not the first page's.
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

// walk runs read, which composes one answer from the pages it reads
// under the context it is handed, as one walk: when a page is drawn under
// another credential read than the first, what read composed is dropped
// and read runs again from the start under the credential the transport
// holds now, once; a second change fails it with an error matching
// trackerapi.ErrCredentialChanged, which asking again resolves.
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
			return zero, fmt.Errorf("tempo: the credential changed twice during one read: %w", trackerapi.ErrCredentialChanged)
		}
	}
}
