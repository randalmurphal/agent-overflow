package app

import (
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

// SearchThreadItems searches the main transcript of one thread for the
// find bar: the visible top-level rows, in document order, so the UI can
// step through them top-to-bottom. A blank query answers an empty slice
// without an error. limit caps the result set. The store owns the query.
//
//ao:scope threads:read
func (a *App) SearchThreadItems(threadID, query string, limit int) ([]store.ThreadMessageHit, error) {
	return a.SearchTimelineItems(threadID, query, store.TimelineSelection{}, limit)
}

// SearchTimelineItems searches the selected transcript of one thread: the
// rows the timeline shows for that selection (an agent scope answers its
// direct children), in document order.
//
//ao:scope threads:read
func (a *App) SearchTimelineItems(threadID, query string, selection store.TimelineSelection, limit int) ([]store.ThreadMessageHit, error) {
	hits, err := a.store.SearchThreadItems(threadID, query, selection, limit)
	if err != nil {
		return nil, fmt.Errorf("search thread items: %w", err)
	}
	return slicesx.OrEmpty(hits), nil
}
