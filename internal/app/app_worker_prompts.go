package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/untrustedtext"
	"agent-overflow/internal/usermessage"
)

// A worker's question goes to the thread that spawned it (standing agents,
// ruling 19), whoever that is, while it works in a run that thread started;
// in any other run it is the person's (store.SpawnOwnerOf, the one place
// that decides, for approvals, questions and async questions alike). A
// thread another thread on this computer spawned does not stop at the user
// while its spawner is not archived and its run is open:
// triage stamps each of its approval and user-input prompts with the
// spawner (routeWorkerPrompt), the emit tap queues the prompt to the
// spawner as a badged wake, and the spawner's agent answers it with
// thread_reply on the token the wake carries. The prompt stays open in the
// worker, so the user can still answer it there, which settles the wake.
//
// Triage's RoutedTo is the one record of where an open prompt goes, and the
// registry follows its announcements: a prompt announced routed is
// registered under a fresh token and its wake queued, which retires the
// prompt's earlier registration and withdraws that wake; one announced as
// the user's retires it. A prompt its agent could not take (its wake never
// reached the session, or its queue was cleared) is rejected
// (triage.RejectPrompt): it goes where the worker's prompts route now, or
// to the user when that is the same agent. A handoff, an archive or a
// delete reconciles the prompts routed to the thread it changed
// (reroutePromptsFrom, triage.ReroutePrompts): a route that still holds is
// kept, and the others go to the assistant's next thread, or the user.
//
// A registration names one prompt occurrence (triage.PromptRef): a session
// can ask again under a request id it resolved, and a replacement session
// under its predecessor's, and an earlier occurrence's token answers,
// settles and releases nothing of a later one.
//
// The record of a routed prompt lives in memory only: a provider prompt
// lives no longer than the worker's session, and none survives a restart.
// A Codex worker's async questions are its prompts too, and outlive both;
// app_worker_prompts_async.go keeps where each goes and routes them again
// at boot.
//
// A worker a paired computer's spawn owns (store.SpawnOwnerOf, the spawn's
// link) routes its prompts to the spawn's thread there. Nothing is queued
// here for such a prompt: it is registered held for that computer, which
// picks it up on its own agent_rows calls and answers it through
// agent_prompt_reply (app_worker_prompts_peer.go). The computer that holds
// the spawner keeps its side in app_worker_prompts_remote.go.

// workerPromptSendIDRoot names the wake that carries a worker's prompt; the
// token completes it. It is not a loop or watch wake's root, so the turn it
// starts is not a tick and notifies like any other.
const workerPromptSendIDRoot = "agent-ask:"

// workerAnswerSendIDRoot names the message that carries an agent's answer
// to its worker's async questions; the prompt's token completes it.
const workerAnswerSendIDRoot = "agent-answer:"

// routeWorkerPrompt is triage's prompt router: the thread that owns
// threadID with a run of its open there (store.SpawnOwnerOf), here or on
// the paired computer whose spawn's link owns it, or nil when the prompt is
// the user's.
func (a *App) routeWorkerPrompt(threadID string) *provider.PromptRoute {
	owner, found, err := a.store.SpawnOwnerOf(threadID)
	if err != nil {
		log.Printf("worker prompts: %v", err)
		return nil
	}
	if !found {
		return nil
	}
	return &provider.PromptRoute{ThreadID: owner.ThreadID, Title: owner.Title, ComputerID: owner.ComputerID, ComputerName: owner.ComputerName}
}

