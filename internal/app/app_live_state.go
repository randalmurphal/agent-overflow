package app

import (
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/flushqueue"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// ThreadLiveState is the backend-owned live projection a freshly loaded
// frontend needs after refresh/reconnect. SQLite remains the history cache;
// every field here mirrors what an active provider process is doing right
// now — with one deliberate durable exception, Todo, which is read from
// threads.live_todo (migration v65) because a todo list outlives the session
// that reported it and the process that received it.
type ThreadLiveState struct {
	ThreadID               string               `json:"threadId"`
	EffectiveModel         string               `json:"effectiveModel,omitempty"`
	EffectiveModelRevision uint64               `json:"effectiveModelRevision,omitempty"`
	ActiveTurn             *LiveStateActiveTurn `json:"activeTurn,omitempty"`
	QueueItems             []QueuedItem         `json:"queueItems"`
	FlushedItems           []QueueFlushedItem   `json:"flushedItems"`
	// DeferredItems are pending-send timeline rows not yet persisted to
	// SQLite (they persist on their wire echo), in FIFO send order. A
	// refresh reconciling against a ListThreadSliceAround page merges
	// these in so the user's own just-sent message survives the install.
	DeferredItems   []store.Item                        `json:"deferredItems"`
	Interactive     provider.PendingInteractiveRequests `json:"interactive"`
	Todo            *LiveStateTodo                      `json:"todo,omitempty"`
	ProviderAccount *ProviderSessionAccountEvent        `json:"providerAccount,omitempty"`
	// CompactingSinceUnixMs is non-zero while the provider is compacting
	// this thread's context (epoch ms of the window's start). Mirrors the
	// `provider:compacting` push channel for refresh/reconnect — the
	// window can span minutes of wire silence, so no push will restate it.
	CompactingSinceUnixMs int64 `json:"compactingSinceUnixMs,omitempty"`
	// SessionCLIVersion / InstalledCLIVersion mirror the `binary_stale`
	// provider:status push for refresh/reconnect: the build this thread's
	// live process is running versus the build now on disk. Both empty
	// unless the thread is currently flagged stale — that push happens
	// once, on the transition, so a webview that connects afterwards has
	// no other way to learn it. See app_provider_binary_watch.go.
	SessionCLIVersion   string `json:"sessionCliVersion,omitempty"`
	InstalledCLIVersion string `json:"installedCliVersion,omitempty"`
}

type LiveStateActiveTurn struct {
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId"`
	TurnIndex int    `json:"turnIndex"`
	StartedAt int64  `json:"startedAt"`
}

type LiveStateTodo struct {
	ThreadID  string              `json:"threadId"`
	Steps     []LiveStateTodoStep `json:"steps"`
	UpdatedAt int64               `json:"updatedAt"`
}

type LiveStateTodoStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
	ID     string `json:"id,omitempty"`
	Owner  string `json:"owner,omitempty"`
}

