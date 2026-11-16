package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// A Codex thread's async questions (internal/userquestion: a turn that ends
// by asking, answered by a later message) are its prompts too, and a
// spawned thread's go where its approvals and blocking questions go: the
// registry, the wake, the spawner's thread_reply, the hand-back and the
// reconcile of app_worker_prompts.go serve all three kinds. Only the record
// of where an open question goes is kept here. Triage's RoutedTo lives in a
// thread's live state, which a session's end drops whole
// (triage.cleanupThread), and an async question outlives its session in
// async_questions: it stays open until it is answered or dismissed, and
// survives a restart, when the boot routes it again
// (seedAsyncQuestionPrompts).
//
// The record follows the store, not the other way round: every change to a
// thread's questions emits provider:async_questions_changed, and so do the
// writes that can open questions on a thread holding none: an unarchive, a
// fork, which carries its source's questions, and a spawn naming its thread.
// A revert emits user_message:reverted, and a thread frame but a patch of a
// thread holding an open question says it may be archived, deleted or
// placed; a frame of any other thread costs nothing. Each queues one
// reconcile of that thread on the prompts' serial queue
// (syncAsyncQuestionPrompts), none once shutdown has begun, which
// reads the thread's unanswered questions and announces what opened and
// what closed. An archived thread's questions are not open: an archive asks
// nobody, as it ends its session's prompts. One item's unanswered questions
// are one prompt, whose occurrence is the item's revision in the store
// (store.AsyncQuestionGroup): any change to the item's questions (the
// person answered, dismissed or reopened some, an answer came back) moves
// it, which closes that occurrence and opens what is left as a new one,
// however many changes one reconcile reads. The spawner's answer is claimed
// against the revision it was written for (store.QueueAsyncAnswers). While
// no turn runs and nothing is queued, an open question is what its thread
// waits on (threadWaitingOn): its Agents row reads blocked and
// thread_status awaiting input.
//
// A question that goes to the person and not to an agent notifies them when
// the thread is owned, since its turn's end did not (notify.MapTurnRest).

// asyncQuestionRoutes is the record of each open async question prompt.
// Its lock is its own, never held with workerPrompts.mu, so the registry
// can ask it whether a prompt is open while holding that one.
type asyncQuestionRoutes struct {
	mu   sync.Mutex
	open map[workerPromptKey]asyncQuestionPrompt
	// toPerson names the items a person took the spawner's answer back from
	// (returnWorkerAnswer): their next occurrence is the person's, as a
	// hand-back is.
	toPerson map[workerPromptKey]bool
}

// asyncQuestionPrompt is one item's unanswered questions, as one prompt:
// a user-input request whose question ids are the questions' indexes in
// the item and whose occurrence is the item's revision, so it renders and
// is answered as a worker's question is. asked is when the item asked,
// which orders a thread's prompts.
type asyncQuestionPrompt struct {
	request provider.UserInputRequest
	route   *provider.PromptRoute
	asked   int64
}

// syncAsyncQuestionPrompts queues a reconcile of threadID's async question
// prompts with its questions as the store has them. Once shutdown has
// begun it queues none: the next boot routes what is open
// (seedAsyncQuestionPrompts).
func (a *App) syncAsyncQuestionPrompts(threadID string) {
	if threadID == "" || a.shuttingDown.Load() {
		return
	}
	a.workerPrompts.run(func() { a.reconcileAsyncQuestions(threadID) })
}

// emitAsyncQuestionsChanged says threadID's async questions may have
// changed by a write that does not report its questions: one that can open
// them on a thread holding none.
func (a *App) emitAsyncQuestionsChanged(threadID string) {
	a.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": threadID})
}

// holds reports whether threadID has an open async question prompt.
func (r *asyncQuestionRoutes) holds(threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.open {
		if key.workerID == threadID {
			return true
		}
	}
	return false
}

// seedAsyncQuestionPrompts routes, at boot, the async questions a restart
// left open: one read of the threads that have one, then a reconcile of
// each. A prompt wake the app quit with still queued was discarded with the
// restore (discardAgentWakeAtBoot); the reconcile queues a fresh one under
// a new token.
func (a *App) seedAsyncQuestionPrompts() {
	ids, err := a.store.ThreadsWithUnansweredAsyncQuestions()
	if err != nil {
		log.Printf("worker prompts: read the threads with open async questions: %v", err)
		return
	}
	for _, id := range ids {
		a.syncAsyncQuestionPrompts(id)
	}
}