// tapWorkerPrompt is the emit tap: a prompt announced routed is delivered
// to its agent, and a prompt announced as the user's or resolved settles
// its routed registration. A change to a thread's async questions
// reconciles its async question prompts, and so does every thread frame but
// a patch: a thread made, copied, moved here or forked, an archive or an
// unarchive, a delete, a placement, a handoff; and so does a transfer that
// moves a thread away. Such a frame also routes the thread's open prompts
// again (routeWorkerPromptsAgain), and a turn's start puts to the thread
// the prompts parked for it (redeliverParkedPrompts).
func (a *App) tapWorkerPrompt(name eventchan.Channel, data any) {
	if event, ok := data.(triage.ThreadUpdateEvent); ok {
		if event.Action != triage.ThreadActionPatch {
			id := event.ID
			if event.Thread != nil {
				id = event.Thread.ID
			}
			a.syncAsyncQuestionPrompts(id)
			if id != "" && !a.shuttingDown.Load() {
				a.workerPrompts.run(func() { a.routeWorkerPromptsAgain(id) })
			}
		}
		return
	}
	switch name {
	case eventchan.ProviderTurnStarted:
		event, ok := data.(triage.TurnStartedEvent)
		if !ok {
			return
		}
		if a.workerPrompts.hasParked(event.ThreadID) {
			a.workerPrompts.run(func() { a.redeliverParkedPrompts(event.ThreadID) })
		}
		if a.workerPrompts.remote.hasParked(event.ThreadID) {
			a.workerPrompts.run(func() { a.redeliverParkedRemotePrompts(event.ThreadID) })
		}
		return
	case eventchan.ProviderAsyncQuestionsChanged:
		if event, ok := data.(map[string]string); ok {
			a.syncAsyncQuestionPrompts(event["threadId"])
		}
		return
	case eventchan.UserMessageReverted:
		if event, ok := data.(UserMessageRevertedEvent); ok {
			a.syncAsyncQuestionPrompts(event.ThreadID)
		}
		return
	case eventchan.ThreadTransfer:
		// A move takes the thread from this computer once it commits, with
		// no thread frame here.
		if row, ok := data.(store.ThreadTransfer); ok && row.Direction == "outgoing" && row.Kind == "move" {
			a.syncAsyncQuestionPrompts(row.ThreadID)
		}
		return
	}
	var prompt workerPrompt
	var route *provider.PromptRoute
	var action string
	switch name {
	case eventchan.ProviderApproval:
		event, ok := data.(provider.ApprovalEvent)
		if !ok {
			return
		}
		action, prompt = event.Action, workerPrompt{workerID: event.ThreadID, requestID: event.RequestID, occurrence: event.Occurrence}
		if request := event.Request; request != nil {
			copied := *request
			route, prompt.approval = copied.RoutedTo, &copied
			prompt.workerID, prompt.requestID, prompt.occurrence = copied.ThreadID, copied.RequestID, copied.Occurrence
		}
	case eventchan.ProviderUserInput:
		event, ok := data.(provider.UserInputEvent)
		if !ok {
			return
		}
		action, prompt = event.Action, workerPrompt{workerID: event.ThreadID, requestID: event.RequestID, occurrence: event.Occurrence}
		if request := event.Request; request != nil {
			copied := *request
			route, prompt.question = copied.RoutedTo, &copied
			prompt.workerID, prompt.requestID, prompt.occurrence = copied.ThreadID, copied.RequestID, copied.Occurrence
		}
	default:
		return
	}
	switch {
	case action == "request" && route != nil:
		to := *route
		a.workerPrompts.run(func() { a.deliverWorkerPrompt(to, prompt) })
	case action == "request" && (prompt.approval != nil || prompt.question != nil), action == "resolve":
		a.workerPrompts.run(func() { a.settleWorkerPrompt(prompt.workerID, prompt.requestID, prompt.occurrence) })
	}
}

