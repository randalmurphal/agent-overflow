package main

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
func (a *App) SearchThreadMessages(query string, limit int) ([]store.ThreadMessageHit, error) {
	hits, err := a.store.SearchThreadMessages(query, limit)
	if err != nil {
		return nil, fmt.Errorf("search thread messages: %w", err)
	}
	return slicesx.OrEmpty(hits), nil
}