// reconcileAsyncQuestions announces what changed in threadID's open async
// question prompts: each that closed settles its registration and
// withdraws its wake, and each that opened goes to the thread's spawner, or
// to the person. It runs on the prompts' serial queue. A failed read
// changes nothing; the next change reads again.
func (a *App) reconcileAsyncQuestions(threadID string) {
	open, err := a.openAsyncQuestions(threadID)
	if err != nil {
		log.Printf("worker prompts: read the async questions of %s: %v", threadID, err)
		return
	}
	routes := &a.workerPrompts.async
	routes.mu.Lock()
	var closed []provider.UserInputRequest
	for key, held := range routes.open {
		if key.workerID != threadID {
			continue
		}
		if now, still := open[key.requestID]; still && now.request.Occurrence == held.request.Occurrence {
			delete(open, key.requestID)
			continue
		}
		delete(routes.open, key)
		closed = append(closed, held.request)
	}
	routes.mu.Unlock()
	var opened []asyncQuestionPrompt
	if len(open) > 0 {
		// The router reads the store, so it runs outside the lock; a change
		// meanwhile queues another reconcile behind this one.
		route := a.routeWorkerPrompt(threadID)
		routes.mu.Lock()
		if routes.open == nil {
			routes.open = make(map[workerPromptKey]asyncQuestionPrompt)
		}
		for _, prompt := range open {
			key := workerPromptKey{workerID: threadID, requestID: prompt.request.RequestID}
			prompt.route = route
			if routes.toPerson[key] {
				prompt.route = nil
				delete(routes.toPerson, key)
			}
			routes.open[key] = prompt
			opened = append(opened, prompt)
		}
		routes.mu.Unlock()
	}
	for _, request := range closed {
		a.settleWorkerPrompt(threadID, request.RequestID, request.Occurrence)
		a.notifyAsyncQuestion(threadID, request.RequestID, false)
	}
	for _, prompt := range opened {
		a.announceAsyncQuestion(prompt)
	}
	if len(closed)+len(opened) > 0 {
		a.noteWorkerLiveness(threadID)
	}
}

// openAsyncQuestions reads threadID's unanswered async questions, by item,
// as the prompts they make, unrouted: none for a thread that is gone or
// archived.
func (a *App) openAsyncQuestions(threadID string) (map[string]asyncQuestionPrompt, error) {
	thread, err := a.store.GetThread(threadID)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrThreadGone) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if thread.Archived {
		return nil, nil
	}
	groups, err := a.store.UnansweredAsyncQuestions(threadID)
	if err != nil {
		return nil, err
	}
	open := make(map[string]asyncQuestionPrompt, len(groups))
	for _, group := range groups {
		request := provider.UserInputRequest{
			RequestID: group.ItemID, ThreadID: threadID, Occurrence: provider.PromptOccurrence(group.Revision),
		}
		for _, row := range group.Questions {
			question := provider.UserInputQuestion{ID: strconv.Itoa(row.Index), Question: row.Title}
			for _, option := range row.Options {
				question.Options = append(question.Options, provider.UserInputQuestionOption{Label: option})
			}
			request.Questions = append(request.Questions, question)
		}
		open[group.ItemID] = asyncQuestionPrompt{request: request, asked: group.Questions[0].CreatedAt}
	}
	return open, nil
}

// announceAsyncQuestion delivers an async question prompt to the thread it
// is routed to, or says it is the person's. It runs on the prompts' serial
// queue, as an announcement's delivery does. A hand-back's announcement
// queued behind the reconcile that closed or replaced its occurrence asks
// nobody, as a delivery overtaken by a later announcement does
// (deliverWorkerPrompt): only the occurrence still open, and still the
// person's, notifies.
func (a *App) announceAsyncQuestion(prompt asyncQuestionPrompt) {
	request := prompt.request
	if prompt.route == nil {
		ref := triage.PromptRef{ThreadID: request.ThreadID, RequestID: request.RequestID, Occurrence: request.Occurrence}
		if route, open := a.asyncQuestionRoute(ref); open && route == nil {
			a.notifyAsyncQuestion(request.ThreadID, request.RequestID, true)
		}
		return
	}
	a.notifyAsyncQuestion(request.ThreadID, request.RequestID, false)
	a.deliverWorkerPrompt(*prompt.route, workerPrompt{
		workerID: request.ThreadID, requestID: request.RequestID, occurrence: request.Occurrence,
		question: &request, async: true,
	})
}