// deliverWorkerPrompt registers a prompt announced routed to route and
// queues its wake there. An announcement a later one overtook (the prompt
// was answered, or moved again) is left to that one; a prompt the agent
// cannot take moves on (handBackWorkerPrompt). A prompt routed to a paired
// computer is registered held for it (holdPeerWorkerPrompt).
func (a *App) deliverWorkerPrompt(route provider.PromptRoute, prompt workerPrompt) {
	if current, open := a.workerPromptRoute(prompt); !open || current == nil || current.ThreadID != route.ThreadID {
		return
	}
	if route.ComputerID != "" {
		a.holdPeerWorkerPrompt(route, prompt)
		return
	}
	prompt.agentID = route.ThreadID
	registered, replaced := a.workerPrompts.add(prompt, a.workerPromptOpen)
	if replaced != nil {
		a.withdrawWorkerPromptWake(*replaced)
	}
	worker, err := a.store.GetThread(prompt.workerID)
	if err != nil {
		log.Printf("worker prompts: read worker %s: %v", prompt.workerID, err)
		a.handBackWorkerPrompt(registered.token)
		return
	}
	message, err := workerPromptMessage(usermessage.Reporter{Label: worker.Title, ThreadID: prompt.workerID}, &registered)
	if err != nil {
		log.Printf("worker prompts: put %s to %s: %v", prompt.workerID, route.ThreadID, err)
		a.handBackWorkerPrompt(registered.token)
		return
	}
	live := func() bool { return a.workerPromptLive(registered.token) }
	// A thread New Thread, Same Assistant superseded after triage routed
	// the prompt there takes nothing, so a wake is queued only where triage
	// says the prompt is: the handoff moves the prompt on
	// (reroutePromptsFrom), and that announcement delivers it.
	moved := false
	// The user may have answered before the prompt is queued and again
	// before a session is started for it. An archive of the agent is the
	// queue's and the start's to hold (queueAgentNotice).
	err = a.queueAgentNotice(agentNotice{
		threadID:  route.ThreadID,
		recipient: func(threadID string) { moved = threadID != route.ThreadID },
		sendID:    registered.sendID(),
		prepare: func() (usermessage.AgentMessage, bool, error) {
			return message, !moved && live(), nil
		},
		keep:      func() (bool, error) { return live(), nil },
		withdrawn: func() { a.handBackWorkerPrompt(registered.token) },
		startFailed: func(err error) string {
			return fmt.Sprintf("Thread %q is waiting on your answer and its prompt is queued here, but this thread's agent could not start: %v", worker.Title, err)
		},
	})
	if err != nil {
		log.Printf("worker prompts: deliver %s of %s to %s: %v", prompt.requestID, prompt.workerID, route.ThreadID, err)
		a.handBackWorkerPrompt(registered.token)
	}
}

// workerPromptRoute is where a registration's prompt is routed now, nil
// when it is the user's, and whether it is still open: in the worker's
// session that asked it, or for an async question, in the store.
func (a *App) workerPromptRoute(prompt workerPrompt) (*provider.PromptRoute, bool) {
	if prompt.async {
		return a.asyncQuestionRoute(prompt.ref())
	}
	if a.triage == nil {
		return nil, false
	}
	return a.triage.PromptRoute(prompt.ref())
}

// workerPromptOpen reports whether a registration's prompt is still open.
func (a *App) workerPromptOpen(prompt workerPrompt) bool {
	_, open := a.workerPromptRoute(prompt)
	return open
}

// workerPromptLive reports whether the registration under token is routed
// and its prompt is open and still routed to its agent.
func (a *App) workerPromptLive(token string) bool {
	prompt, found := a.workerPrompts.get(token)
	return found && prompt.state == workerPromptRouted && a.workerPromptOpenTo(prompt)
}

// workerPromptOpenTo reports whether a registration's prompt is open and
// still routed to the registration's agent.
func (a *App) workerPromptOpenTo(prompt workerPrompt) bool {
	route, open := a.workerPromptRoute(prompt)
	return open && route != nil && route.ThreadID == prompt.agentID
}

// settleWorkerPrompt records that the prompt occurrence names was resolved,
// or was announced as the user's. Unless the agent's own answer resolved
// it, the user answered it in the worker, the worker stopped waiting or the
// prompt went to the user, and a wake still queued is withdrawn.
func (a *App) settleWorkerPrompt(workerID, requestID string, occurrence provider.PromptOccurrence) {
	prompt, settled := a.workerPrompts.settle(workerID, requestID, occurrence)
	if !settled {
		return
	}
	a.withdrawWorkerPromptWake(prompt)
}

