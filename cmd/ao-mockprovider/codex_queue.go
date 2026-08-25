package main

import (
	"encoding/json"
	"log"
)

// Provider-owned user-message queue (`thread/queue/*`, codex >= 0.148).
//
// **Two methods here are tripwires, and that is the file's main job.**
// Upstream installs `QueuedItemService` as a thread-lifecycle contributor and
// drains the head from `on_thread_idle` (codex-rs/ext/queue/src/service.rs):
// dispatch is AUTOMATIC and on the app-server's clock. A client that also
// keeps a queue of its own — which Agent Overflow does — would then have two
// dispatchers for one message. So AO sends every mid-turn message with
// `turn/steer` and must never call `thread/queue/add`, and it must never call
// `thread/queue/start` either (that races the automatic drain). Both answer
// with an ERROR here, so a harness run that regrows either caller fails loudly
// instead of passing with a duplicated turn.
//
// `list` and `delete` DO answer, because AO still calls them: a rollback has
// to purge rows a foreign producer left behind, and a session start retires
// rows an older AO build added during the 2026-08-21..24 window. Nothing in
// this mock can put a row in the queue, so both answer over an empty one —
// which is exactly the shape a harness thread is in.
//
// `update` and `reorder` answer method-not-found, same as before: they exist
// upstream and have no AO caller.

// codexQueueEntry is one queued user message, in wire order. Nothing in the
// mock creates one — `add` is refused — but `list` and `delete` are written
// against the real shape rather than a hardcoded empty answer, so a future
// foreign-producer fixture can seed the slice without rewriting them.
type codexQueueEntry struct {
	id       string
	clientID string // clientUserMessageId the producer correlated it with
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
		// A tripwire, not an omission. A message handed to this queue is
		// dispatched by the app-server itself, so an app that also owns a
		// queue can send it twice. AO reverted to `turn/steer` for every
		// mid-turn message and must never come back here.
		log.Printf("codex: thread/queue/add called — Agent Overflow dispatches mid-turn messages with turn/steer and must never queue them")
		a.writeRPCError(id, -32601,
			"thread/queue/add: mock refuses — Agent Overflow must dispatch mid-turn messages with turn/steer")
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
		// The second tripwire. Dispatch is automatic; a client that also calls
		// start races the drain and can double-send.
		log.Printf("codex: thread/queue/start called — dispatch is automatic; the client must not ask")
		a.writeRPCError(id, -32601,
			"thread/queue/start: mock refuses — queued submissions dispatch automatically on idle")
	default:
		return false
	}
	return true
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
