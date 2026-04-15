package domain

import "time"

// EventKind identifies the type of domain event.
type EventKind string

const (
	// Thread lifecycle
	ThreadCreated  EventKind = "thread.created"
	ThreadDeleted  EventKind = "thread.deleted"
	ThreadArchived EventKind = "thread.archived"
	ThreadRenamed  EventKind = "thread.renamed"

	// Messages
	MessageSent EventKind = "thread.message-sent"

	// Turns
	TurnStartRequested     EventKind = "thread.turn-start-requested"
	TurnInterruptRequested EventKind = "thread.turn-interrupt-requested"
	TurnCompleted          EventKind = "thread.turn-completed"

	// Session
	SessionSet          EventKind = "thread.session-set"
	SessionStopRequested EventKind = "thread.session-stop-requested"

	// Activity
	ActivityAppended EventKind = "thread.activity-appended"

	// Diffs
	TurnDiffCompleted EventKind = "thread.turn-diff-completed"
)

// Event is a persisted domain event.
type Event struct {
	Sequence   uint64    `json:"sequence"`
	EventID    string    `json:"eventId"`
	Kind       EventKind `json:"kind"`
	ThreadID   string    `json:"threadId"`
	OccurredAt time.Time `json:"occurredAt"`
	Payload    any       `json:"payload"`
}