// settleEndedWorkerPrompts closes the prompts a worker's session took with
// it when it ended, which no resolution settles, and withdraws their wakes
// still queued for the agent, so the agent is not asked what nobody waits
// on. Its async questions are not its session's and stay open. It runs on
// the prompts' serial queue, after any delivery announced before the end.
func (a *App) settleEndedWorkerPrompts(workerID string) {
	a.workerPrompts.run(func() {
		for _, prompt := range a.workerPrompts.settleWorker(workerID) {
			a.withdrawWorkerPromptWake(prompt)
		}
	})
}

// withdrawWorkerPromptWake takes a closed registration's wake out of its
// agent's queue, when it is still there. A prompt held for a paired
// computer has no wake here: that computer withdraws its own once this one
// stops listing the prompt.
func (a *App) withdrawWorkerPromptWake(prompt workerPrompt) {
	if prompt.remote != nil {
		return
	}
	if _, err := a.removeQueuedItem(a.lifeCtx(), prompt.agentID, prompt.sendID()); err != nil {
		log.Printf("worker prompts: withdraw the wake for %s of %s: %v", prompt.requestID, prompt.workerID, err)
	}
}

// handBackWorkerPrompt moves on the prompt registered under token, which its
// agent could not take: an owned worker's stays its agent's while its
// prompts route there (parkWorkerPrompt); any other goes to where the
// worker's prompts route now, the assistant's current thread when New
// Thread, Same Assistant superseded the agent's, and otherwise the user.
// Only that registration is settled: one that is already closed, or that a
// later registration of the same prompt replaced, moves nothing.
func (a *App) handBackWorkerPrompt(token string) {
	prompt, settled := a.workerPrompts.settleToken(token)
	if !settled || a.parkWorkerPrompt(prompt) {
		return
	}
	moved := false
	switch {
	case prompt.async:
		moved = a.rerouteAsyncQuestions(prompt.agentID, []triage.PromptRef{prompt.ref()}, true) > 0
	case a.triage != nil:
		moved = a.triage.RejectPrompt(prompt.agentID, prompt.ref())
	}
	if moved {
		log.Printf("worker prompts: %s of %s moved on: %s could not take it", prompt.requestID, prompt.workerID, prompt.agentID)
	}
}

// parkWorkerPrompt keeps the prompt of an owned worker that its agent did
// not take routed to that agent, while the worker's prompts route there:
// the worker reads as waiting on its spawner, nothing is started or woken
// for it, and the agent's next turn puts it to the agent again
// (redeliverParkedPrompts). It reports whether it parked the prompt.
func (a *App) parkWorkerPrompt(prompt workerPrompt) bool {
	if prompt.remote != nil {
		return false
	}
	if route, open := a.workerPromptRoute(prompt); !open || route == nil || route.ThreadID != prompt.agentID || route.ComputerID != "" {
		return false
	}
	worker, err := a.store.GetThread(prompt.workerID)
	if err != nil {
		log.Printf("worker prompts: read worker %s: %v", prompt.workerID, err)
		return false
	}
	if worker.Placement != store.PlacementOwned {
		return false
	}
	if now := a.routeWorkerPrompt(prompt.workerID); now == nil || now.ThreadID != prompt.agentID || now.ComputerID != "" {
		return false
	}
	a.workerPrompts.park(prompt, a.workerPromptOpen)
	log.Printf("worker prompts: %s of %s waits for %s's next turn", prompt.requestID, prompt.workerID, prompt.agentID)
	return true
}

// redeliverParkedPrompts puts agentID's parked prompts to it again, each
// still open and routed to it, under a fresh registration and wake. It runs
// on the prompts' serial queue when agentID's turn starts.
func (a *App) redeliverParkedPrompts(agentID string) {
	for _, prompt := range a.workerPrompts.takeParked(agentID) {
		route, open := a.workerPromptRoute(prompt)
		if !open || route == nil || route.ThreadID != agentID || route.ComputerID != "" {
			continue
		}
		prompt.state, prompt.closedAt, prompt.answer, prompt.handBack = workerPromptRouted, time.Time{}, "", false
		a.deliverWorkerPrompt(*route, prompt)
	}
}

