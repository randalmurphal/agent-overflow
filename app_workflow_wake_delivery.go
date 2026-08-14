package main

import (
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/workflow/engine"
)

// Wake delivery — the one place a composed message becomes a message in a
// thread, and the one place that decides whether it should.
//
// Every composer in the app (a resting run, a descendant's park, a
// `notify:`-decorated gate) hands its result here. That is deliberate: wake
// coalescing is a rule about what a READER has already been told, so it cannot
// be a check each producer makes for itself — a second producer that forgot it
// would put the duplicates straight back.

// maxWakeMessageBytes bounds one composed wake. The composer's own budgets keep
// a normal message far below this; the cap is the backstop that keeps a
// pathological run record from producing a message the queue would refuse.
const maxWakeMessageBytes = 24 * 1024

// composedWake is one message and the signature identifying the ask it carries.
// The two travel together so no delivery path can record a signature for a
// message it did not send, or send a message whose signature nobody computed.
type composedWake struct {
	signature      string
	message        string
	usageAttention *store.WorkflowProviderUsageAttentionClaim
}

// deliverWorkflowWake injects the composed message into the bound thread, unless
// the run's bound thread has already been told exactly this.
//
// The order is load-bearing. The binding is resolved first, so a run whose
// thread is gone converges on the unbound surface without recording a delivery
// that never happened. The signature is compared next, so a suppressed wake
// costs one indexed read rather than a composed message nobody wanted. The
// record is written last, after the message has stopped being losable, so a
// failed send leaves the run able to say the same thing again.
//
// The two delivery branches are the same two a human gets, and each records its
// signature at its OWN durability point:
//
//   - A live session takes the message through the flush queue. That queue is
//     process memory until the dispatch worker writes the message to the
//     provider or session-death recovery persists it in the composer, so the
//     row is CLAIMED here with a `queued:` marker and the delivered record is
//     deferred to the queue item's durability settlement. Recording the real
//     signature at register time would let a
//     crash, a session teardown, or a rollback take the message while the run
//     row swore it had been delivered — and that record suppresses the
//     identical wake FOREVER, because the coalescing rule only spends it when
//     somebody acts on the run. A run nobody is ever told about again is the
//     failure this ordering exists to prevent; the cost of getting it wrong in
//     the other direction is one duplicate message, which is the same trade the
//     guidance slot makes.
//   - A session-less thread takes an ordinary send, which lazily starts the
//     session and persists the message as a durable row before it returns — so
//     its record is written right here, where that durability already is.
func (a *App) deliverWorkflowWake(item store.WorkItem, composed composedWake) bool {
	threadID, ok := a.resolveWakeThread(item)
	if !ok {
		a.releaseWorkflowUsageAttention(composed.usageAttention)
		return false
	}
	if a.wakeAlreadyDelivered(item, composed.signature) {
		a.releaseWorkflowUsageAttention(composed.usageAttention)
		return false
	}
	if _, live := a.sessionManager().get(threadID); live {
		// Claim the row BEFORE handing the message over, with the marker that
		// says "queued, not durable" — see wakeQueuedPrefix. The claim is an
		// ordinary write because this path and the clear both run on the app's
		// serial wake queue, so they are ordered by the transitions that caused
		// them; the PROMOTION below is the one that races and is guarded.
		//
		// The closure captures ids and strings rather than the row: it outlives
		// this call by however long the thread stays busy, and the row it came
		// from carries the run's frozen workflow snapshot.
		itemID, signature := item.ID, composed.signature
		claim := wakeQueuedRecord(signature)
		a.recordWakeDelivery(itemID, claim)
		injected := injectedQueueOptions{
			preserveDraft: true,
			onDurable: func() {
				a.promoteWakeDelivery(itemID, claim, signature)
				if composed.usageAttention != nil {
					a.promoteWorkflowUsageAttention(*composed.usageAttention)
				}
			},
		}
		if _, err := a.registerQueueItem(threadID, composed.message, SendMessageOptions{}, injected); err != nil {
			a.reportWakeFailure(item, threadID, err)
			a.releaseWorkflowUsageAttention(composed.usageAttention)
			// Nothing will ever promote this claim — drop it rather than leave
			// the row marked for a message that was never queued. Guarded, so a
			// claim already superseded by a later wake is left alone.
			a.casWakeRecord(itemID, claim, "")
			return false
		}
		return true
	}
	if _, err := a.sendMessageWithOptions(threadID, composed.message, sendMessageOptions{PreserveDraft: true}); err != nil {
		a.reportWakeFailure(item, threadID, err)
		a.releaseWorkflowUsageAttention(composed.usageAttention)
		return false
	}
	a.recordWakeDelivery(item.ID, composed.signature)
	if composed.usageAttention != nil {
		a.promoteWorkflowUsageAttention(*composed.usageAttention)
	}
	return true
}