// notifyAsyncQuestion raises, or with open false retracts, the
// notification of an async question that waits on the person, under the
// id a blocking question's takes. It is raised only for an owned worker,
// whose turn's end raised none.
func (a *App) notifyAsyncQuestion(threadID, itemID string, open bool) {
	a.queueNotification(func() (notify.Notification, bool) {
		moment := notify.QuestionMoment{Thread: notify.ThreadRef{ID: threadID}, RequestID: itemID, Answered: !open}
		if open {
			moment.Thread = a.notificationThreadRef(threadID)
			if !moment.Thread.Owned {
				return notify.Notification{}, false
			}
		}
		return notify.MapQuestion(moment)
	})
}

// asyncQuestionRoute is triage.Router.PromptRoute for an async question
// prompt: its route, nil when it is the person's, and whether the
// occurrence ref names is still open.
func (a *App) asyncQuestionRoute(ref triage.PromptRef) (*provider.PromptRoute, bool) {
	routes := &a.workerPrompts.async
	routes.mu.Lock()
	defer routes.mu.Unlock()
	held, open := routes.open[workerPromptKey{workerID: ref.ThreadID, requestID: ref.RequestID}]
	if !open || held.request.Occurrence != ref.Occurrence {
		return nil, false
	}
	if held.route == nil {
		return nil, true
	}
	route := *held.route
	return &route, true
}

// rerouteAsyncQuestions is triage's reroutePrompts for async question
// prompts: the ones routed to from, those refs names or every one when refs
// is empty, go where the prompt router routes their threads now. reject
// says from cannot take them, so a route back to it goes to the person;
// otherwise such a prompt keeps its route. Each prompt that moved is
// announced again on the prompts' serial queue. It returns how many moved.
func (a *App) rerouteAsyncQuestions(from string, refs []triage.PromptRef, reject bool) int {
	if from == "" {
		return 0
	}
	routedFrom := func(route *provider.PromptRoute) bool { return route != nil && route.ThreadID == from }
	routes := &a.workerPrompts.async
	var moving []triage.PromptRef
	routes.mu.Lock()
	for key, held := range routes.open {
		ref := triage.PromptRef{ThreadID: key.workerID, RequestID: key.requestID, Occurrence: held.request.Occurrence}
		if routedFrom(held.route) && (len(refs) == 0 || slices.Contains(refs, ref)) {
			moving = append(moving, ref)
		}
	}
	routes.mu.Unlock()
	if len(moving) == 0 {
		return 0
	}
	to := make(map[string]*provider.PromptRoute, len(moving))
	for _, ref := range moving {
		if _, done := to[ref.ThreadID]; done {
			continue
		}
		route := a.routeWorkerPrompt(ref.ThreadID)
		if reject && routedFrom(route) {
			route = nil
		}
		to[ref.ThreadID] = route
	}
	var moved []asyncQuestionPrompt
	routes.mu.Lock()
	for _, ref := range moving {
		route := to[ref.ThreadID]
		key := workerPromptKey{workerID: ref.ThreadID, requestID: ref.RequestID}
		held, open := routes.open[key]
		if routedFrom(route) || !open || held.request.Occurrence != ref.Occurrence || !routedFrom(held.route) {
			continue
		}
		held.route = route
		routes.open[key] = held
		moved = append(moved, held)
	}
	routes.mu.Unlock()
	for _, prompt := range moved {
		a.workerPrompts.run(func() { a.announceAsyncQuestion(prompt) })
		a.noteWorkerLiveness(prompt.request.ThreadID)
	}
	return len(moved)
}

// asyncQuestionBlocked says what threadID's open async questions ask, for
// threadWaitingOn: the first question of its earliest prompt and the thread
// it is routed to, empty when it is the person's. Found is false when it
// has none open.
func (a *App) asyncQuestionBlocked(threadID string) (detail, routedTo string, found bool) {
	routes := &a.workerPrompts.async
	routes.mu.Lock()
	defer routes.mu.Unlock()
	var first *asyncQuestionPrompt
	for key, held := range routes.open {
		if key.workerID != threadID || (first != nil && (held.asked > first.asked ||
			held.asked == first.asked && held.request.RequestID > first.request.RequestID)) {
			continue
		}
		copied := held
		first = &copied
	}
	if first == nil {
		return "", "", false
	}
	if len(first.request.Questions) > 0 {
		detail = first.request.Questions[0].Question
	}
	if first.route != nil {
		routedTo = first.route.Title
	}
	return detail, routedTo, true
}