// routeWorkerPromptsAgain reconciles workerID's open prompts with where its
// prompts route now, after a change of its placement: Show in Sidebar gives
// a prompt asked outside its spawner's runs to the person, and Remove from
// Sidebar puts the person's to the spawner. A prompt of a worker with a
// sidebar row that is the person's stays the person's, one being answered
// is left to its answer's end, and a worker being deleted keeps its
// prompts until they end with it. A spawner on a paired computer is
// one by the same rule: its hold there moves with the announcement
// (deliverWorkerPrompt, settleWorkerPrompt). A worker asking nothing costs
// no store read.
func (a *App) routeWorkerPromptsAgain(workerID string) {
	if !a.workerAsks(workerID) {
		return
	}
	worker, err := a.store.GetThread(workerID)
	if err != nil {
		// Its prompts stay where they are until its next thread frame.
		log.Printf("worker prompts: read %s to route its prompts for its placement: %v", workerID, err)
		return
	}
	// A worker a delete claimed takes no placement move, and its prompts end
	// with it: its spawn reads as gone meanwhile, which routes nowhere.
	claim, committed, err := a.store.ThreadDeleteClaim(workerID)
	if err != nil {
		log.Printf("worker prompts: read the delete of %s to route its prompts for its placement: %v", workerID, err)
		return
	}
	if claim != "" || committed {
		return
	}
	owned := worker.Placement == store.PlacementOwned
	moved := a.routeAsyncQuestionsAgain(workerID, owned)
	if a.triage != nil {
		moved += a.triage.RouteThreadPrompts(workerID, owned, a.workerPromptAnswering)
	}
	if moved > 0 {
		log.Printf("worker prompts: %d prompts of %s moved with its placement", moved, workerID)
	}
}

// workerPromptAnswering reports whether the agent's answer to the prompt
// ref names is being written: every transition but its own end skips it
// (endPromptAnswer).
func (a *App) workerPromptAnswering(ref triage.PromptRef) bool {
	w := &a.workerPrompts
	w.mu.Lock()
	defer w.mu.Unlock()
	prompt := w.byToken[w.current[workerPromptKey{workerID: ref.ThreadID, requestID: ref.RequestID}]]
	return prompt != nil && prompt.state == workerPromptAnswering && prompt.occurrence == ref.Occurrence
}

// workerAsks reports whether workerID has an open prompt or async
// question prompt.
func (a *App) workerAsks(workerID string) bool {
	if a.triage != nil {
		if pending := a.triage.PendingInteractiveRequests(workerID); len(pending.Approvals)+len(pending.UserInputs) > 0 {
			return true
		}
	}
	routes := &a.workerPrompts.async
	routes.mu.Lock()
	defer routes.mu.Unlock()
	for key := range routes.open {
		if key.workerID == workerID {
			return true
		}
	}
	return false
}

// reroutePromptsFrom reconciles every open prompt routed to threadID, which
// a handoff, an archive or a delete changed, whatever became of its wake: a
// prompt the worker's prompts still route to threadID stays, as one does
// when an unarchive reopened it before a delayed archive cleanup ran, and
// the others go to the assistant's next thread, or to the user when the
// worker's prompts route to nobody now.
func (a *App) reroutePromptsFrom(threadID string) {
	a.rerouteRemotePromptsFrom(threadID)
	moved := a.rerouteAsyncQuestions(threadID, nil, false)
	if a.triage != nil {
		moved += a.triage.ReroutePrompts(threadID)
	}
	if moved > 0 {
		log.Printf("worker prompts: %d prompts routed to %s moved on", moved, threadID)
	}
}

