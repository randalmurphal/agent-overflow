package app

import (
	"context"
	"fmt"

	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
)

// SearchThreadMessages runs a global substring search across thread titles
// and item summaries. Intended to back the message-search overlay in the
// frontend — kept thin so the store owns the query shape.
//
// A zero or empty query short-circuits to an empty slice in the store; this
// binding returns the same without an error so the frontend can call it on
// every keystroke cheaply. limit caps the result set; callers should pass a
// reasonable ceiling (50–100) for interactive UIs.
//
//ao:scope threads:read
//ao:route all
func (a *App) SearchThreadMessages(query string, limit int) ([]store.ThreadMessageHit, error) {
	hits, err := a.store.SearchThreadMessages(query, limit)
	if err != nil {
		return nil, fmt.Errorf("search thread messages: %w", err)
	}
	return slicesx.OrEmpty(hits), nil
}

// SearchThreadItems answers the find bar of one thread's main transcript:
// the visible top-level rows that may hold a match, in document order, as
// timeline pages serve them; the find bar decides which hold one. A blank
// query answers no rows without an error. limit caps the rows. since, an
// earlier answer's state, answers only what changed after it when the
// store can. The store owns the query.
//
//ao:scope threads:read
//ao:callerbound
func (a *App) SearchThreadItems(ctx context.Context, threadID, query string, limit int, since *store.ThreadFindSince) (store.ThreadFindAnswer, error) {
	return a.SearchTimelineItems(ctx, threadID, query, store.TimelineSelection{}, limit, since)
}

// SearchTimelineItems is SearchThreadItems over the selected transcript of
// one thread: the rows the timeline shows for that selection (an agent
// scope answers its direct children). A cancelled call (the connection
// closed) stops the read.
//
//ao:scope threads:read
//ao:callerbound
func (a *App) SearchTimelineItems(ctx context.Context, threadID, query string, selection store.TimelineSelection, limit int, since *store.ThreadFindSince) (store.ThreadFindAnswer, error) {
	answer, err := a.store.SearchThreadItems(ctx, threadID, query, selection, limit, since)
	if err != nil {
		return store.ThreadFindAnswer{}, fmt.Errorf("search thread items: %w", err)
	}
	return answer, nil
}