// GetThreadLiveState returns the current server-side live state for a thread.
// It is the refresh/reconnect companion to the push events emitted during a
// normal uninterrupted frontend session.
//
// LocalOnly: the payload can expose pending prompts, tool approvals, drafted
// queued messages, attachment ids, and provider session state. It belongs in
// the same loopback-only class as GetQueueState and ListPendingInteractiveRequests.
func (a *App) GetThreadLiveState(threadID string) (ThreadLiveState, error) {
	threadID = strings.TrimSpace(threadID)
	state := ThreadLiveState{
		ThreadID:      threadID,
		QueueItems:    []QueuedItem{},
		FlushedItems:  []QueueFlushedItem{},
		DeferredItems: []store.Item{},
		Interactive: provider.PendingInteractiveRequests{
			Approvals:  []provider.ApprovalRequest{},
			UserInputs: []provider.UserInputRequest{},
		},
	}
	if threadID == "" {
		return state, fmt.Errorf("get thread live state: empty thread id")
	}
	if _, err := a.store.GetThread(threadID); err != nil {
		return state, fmt.Errorf("get thread live state: %w", err)
	}
	state.ProviderAccount = a.providerSessionAccount(threadID)
	state.Todo = a.threadLiveTodo(threadID)
	state.SessionCLIVersion, state.InstalledCLIVersion = a.staleProviderBinaryVersions(threadID)
	if a.triage == nil {
		return state, nil
	}

	live := a.triage.LiveStateSnapshotForThread(threadID)
	state.EffectiveModel = live.EffectiveModel
	state.EffectiveModelRevision = live.EffectiveModelRevision
	state.CompactingSinceUnixMs = live.CompactingSinceUnixMs
	if live.ActiveTurn != nil {
		state.ActiveTurn = &LiveStateActiveTurn{
			ThreadID:  live.ActiveTurn.ThreadID,
			TurnID:    live.ActiveTurn.TurnID,
			TurnIndex: live.ActiveTurn.TurnIndex,
			StartedAt: live.ActiveTurn.StartedAt,
		}
	}
	state.Interactive = live.Interactive
	state.DeferredItems = append(state.DeferredItems, live.DeferredItems...)
	for _, item := range live.QueueItems {
		state.QueueItems = append(state.QueueItems, flushqueue.ItemFromTriage(threadID, item))
	}
	for _, item := range live.FlushedItems {
		state.FlushedItems = append(state.FlushedItems, QueueFlushedItem{
			QueueItemID: item.QueueItemID,
			UserItemID:  item.UserItemID,
			Message:     item.Message,
		})
	}
	return state, nil
}

// liveTodoAutoHideMillis is how long an all-completed todo list stays worth
// showing after the report that completed it. It MUST agree with the
// frontend's LIVE_TODO_AUTOHIDE_MS (frontend/src/lib/stores/
// liveTodoState.svelte.ts): the frontend hides a finished list on this timer
// while a live pane is open, and this filter is what makes a refresh after
// the timer agree with the pane the user was already looking at rather than
// resurrecting the list. A list with any unfinished step never ages out —
// it is exactly the state a user came back for.
const liveTodoAutoHideMillis int64 = 5_000

// threadLiveTodo hydrates the durable todo list for one thread, applying the
// auto-hide age filter. Nil means "show nothing", which covers a thread that
// never had a list, one the provider cleared, and one whose finished list has
// aged out.
//
// A read failure (a corrupt blob is the only realistic one — the writer
// refuses everything else) is LOGGED AND SURVIVED rather than returned: the
// todo leg is auxiliary to live state, and failing the whole hydrate would
// take the active turn, the queue, and pending approvals down with it. The
// user sees no todo list; the log says why.
func (a *App) threadLiveTodo(threadID string) *LiveStateTodo {
	stored, found, err := a.store.ThreadLiveTodo(threadID)
	if err != nil {
		log.Printf("thread %s: live todo unreadable, hydrating without it: %v", threadID, err)
		return nil
	}
	// An empty list cannot be stored (SetThreadLiveTodo refuses one), but a
	// reader that trusted that would break silently if it ever were.
	if !found || len(stored.Steps) == 0 {
		return nil
	}
	if liveTodoAgedOut(stored, time.Now().UnixMilli()) {
		return nil
	}
	steps := make([]LiveStateTodoStep, 0, len(stored.Steps))
	for _, step := range stored.Steps {
		steps = append(steps, LiveStateTodoStep{
			Step:   step.Step,
			Status: step.Status,
			ID:     step.ID,
			Owner:  step.Owner,
		})
	}
	return &LiveStateTodo{
		ThreadID:  threadID,
		Steps:     steps,
		UpdatedAt: stored.UpdatedAt,
	}
}

func liveTodoAgedOut(todo store.ThreadLiveTodo, nowMillis int64) bool {
	for _, step := range todo.Steps {
		if step.Status != "completed" {
			return false
		}
	}
	// Negative age (an UpdatedAt in the future — a backward clock step
	// between the report and this read) fails closed, exactly like the
	// frontend's `age >= 0` leg in shouldHydrateLiveTodoSnapshot: the two
	// filters must agree, and a finished list under a broken clock is the
	// one to hide, not the one to pin forever.
	age := nowMillis - todo.UpdatedAt
	return age < 0 || age > liveTodoAutoHideMillis
}