// handBackWorkerPromptWake hands back the prompt a dropped wake carried:
// one a worker here asked, or one a paired computer's worker asked
// (handBackRemotePromptWake). False when sendID is not a worker prompt's
// wake. Every consumer of a dropped, withdrawn or untaken message comes
// here, so this is the one reader of the wake's send id.
func (a *App) handBackWorkerPromptWake(sendID string) bool {
	wake, ok := parseWorkerPromptWake(sendID)
	switch {
	case !ok:
		return false
	case wake.delivery != "":
		a.handBackRemotePromptWake(wake)
	default:
		a.handBackWorkerPrompt(wake.token)
	}
	return true
}

// workerPromptMessage is the wake that puts a worker's prompt to its agent:
// what the worker asks, quoted as data, and how to answer, headed in its
// row by the prompt's first line. from names the worker: its title, its
// thread and, for a worker on a paired computer, that computer.
func workerPromptMessage(from usermessage.Reporter, prompt *workerPrompt) (usermessage.AgentMessage, error) {
	kind := agentmeta.ReportQuestion
	if prompt.approval != nil {
		kind = agentmeta.ReportApproval
	}
	where := ""
	if from.Computer != "" {
		where = " on " + untrustedtext.Quote(from.Computer, 0)
	}
	notice := fmt.Sprintf("Thread %s (%s)%s, which you spawned, is waiting on your answer. %s",
		untrustedtext.Quote(from.Label, 0), from.ThreadID, where, agentWakeDataNotice)
	var out strings.Builder
	var data strings.Builder
	if prompt.approval != nil {
		request := prompt.approval
		fmt.Fprintf(&data, "It asks for approval to use %s.\n", request.ToolName)
		if request.Description != "" {
			fmt.Fprintf(&data, "%s\n", request.Description)
		}
		if len(request.Input) > 0 {
			fmt.Fprintf(&data, "input: %s\n", request.Input)
		}
	} else {
		for i, q := range prompt.question.Questions {
			if q.Header == "" {
				fmt.Fprintf(&data, "%d. %s\n", i+1, q.Question)
			} else {
				fmt.Fprintf(&data, "%d. %s: %s\n", i+1, q.Header, q.Question)
			}
			for _, option := range q.Options {
				if option.Description != "" {
					fmt.Fprintf(&data, "   option: %s (%s)\n", option.Label, option.Description)
				} else {
					fmt.Fprintf(&data, "   option: %s\n", option.Label)
				}
			}
			if q.MultiSelect {
				data.WriteString("   (several options may be chosen; join them with commas)\n")
			}
		}
	}
	writeQuotedLines(&out, newCappedText(data.String(), agentWakeOutputCap))
	if prompt.approval != nil {
		fmt.Fprintf(&out, "Answer with thread_reply, token %s, once: decision allow or deny, and your reason in text.", prompt.token)
	} else if len(prompt.question.Questions) == 1 {
		fmt.Fprintf(&out, "Answer with thread_reply, token %s, once: your answer in text.", prompt.token)
	} else {
		fmt.Fprintf(&out, "Answer with thread_reply, token %s, once: one answer per part in answers, in order, and a summary in text.", prompt.token)
	}
	out.WriteString(" If it is not yours to decide, ask the user yourself, as one question with this context. The user can also answer in that thread, which settles this and refuses your reply.")
	return usermessage.Report(kind, from, notice, out.String(), firstDataLine(data.String()))
}

// drainWorkerPrompts waits for queued deliveries, settlements and
// reconciles to finish.
func (a *App) drainWorkerPrompts(ctx context.Context, timeout time.Duration) error {
	return drainSerialQueue(ctx, &a.workerPrompts.queue, timeout)
}

// closeWorkerPrompts is shutdown's join of the prompts' queue, before
// SQLite closes: nothing queued from here on runs, and what was queued
// finishes.
func (a *App) closeWorkerPrompts(ctx context.Context, timeout time.Duration) error {
	a.workerPrompts.gate.Lock()
	a.workerPrompts.closed = true
	a.workerPrompts.gate.Unlock()
	return a.drainWorkerPrompts(ctx, timeout)
}