// dismissRestingAsyncQuestions dismisses the async questions of a thread a
// Stop brings to rest, as its session's end closes its other prompts: a
// thread at rest waits on nothing. The reconcile their change queues
// withdraws the wakes that put them to its spawner.
func (a *App) dismissRestingAsyncQuestions(threadID string) error {
	dismissed, err := a.store.DismissUnansweredAsyncQuestions(threadID)
	if err != nil {
		return err
	}
	if dismissed > 0 {
		a.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": threadID})
	}
	return nil
}

// isWorkerAnswer reports the send id of an agent's answer to its worker's
// async questions.
func isWorkerAnswer(sendID string) bool {
	return strings.HasPrefix(sendID, workerAnswerSendIDRoot)
}

// returnWorkerAnswer takes back the spawner's answer to a worker's async
// questions, which never reached the worker: in one store transaction its
// queue row goes and the questions it answered are unanswered again
// (store.ReturnAsyncAnswer), and their change routes them again under a
// fresh token. It never goes to the worker's composer: it is not the
// person's to send. Every exit an undelivered answer takes comes here: a
// person's withdrawal from the worker's Incoming rows, and, through
// giveBackToOwners, a session that ended or a provider that refused it, a
// failed Codex resend, a wholesale drop (End Session, a Stop, an archive, a
// revert, a cleared queue) and the boot.
//
// Returned by the app or a failure, the questions go where the worker's
// questions route now, the spawner when it can take them. Withdrawn by a
// person (byPerson), who overruled the agent, they go to the person, as a
// hand-back does: the items are marked before the write, so no reconcile
// opens them for the spawner in between.
func (a *App) returnWorkerAnswer(threadID, sendID string, byPerson bool) error {
	var marked []workerPromptKey
	if byPerson {
		items, err := a.store.AsyncAnswerItems(threadID, sendID)
		if err != nil {
			return err
		}
		marked = a.workerPrompts.async.markToPerson(threadID, items)
	}
	reopened, err := a.store.ReturnAsyncAnswer(threadID, sendID)
	if err != nil {
		a.workerPrompts.async.unmarkToPerson(marked)
		return err
	}
	if reopened {
		a.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": threadID})
	}
	return nil
}

func (r *asyncQuestionRoutes) markToPerson(threadID string, items []string) []workerPromptKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.toPerson == nil {
		r.toPerson = make(map[workerPromptKey]bool)
	}
	keys := make([]workerPromptKey, 0, len(items))
	for _, item := range items {
		key := workerPromptKey{workerID: threadID, requestID: item}
		r.toPerson[key] = true
		keys = append(keys, key)
	}
	return keys
}

func (r *asyncQuestionRoutes) unmarkToPerson(keys []workerPromptKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range keys {
		delete(r.toPerson, key)
	}
}

// answerAsyncWorkerPrompt writes the spawner's answer to a worker's async
// questions through the path the person's answers take (queueAsyncAnswers),
// attributed to the spawner and claimed against the occurrence the prompt
// was put under, so a question changed since refuses it. Its send id is
// the registration's, so the same answer again is the same message.
func (a *App) answerAsyncWorkerPrompt(ctx context.Context, prompt workerPrompt, response provider.UserInputResponse) error {
	answers := make([]store.AsyncQuestionAnswer, 0, len(prompt.question.Questions))
	for _, q := range prompt.question.Questions {
		index, err := strconv.Atoi(q.ID)
		if err != nil {
			return fmt.Errorf("async question id %q: %w", q.ID, err)
		}
		answers = append(answers, store.AsyncQuestionAnswer{ItemID: prompt.requestID, Index: index, Answer: strings.Join(response.Answers[q.ID], ", ")})
	}
	spawner := usermessage.Sender{ThreadID: prompt.agentID}
	if agent, err := a.store.GetThread(prompt.agentID); err == nil {
		spawner.Title = agent.Title
	}
	sendID := workerAnswerSendIDRoot + prompt.token
	unlock, err := a.lockSendAdmission(ctx, prompt.workerID, sendID)
	if err != nil {
		return err
	}
	defer unlock()
	// The answer is the spawner's own message to its worker, all of it what
	// the spawner says: no request, and no answer to one of the worker's.
	written := func(text string) usermessage.AgentMessage { return usermessage.Message(spawner, text) }
	asked := map[string]int64{prompt.requestID: int64(prompt.occurrence)}
	return a.queueAsyncAnswers(prompt.workerID, sendID, answers, asked, written, agentNotice{
		startFailed: func(err error) string {
			return fmt.Sprintf("Thread %q answered this thread's questions, but this thread's agent could not start: %v", spawner.Title, err)
		},
	})
}