// wakeAlreadyDelivered is the coalescing rule itself: a wake whose signature
// matches the last one delivered for this run, with nothing having happened on
// the run since, says what the reader was already told.
//
// "Nothing has happened since" is not inferred here — it is recorded, by
// clearing the stored signature whenever any member of the run tree returns to
// `running`, which is what every resolve, answer, resume, retry, and rerun does
// (clearTreeWakeSignature, below). So a match here means both halves of the
// rule at once: same words, and no action between them.
//
// Suppression is LOGGED, never silent. A wake that did not arrive is
// indistinguishable from a wake that was never composed unless something says
// so, and "why did my thread not hear about this" is exactly the question this
// mechanism creates.
//
// A signature that cannot be read delivers. Going quiet on a storage error
// would suppress a message on the strength of a fact we do not have; a
// duplicate is a nuisance, a swallowed park is a run nobody knows is stuck.
func (a *App) wakeAlreadyDelivered(item store.WorkItem, signature string) bool {
	if signature == "" {
		return false
	}
	last, err := a.store.WorkItemWakeSignature(item.ID)
	if err != nil {
		log.Printf("workflow wake %s: read last delivered signature: %v; delivering rather than risking silence", item.ID, err)
		return false
	}
	if last != signature {
		return false
	}
	log.Printf("workflow wake %s: suppressed a repeat of the wake already delivered to thread %s "+
		"(nothing has happened on this run since) — signature %s",
		item.ID, item.OriginThreadID, signature)
	return true
}

// wakeQueuedPrefix marks a record whose message has been handed to the flush
// queue but has NOT yet reached a durable endpoint. It is what closes the gap
// the deferred record opens: settlement can land on the flush-dispatch worker
// or session-death recovery, while the "somebody acted" clear lands on the wake
// queue, and nothing orders them —
// so without a claim, an action taken while the message was still queued would
// find nothing to spend, the record would land after it, and the next identical
// park would be suppressed forever. That is exactly the sequence a bare `run
// resume` of a provider-retries-exhausted run produces: it continues the same attempt,
// so every field of the signature matches.
//
// The invalidation lives WHERE THE CLEAR ALREADY LIVES rather than in a second
// mechanism beside it: a claim is an ordinary value in the same column, so any
// code that spends the record — today's clearTreeWakeSignature, tomorrow's
// third clear site — invalidates a pending promotion for free, because the
// promotion is a compare-and-set against the claim it wrote.
//
// A real signature always begins `kind=rest ` or `kind=progress `
// (wake.Signature), so a claim can never be mistaken for one, and it never
// suppresses: `wakeAlreadyDelivered` compares for equality, so a queued claim
// re-delivers rather than going quiet over a message that may still be lost.
// A claim stranded by a crash is inert for the same reason and is cleared by
// the next transition on the run.
const wakeQueuedPrefix = "queued:"

func wakeQueuedRecord(signature string) string { return wakeQueuedPrefix + signature }

// promoteWakeDelivery turns a queued claim into the delivered record, and says
// so when it cannot: a claim that is gone means somebody acted on the run while
// its message was still in the queue, so the ask is live again and the next
// identical park must deliver.
func (a *App) promoteWakeDelivery(itemID, claim, signature string) {
	if a.casWakeRecord(itemID, claim, signature) {
		return
	}
	log.Printf("workflow wake %s: the queued wake record was spent before its message reached a durable endpoint "+
		"(somebody acted on the run); leaving it unrecorded so the next identical park delivers", itemID)
}

