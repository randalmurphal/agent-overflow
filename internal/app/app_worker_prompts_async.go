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
// thread's questions emits provider:async_questions_changed, a revert emits
// user_message:reverted, and every thread frame but a patch says it may be
// archived, unarchived, deleted or newly named by a spawn (a fork carries
// its source's questions). Each queues one reconcile of that
// thread on the prompts' serial queue (syncAsyncQuestionPrompts), which
// reads the thread's unanswered questions and announces what opened and
// what closed. An archived thread's questions are not open: an archive asks
// nobody, as it ends its session's prompts. One item's unanswered questions
// are one prompt; a change to which of them are unanswered (the person
// answered or dismissed some) closes that occurrence and opens the rest as
// a new one. While no turn runs and nothing is queued, an open question is
// what its thread waits on: its Agents row reads blocked (workerHeldUnrest).
//
// A question that goes to the person and not to an agent notifies them when
// the thread is owned, since its turn's end did not (notify.MapTurnRest).

// asyncQuestionRoutes is the record of each open async question prompt.
// Its lock is its own, never held with workerPrompts.mu, so the registry
// can ask it whether a prompt is open while holding that one.
type asyncQuestionRoutes struct {
	mu   sync.Mutex
	open map[workerPromptKey]asyncQuestionPrompt
	last provider.PromptOccurrence
}

// asyncQuestionPrompt is one item's unanswered questions, as one prompt:
// a user-input request whose question ids are the questions' indexes in
// the item, so it renders and is answered as a worker's question is.
type asyncQuestionPrompt struct {
	request provider.UserInputRequest
	route   *provider.PromptRoute
}

// syncAsyncQuestionPrompts queues a reconcile of threadID's async question
// prompts with its questions as the store has them.
func (a *App) syncAsyncQuestionPrompts(threadID string) {
	if threadID == "" {
		return
	}
	a.workerPrompts.queue.Go(func() { a.reconcileAsyncQuestions(threadID) })
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
		if now, still := open[key.requestID]; still && sameAsyncQuestions(now, held.request) {
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
		for _, request := range open {
			routes.last++
			request.Occurrence = routes.last
			prompt := asyncQuestionPrompt{request: request, route: route}
			routes.open[workerPromptKey{workerID: threadID, requestID: request.RequestID}] = prompt
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
// as the prompts they make: none for a thread that is gone or archived.
func (a *App) openAsyncQuestions(threadID string) (map[string]provider.UserInputRequest, error) {
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
	rows, err := a.store.ListAsyncQuestions(threadID, "")
	if err != nil {
		return nil, err
	}
	open := map[string]provider.UserInputRequest{}
	for _, row := range rows {
		if row.State != "unanswered" {
			continue
		}
		request := open[row.ItemID]
		request.RequestID, request.ThreadID = row.ItemID, threadID
		question := provider.UserInputQuestion{ID: strconv.Itoa(row.Index), Question: row.Title}
		for _, option := range row.Options {
			question.Options = append(question.Options, provider.UserInputQuestionOption{Label: option})
		}
		request.Questions = append(request.Questions, question)
		open[row.ItemID] = request
	}
	return open, nil
}

// sameAsyncQuestions reports whether two readings of one item ask the same
// unanswered questions.
func sameAsyncQuestions(a, b provider.UserInputRequest) bool {
	return slices.EqualFunc(a.Questions, b.Questions, func(x, y provider.UserInputQuestion) bool { return x.ID == y.ID })
}

// announceAsyncQuestion delivers an async question prompt to the thread it
// is routed to, or says it is the person's. It runs on the prompts' serial
// queue, as an announcement's delivery does.
func (a *App) announceAsyncQuestion(prompt asyncQuestionPrompt) {
	request := prompt.request
	if prompt.route == nil {
		a.notifyAsyncQuestion(request.ThreadID, request.RequestID, true)
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
		a.workerPrompts.queue.Go(func() { a.announceAsyncQuestion(prompt) })
		a.noteWorkerLiveness(prompt.request.ThreadID)
	}
	return len(moved)
}

// asyncQuestionBlocked says what threadID's open async questions ask, for
// its Agents row: the first question of its earliest prompt and the thread
// it is routed to, empty when it is the person's. Found is false when it
// has none open.
func (a *App) asyncQuestionBlocked(threadID string) (detail, routedTo string, found bool) {
	routes := &a.workerPrompts.async
	routes.mu.Lock()
	defer routes.mu.Unlock()
	var first *asyncQuestionPrompt
	for key, held := range routes.open {
		if key.workerID != threadID || (first != nil && held.request.Occurrence > first.request.Occurrence) {
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

// answerAsyncWorkerPrompt writes the spawner's answer to a worker's async
// questions through the path the person's answers take (queueAsyncAnswers),
// attributed to the spawner. Its send id is the registration's, so the same
// answer again is the same message.
func (a *App) answerAsyncWorkerPrompt(ctx context.Context, prompt workerPrompt, response provider.UserInputResponse) error {
	answers := make([]store.AsyncQuestionAnswer, 0, len(prompt.question.Questions))
	for _, q := range prompt.question.Questions {
		index, err := strconv.Atoi(q.ID)
		if err != nil {
			return fmt.Errorf("async question id %q: %w", q.ID, err)
		}
		answers = append(answers, store.AsyncQuestionAnswer{ItemID: prompt.requestID, Index: index, Answer: strings.Join(response.Answers[q.ID], ", ")})
	}
	origin := &usermessage.OriginThread{ThreadID: prompt.agentID}
	if agent, err := a.store.GetThread(prompt.agentID); err == nil {
		origin.Title = agent.Title
	}
	sendID := workerAnswerSendIDRoot + prompt.token
	unlock, err := a.lockSendAdmission(ctx, prompt.workerID, sendID)
	if err != nil {
		return err
	}
	defer unlock()
	return a.queueAsyncAnswers(prompt.workerID, sendID, answers, agentNotice{
		origin:       usermessage.OriginAgentThread,
		originThread: origin,
		startFailed: func(err error) string {
			return fmt.Sprintf("Thread %q answered this thread's questions, but this thread's agent could not start: %v", origin.Title, err)
		},
	})
}
