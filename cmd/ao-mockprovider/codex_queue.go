package main

import (
	"encoding/json"
	"fmt"
	"log"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
)

// Provider-owned user-message queue (`thread/queue/*`, codex >= 0.148).
//
// The point of mocking this family is that its dispatch is AUTOMATIC. Upstream
// installs `QueuedItemService` as a thread-lifecycle contributor and drains the
// head from `on_thread_idle` (codex-rs/ext/queue/src/service.rs), so a client
// that also dispatched from a queue of its own would send every message twice.
// A mock that only answered the RPCs could not show that: the harness has
// to watch the mock start a turn nobody asked for.
//
// So the queue here is drained from the engine's idle hook, exactly once per
// entry, and `thread/queue/start` is answered with an ERROR — see
// dispatchQueuedOnIdle and the start case below.
//
// Only the three methods AO calls are implemented (`add` / `list` /
// `delete`); `update`, `reorder` and `start` all answer method-not-found, so a
// harness run that grows a caller for one of them fails here.

// codexQueueEntry is one queued user message, in wire order.
type codexQueueEntry struct {
	id       string
	clientID string // clientUserMessageId the app correlated it with
	text     string
}

// queueSubmissionJSON is the `QueuedSubmission` shape the app decodes
// (`internal/provider/codex/thread_queue.go#parseQueuedSubmission`): an id, the
// input vec, and the client id echoed back.
func (q codexQueueEntry) queueSubmissionJSON() map[string]any {
	return map[string]any{
		"id":                  q.id,
		"input":               []any{map[string]any{"type": "text", "text": q.text}},
		"clientUserMessageId": q.clientID,
	}
}

// handleQueueRequest answers one `thread/queue/*` request. Returns false when
// the method is not part of the family, so the caller's switch can fall
// through to its default.
func (a *codexAdapter) handleQueueRequest(id json.RawMessage, method string, params json.RawMessage) bool {
	switch method {
	case "thread/queue/add":
		a.queueAdd(id, params)
	case "thread/queue/list":
		a.queueList(id)
	case "thread/queue/delete":
		a.queueDelete(id, params)
	case "thread/queue/update", "thread/queue/reorder":
		// Answered as unknown on purpose. Both exist upstream, and neither has
		// an AO caller: the composer cannot edit or re-order a message once the
		// provider owns it, so a wrapper for either would be dead code whose
		// wire shape nothing verifies. A harness run that reaches one of these
		// should fail here rather than pass against a mock that is more
		// permissive than the app.
		log.Printf("codex: %s called — Agent Overflow has no caller for it", method)
		a.writeRPCError(id, -32601,
			method+": mock refuses — Agent Overflow does not edit or re-order provider-queued messages")
	case "thread/queue/start":
		// A tripwire, not an omission. Dispatch is automatic; a client that
		// also calls start races the drain and can double-send. AO must never
		// call it, and a harness run that does should fail loudly here rather
		// than pass with a duplicated turn.
		log.Printf("codex: thread/queue/start called — dispatch is automatic; the client must not ask")
		a.writeRPCError(id, -32601,
			"thread/queue/start: mock refuses — queued submissions dispatch automatically on idle")
	default:
		return false
	}
	return true
}

func (a *codexAdapter) queueAdd(id json.RawMessage, params json.RawMessage) {
	entry := codexQueueEntry{
		clientID: readParamString(params, "clientUserMessageId"),
		text:     codexInputText(params),
	}
	a.mu.Lock()
	a.queueSeq++
	entry.id = fmt.Sprintf("queue-sub-%d", a.queueSeq)
	a.queue = append(a.queue, entry)
	a.mu.Unlock()

	a.respondJSON(id, map[string]any{"queuedSubmission": entry.queueSubmissionJSON()})
	// Upstream's `enqueue` notifies AFTER the row lands, and does so for the
	// adding client too — which is the whole reason the app has to tell its own
	// adds apart from a `codex queue` write it never made.
	a.emitQueueChanged()
}

func (a *codexAdapter) queueList(id json.RawMessage) {
	a.mu.Lock()
	data := make([]any, 0, len(a.queue))
	for _, entry := range a.queue {
		data = append(data, entry.queueSubmissionJSON())
	}
	a.mu.Unlock()
	// One page: the mock never holds enough to paginate, and `nextCursor: null`
	// is the terminating shape the app's pager stops on.
	a.respondJSON(id, map[string]any{"data": data, "nextCursor": nil})
}

func (a *codexAdapter) queueDelete(id json.RawMessage, params json.RawMessage) {
	target := readParamString(params, "queuedSubmissionId")
	a.mu.Lock()
	deleted := false
	kept := a.queue[:0]
	for _, entry := range a.queue {
		if entry.id == target && !deleted {
			deleted = true
			continue
		}
		kept = append(kept, entry)
	}
	a.queue = kept
	a.mu.Unlock()
	// `deleted: false` is a STATE (matched nothing), not an error — same as the
	// background-terminal terminate answer.
	a.respondJSON(id, map[string]any{"deleted": deleted})
	if deleted {
		a.emitQueueChanged()
	}
}

// dispatchQueuedOnIdle drains one queued submission the way upstream's
// `on_thread_idle` hook does: pop the head, delete it (which is itself a queue
// change upstream), then start a turn the client never requested.
//
// Exactly one entry per idle edge. The turn it starts ends with another idle
// edge, so a backlog drains one turn at a time in FIFO order — which is the
// property the harness scenario asserts.
func (a *codexAdapter) dispatchQueuedOnIdle() {
	a.mu.Lock()
	if len(a.queue) == 0 {
		a.mu.Unlock()
		return
	}
	head := a.queue[0]
	a.queue = a.queue[1:]
	a.mu.Unlock()

	// Upstream's dispatch deletes the row under the lock and the delete emits
	// the change, so a client watching the queue sees it shrink before the turn
	// starts.
	a.emitQueueChanged()

	n, vars := a.e.beginTurn()
	// The dispatched text exists only here — no scenario file could know it —
	// so bind it for the turn's steps to echo as the userMessage the app's
	// pending-send FIFO is waiting for.
	a.e.setTurnVars(n, scenario.Vars{
		"USER_INPUT":      head.text,
		"QUEUE_CLIENT_ID": head.clientID,
	})
	a.e.rep.report(control.Report{
		Kind: control.ReportUserInput, Turn: n,
		Input: head.text, SessionRef: vars["THREAD_ID"],
	})
	// No `turn/start` response to write: nobody asked. The turn's scenario
	// steps carry `turn/started` themselves, exactly as they do for a
	// client-initiated turn.
	a.e.enqueueTurn(n)
}

// emitQueueChanged writes the notification upstream raises on every queue
// mutation. `{threadId}` and nothing else at rust-v0.149.0 — no count, no ids —
// which is why a client that wants depth has to call list.
func (a *codexAdapter) emitQueueChanged() {
	a.w.writeLine(mustJSON(map[string]any{
		"jsonrpc": "2.0",
		"method":  "thread/queue/changed",
		"params":  map[string]any{"threadId": a.e.currentVars()["THREAD_ID"]},
	}), 0, 0)
}

// respondJSON answers a request with a marshalled result, for the queue family
// whose replies carry live state rather than a scenario template.
func (a *codexAdapter) respondJSON(id json.RawMessage, result any) {
	a.w.writeLine(mustJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}), 0, 0)
}