// casWakeRecord moves the record from `expected` to `next` only while the row
// still holds `expected`. Reports whether it did; a storage failure is reported
// as "did not" after being logged, because every caller's fallback for an
// unwritten record is a duplicate wake rather than a missed one.
func (a *App) casWakeRecord(itemID, expected, next string) bool {
	written, err := a.store.UpdateWorkItemWakeSignatureIfCurrent(itemID, expected, next)
	if err != nil {
		log.Printf("workflow wake %s: settle wake record: %v", itemID, err)
		return false
	}
	return written
}

// recordWakeDelivery remembers what the bound thread was just told. It takes an
// id rather than the row because one caller is a callback that outlives its
// delivery call, and the row carries the run's frozen workflow snapshot.
func (a *App) recordWakeDelivery(itemID, signature string) {
	if signature == "" {
		return
	}
	if err := a.store.UpdateWorkItemWakeSignature(itemID, signature); err != nil {
		// A lost record costs a duplicate wake next time, never a missed one, so
		// it is reported rather than propagated into the lifecycle transition
		// that triggered the delivery.
		log.Printf("workflow wake %s: record delivered signature: %v", itemID, err)
	}
}

// clearTreeWakeSignature is the other half of the coalescing rule: somebody
// acted on this run, so whatever its bound thread was last told is spent and the
// next wake delivers however familiar it looks.
//
// It clears the ROOT's record for a transition on ANY tree member, because the
// root is where wakes are delivered and recorded: a descendant that was resumed
// is precisely the action that makes its next identical park worth reporting
// again.
func (a *App) clearTreeWakeSignature(itemID string) {
	item, err := a.store.GetWorkItem(itemID)
	if err != nil {
		log.Printf("workflow wake %s: load run to clear its wake record: %v", itemID, err)
		return
	}
	root := item
	if item.ParentItemID != "" {
		chain, err := a.workflowAncestry(item)
		if err != nil {
			log.Printf("workflow wake %s: resolve root to clear its wake record: %v", itemID, err)
			return
		}
		root = chain[0]
	}
	a.clearRootWakeSignature(root)
}

// clearRootWakeSignature spends a root whose caller has already resolved the
// run tree. Keeping the read/write half here prevents a transition that also
// rearms provider attention from walking the same ancestry twice.
func (a *App) clearRootWakeSignature(root store.WorkItem) {
	if root.OriginThreadID == "" {
		// An unbound root has never had a wake delivered, so there is nothing to
		// spend — and this transition happens on every phase advance of every
		// run in the app.
		return
	}
	last, err := a.store.WorkItemWakeSignature(root.ID)
	if err != nil {
		log.Printf("workflow wake %s: read wake record to clear it: %v", root.ID, err)
		return
	}
	if last == "" {
		return
	}
	a.clearWakeRecord(root.ID)
}

// clearTreeWorkflowAttentionForTransition spends both records attached to
// action on a run: the root's content wake and every provider-usage scope
// watched by the same conversation. A called child's birth is engine progress,
// not a new action by the watcher, so it must not re-arm an unresolved outage.
// Root births remain explicit starts, while every non-birth transition to
// running is a resume, retry, answer, resolve, or rerun.
//
// Rearming is notification metadata only; no provider send ever reads it, so
// an immediate resume remains an unconditional real attempt.
func (a *App) clearTreeWorkflowAttentionForTransition(itemID string, from engine.State) {
	item, err := a.store.GetWorkItem(itemID)
	if err != nil {
		log.Printf("workflow wake %s: load run to rearm attention: %v", itemID, err)
		return
	}
	if from == "" && item.ParentItemID != "" {
		return
	}
	root := item
	if item.ParentItemID != "" {
		chain, err := a.workflowAncestry(item)
		if err != nil {
			log.Printf("workflow wake %s: resolve root to rearm attention: %v", itemID, err)
			return
		}
		root = chain[0]
	}
	a.clearRootWakeSignature(root)
	if root.OriginThreadID == "" {
		return
	}
	if err := a.store.RearmWorkflowProviderUsageAttention(root.OriginThreadID, time.Now().UnixMilli()); err != nil {
		log.Printf("workflow usage attention %s: rearm watcher %s: %v", root.ID, root.OriginThreadID, err)
	}
}

