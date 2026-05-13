// Package flushqueue owns the wire-side queue projection: the QueuedItem
// shape the frontend mirrors, its inner Payload JSON, the
// triage.QueuedFlushItem → QueuedItem projector, and the queue-item id
// allocator. Pure logic only — the App-bound register/dispatch/undo
// sagas stay in main.
package flushqueue
