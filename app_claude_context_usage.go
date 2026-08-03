package main

import (
	"context"
	"time"

	"agent-overflow/internal/provider/claude"
)

// claudeContextUsageTimeout bounds the wait on the get_context_usage
// round-trip. The CLI answers out of band (no turn, no API call), but it
// does tokenize the whole conversation to build the breakdown, so this is
// looser than the sub-100ms stop_task ceiling. The per-session control
// timeout applies underneath; this context only guarantees the binding
// itself can never hang the caller.
const claudeContextUsageTimeout = 15 * time.Second

// ThreadContextUsage is the answer to "what is actually in this thread's
// context window right now".
//
// Available=false is a first-class answer, not a failure: the breakdown
// exists only while a Claude process is running, and there is no honest way
// to synthesise it from history. Callers render Reason, never zeros.
// Genuine faults (a wedged CLI, a provider-side error) come back as a Go
// error instead, so "no session" and "something broke" never collapse into
// the same UI state.
type ThreadContextUsage struct {
	Available bool `json:"available"`
	// Reason is a short user-facing sentence, set only when Available is
	// false.
	Reason string `json:"reason,omitempty"`
	// TotalTokens is the context the model actually sees. Deferred tool
	// definitions are listed in Categories but excluded from this figure.
	TotalTokens int `json:"totalTokens"`
	// MaxTokens is the model's context window as the CLI reports it.
	MaxTokens int `json:"maxTokens"`
	// Percentage is the CLI's own rounded occupancy. Displayed as given —
	// the CLI owns the denominator it used.
	Percentage int `json:"percentage"`
	// Model is the slug the breakdown was computed for. It can legitimately
	// differ from the thread's configured model when a live model switch is
	// pending (set_model applies from the next turn).
	Model string `json:"model,omitempty"`
	// Categories is the breakdown in the CLI's own order. Never nil when
	// Available is true.
	Categories []ThreadContextUsageCategory `json:"categories"`
}

// ThreadContextUsageCategory is one row of the breakdown. Name is passed
// through from the CLI verbatim so a category added in a future release
// still renders.
type ThreadContextUsageCategory struct {
	Name   string `json:"name"`
	Tokens int    `json:"tokens"`
	// Deferred rows are excluded from TotalTokens. A consumer that sums the
	// rows must skip them or it will overcount.
	Deferred bool `json:"deferred,omitempty"`
}

// GetThreadContextUsage returns Claude's canonical `/context` breakdown for
// a thread's live session.
//
// This is a user-initiated, live-session-only read. It is not cached, not
// persisted, and not polled: the numbers describe the provider process's
// state right now and are stale as soon as the next turn runs (Core
// Principle 2 — the provider process is the source of truth during a turn,
// and we do not duplicate its state). The passive `message_delta.usage`
// signal continues to drive the always-on meter; this is the exact reading
// behind it.
//
// Local-only on the wire: it drives a provider session running under the
// user's credentials on the host.
func (a *App) GetThreadContextUsage(threadID string) (ThreadContextUsage, error) {
	if a.shuttingDown.Load() {
		return ThreadContextUsage{}, ErrShuttingDown
	}

	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return ThreadContextUsage{
			Reason: "The exact breakdown needs a running Claude session. Start the thread to read it.",
		}, nil
	}
	if sess.claude == nil {
		// Codex has no equivalent: its context accounting arrives on
		// token-count notifications with no category breakdown at all.
		return ThreadContextUsage{
			Reason: "The exact breakdown is only available on Claude threads.",
		}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), claudeContextUsageTimeout)
	defer cancel()
	usage, err := sess.claude.GetContextUsage(ctx)
	if err != nil {
		return ThreadContextUsage{}, err
	}
	return projectThreadContextUsage(usage), nil
}

// projectThreadContextUsage narrows the provider-shaped breakdown onto the
// wire type. Split out so the projection is testable without a subprocess.
func projectThreadContextUsage(usage *claude.ContextUsage) ThreadContextUsage {
	out := ThreadContextUsage{
		Available:   true,
		TotalTokens: usage.TotalTokens,
		MaxTokens:   usage.MaxTokens,
		Percentage:  usage.Percentage,
		Model:       usage.Model,
		Categories:  make([]ThreadContextUsageCategory, 0, len(usage.Categories)),
	}
	for _, cat := range usage.Categories {
		out.Categories = append(out.Categories, ThreadContextUsageCategory{
			Name:     cat.Name,
			Tokens:   cat.Tokens,
			Deferred: cat.Deferred,
		})
	}
	return out
}