// clearWakeRecord spends a run's wake record: whatever its bound thread was
// last told no longer stands. It is the ONE write that does this, and every
// clear site must route through it — an unconditional write is what makes a
// queued claim's compare-and-set fail, so a message still sitting in the queue
// when somebody acts is never recorded as delivered.
func (a *App) clearWakeRecord(itemID string) {
	if err := a.store.UpdateWorkItemWakeSignature(itemID, ""); err != nil {
		log.Printf("workflow wake %s: clear wake record: %v", itemID, err)
	}
}

// resolveWakeThread validates the binding and reports whether it can carry a
// wake. A binding that no longer resolves is cleared here — loudly — so the run
// converges on the unbound surface instead of retrying a dead thread on every
// future transition.
func (a *App) resolveWakeThread(item store.WorkItem) (string, bool) {
	thread, err := a.store.GetThread(item.OriginThreadID)
	if err != nil {
		a.clearStaleWakeBinding(item, fmt.Sprintf("bound thread %s could not be loaded: %v", item.OriginThreadID, err))
		return "", false
	}
	if thread.Archived {
		a.clearStaleWakeBinding(item, fmt.Sprintf("bound thread %s is archived", thread.ID))
		return "", false
	}
	if err := validWorkflowBindingThread(item, thread); err != nil {
		a.clearStaleWakeBinding(item, err.Error())
		return "", false
	}
	return thread.ID, true
}

func (a *App) clearStaleWakeBinding(item store.WorkItem, reason string) {
	log.Printf("workflow wake %s: %s; falling back to the unbound surface", item.ID, reason)
	if err := a.store.UpdateWorkItemOriginThread(item.ID, ""); err != nil {
		log.Printf("workflow wake %s: clear stale binding: %v", item.ID, err)
	}
	// The record describes what a thread that no longer exists was told, so it
	// is spent — and clearTreeWakeSignature deliberately skips unbound roots
	// (that transition fires on every phase advance of every run in the app),
	// which would otherwise strand a queued claim here with nothing left to
	// invalidate it.
	a.clearWakeRecord(item.ID)
	a.emit("workflow:error", engine.ErrorEvent{
		ItemID: item.ID,
		Error:  "this run's bound thread is gone; its results now surface in the workflows overlay",
	})
}

func (a *App) reportWakeFailure(item store.WorkItem, threadID string, cause error) {
	log.Printf("workflow wake %s: deliver to thread %s: %v", item.ID, threadID, cause)
	a.emit("workflow:error", engine.ErrorEvent{
		ItemID: item.ID,
		Error:  "this run's result could not be delivered to its bound thread; open the run in the workflows overlay",
	})
}

// validWorkflowBindingThread is the one rule set both binding and waking apply,
// so a thread that could never be woken can never be bound in the first place.
//
// Workflow-owned threads (phase, unit, studio, triage) are excluded because
// they are driven by the run machinery itself: waking one would inject a user
// turn into a session the engine is steering. Terminal and discussion threads
// are excluded because neither takes an ordinary user message.
func validWorkflowBindingThread(item store.WorkItem, thread store.Thread) error {
	if thread.ProjectID != item.ProjectID {
		return fmt.Errorf("thread %s belongs to project %s, not to this run's project %s",
			thread.ID, thread.ProjectID, item.ProjectID)
	}
	if _, ok := threadmode.ManualSelectionModes[thread.Mode]; !ok {
		return fmt.Errorf("thread %s has mode %q; a run binds a conversation thread (chat, plan, or design)",
			thread.ID, thread.Mode)
	}
	return nil
}
