package main

import (
	"fmt"
	"log"

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
	signature string
	message   string
}

// deliverWorkflowWake injects the composed message into the bound thread, unless
// the run's bound thread has already been told exactly this.
//
// The order is load-bearing. The binding is resolved first, so a run whose
// thread is gone converges on the unbound surface without recording a delivery
// that never happened. The signature is compared next, so a suppressed wake
// costs one indexed read rather than a composed message nobody wanted. The
// record is written last, after the delivery seam accepted the message, so a
// failed send leaves the run able to say the same thing again.
//
// The two delivery branches are the same two a human gets: a live session takes
// the message through the flush queue (delivered at the provider's next tool
// boundary, or straight through when nothing is in flight), and a session-less
// thread takes an ordinary send, which lazily starts the session and persists
// the message as a durable row rather than parking it in a queue this process
// would lose.
func (a *App) deliverWorkflowWake(item store.WorkItem, composed composedWake) {
	threadID, ok := a.resolveWakeThread(item)
	if !ok {
		return
	}
	if a.wakeAlreadyDelivered(item, composed.signature) {
		return
	}
	if _, live := a.sessionManager().get(threadID); live {
		if _, err := a.registerQueueItem(threadID, composed.message, SendMessageOptions{}, true); err != nil {
			a.reportWakeFailure(item, threadID, err)
			return
		}
	} else if _, err := a.sendMessageWithOptions(threadID, composed.message, sendMessageOptions{PreserveDraft: true}); err != nil {
		a.reportWakeFailure(item, threadID, err)
		return
	}
	a.recordWakeDelivery(item, composed.signature)
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

// recordWakeDelivery remembers what the bound thread was just told.
func (a *App) recordWakeDelivery(item store.WorkItem, signature string) {
	if signature == "" {
		return
	}
	if err := a.store.UpdateWorkItemWakeSignature(item.ID, signature); err != nil {
		// A lost record costs a duplicate wake next time, never a missed one, so
		// it is reported rather than propagated into the lifecycle transition
		// that triggered the delivery.
		log.Printf("workflow wake %s: record delivered signature: %v", item.ID, err)
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
	if err := a.store.UpdateWorkItemWakeSignature(root.ID, ""); err != nil {
		log.Printf("workflow wake %s: clear wake record: %v", root.ID, err)
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
