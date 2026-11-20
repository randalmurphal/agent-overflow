package app

import (
	"context"
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
// reconcile of app_worker_prompts*.go serve all three kinds. Only the record
// of where an open question goes is kept here. Triage's RoutedTo lives in a
// thread's live state, which a session's end drops whole
// (triage.cleanupThread), and an async question outlives its session in
// async_questions: it stays open until it is answered or dismissed.
//
// The record is this run's: a question not recorded by this run of the app
// is the person's, as every question was before a spawner could take one.
// The store marks each question with the run that recorded it and says
// whether that is this one (store.AsyncQuestionGroup.AskedThisRun); a time
// cannot say it, since the wall clock can be set during a run. No reconcile
// opens such a question, so it is put to no agent, notifies nobody and is
// not what its thread waits on, and the boot reads nothing. The mark never
// changes and a fork copies it, so an answer or a return that leaves some
// of an item's questions open leaves them the person's too, and a fork's
// copy of one is the person's. A question that arrives by transfer carries
// no mark. The person answers it in the thread as any question.
//
// The record follows the store, not the other way round: every change to a
// thread's questions emits provider:async_questions_changed, a revert emits
// user_message:reverted, and every thread frame but a patch says the thread
// may have been made, copied, moved here, forked (a fork carries its
// source's questions), archived, unarchived, deleted or placed, and a
// move's transfer row says it may have left this computer. Each queues
// one reconcile of that thread on the prompts' serial queue
// (syncAsyncQuestionPrompts), none once shutdown has begun, which reads the
// thread's unanswered questions in one statement and announces what opened
// and what closed: a thread asking nothing costs that read and no more. A
// spawn that names a fork's thread adopts the questions a frame before the
// naming left the person's (adoptAsyncQuestionPrompts). An archived or
// moved thread's questions are not open: an archive asks nobody, as it
// ends its session's prompts. One item's unanswered questions
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
	// (returnWorkerAnswer), each with the revision that return left it at:
	// the occurrence a reconcile opens at that revision or later is the
	// person's, as a hand-back is. A reconcile that read the item before the
	// return reads an older revision, and neither takes nor consumes it.
	toPerson map[workerPromptKey]int64
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
// begun it queues none: what is open then is the person's after the
// restart.
func (a *App) syncAsyncQuestionPrompts(threadID string) {
	if threadID == "" || a.shuttingDown.Load() {
		return
	}
	a.workerPrompts.run(func() { a.reconcileAsyncQuestions(threadID, false) })
}

// adoptAsyncQuestionPrompts queues a reconcile of threadID's async
// question prompts that puts those it holds unrouted to the thread's
// spawner, for the spawn that just named it. A reconcile that read them
// before the naming found no spawner and left them the person's; nothing
// else routes them again while their questions do not change.
func (a *App) adoptAsyncQuestionPrompts(threadID string) {
	if threadID == "" || a.shuttingDown.Load() {
		return
	}
	a.workerPrompts.run(func() { a.reconcileAsyncQuestions(threadID, true) })
}

// reconcileAsyncQuestions announces what changed in threadID's open async
// question prompts: each that closed settles its registration and
// withdraws its wake, and each that opened goes to the thread's spawner, or
// to the person. adopt opens again, under the route the thread has now,
// each prompt it holds unrouted (adoptAsyncQuestionPrompts). It runs on
// the prompts' serial queue, one store read when nothing opens. A failed
// read changes nothing; the next change reads again.
func (a *App) reconcileAsyncQuestions(threadID string, adopt bool) {
	open, err := a.openAsyncQuestions(threadID)
	if err != nil {
		log.Printf("worker prompts: read the async questions of %s: %v", threadID, err)
		return
	}
	if hook := a.asyncQuestionsReadForTest; hook != nil {
		hook(threadID)
	}
	routes := &a.workerPrompts.async
	routes.mu.Lock()
	var closed []provider.UserInputRequest
	for key, held := range routes.open {
		if key.workerID != threadID {
			continue
		}
		if now, still := open[key.requestID]; still && now.request.Occurrence == held.request.Occurrence && (!adopt || held.route != nil) {
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
			if at, marked := routes.toPerson[key]; marked && int64(prompt.request.Occurrence) >= at {
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
// as the prompts they make, unrouted, in one store read: none for a thread
// that is gone, moved away or archived, and none for an item this run of
// the app did not record, which is the person's.
func (a *App) openAsyncQuestions(threadID string) (map[string]asyncQuestionPrompt, error) {
	groups, err := a.store.UnansweredAsyncQuestions(threadID)
	if err != nil {
		return nil, err
	}
	open := make(map[string]asyncQuestionPrompt, len(groups))
	for _, group := range groups {
		if !group.AskedThisRun {
			continue
		}
		// An item's rows share the item's time, which orders its prompts.
		asked := group.Questions[0].CreatedAt
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
		open[group.ItemID] = asyncQuestionPrompt{request: request, asked: asked}
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

// routeAsyncQuestionsAgain is routeWorkerPromptsAgain for workerID's async
// question prompts: each whose route the router now changes moves, a
// person's only when fromPerson, and one that moved to the person settles
// the registration its agent held and withdraws that wake. A route to or
// from a paired computer is its link's to change (handBackLinkPrompts). It
// returns how many moved.
func (a *App) routeAsyncQuestionsAgain(workerID string, fromPerson bool) int {
	routes := &a.workerPrompts.async
	routes.mu.Lock()
	var refs []triage.PromptRef
	for key, prompt := range routes.open {
		if key.workerID == workerID {
			refs = append(refs, triage.PromptRef{ThreadID: workerID, RequestID: key.requestID, Occurrence: prompt.request.Occurrence})
		}
	}
	routes.mu.Unlock()
	if len(refs) == 0 {
		return 0
	}
	// Read outside the routes' lock: the answering state has its own.
	answering := map[triage.PromptRef]bool{}
	for _, ref := range refs {
		if a.workerPromptAnswering(ref) {
			answering[ref] = true
		}
	}
	to := a.routeWorkerPrompt(workerID)
	sameRoute := func(x, y *provider.PromptRoute) bool {
		return (x == nil) == (y == nil) && (x == nil || (x.ThreadID == y.ThreadID && x.ComputerID == y.ComputerID))
	}
	remote := func(route *provider.PromptRoute) bool { return route != nil && route.ComputerID != "" }
	var moved []asyncQuestionPrompt
	routes.mu.Lock()
	for key, prompt := range routes.open {
		if key.workerID != workerID || sameRoute(prompt.route, to) || (prompt.route == nil && !fromPerson) || remote(prompt.route) || remote(to) {
			continue
		}
		if answering[triage.PromptRef{ThreadID: workerID, RequestID: key.requestID, Occurrence: prompt.request.Occurrence}] {
			continue
		}
		prompt.route = to
		routes.open[key] = prompt
		moved = append(moved, prompt)
	}
	routes.mu.Unlock()
	for _, prompt := range moved {
		request := prompt.request
		if prompt.route == nil {
			a.settleWorkerPrompt(workerID, request.RequestID, request.Occurrence)
		}
		a.announceAsyncQuestion(prompt)
	}
	if len(moved) > 0 {
		a.noteWorkerLiveness(workerID)
	}
	return len(moved)
}

// asyncQuestionBlocked says what threadID's open async questions ask, for
// threadWaitingOn: the first question of its earliest prompt and the route
// it took, nil when it is the person's. Found is false when it has none
// open.
func (a *App) asyncQuestionBlocked(threadID string) (detail string, route *provider.PromptRoute, found bool) {
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
		return "", nil, false
	}
	if len(first.request.Questions) > 0 {
		detail = first.request.Questions[0].Question
	}
	return detail, first.route, true
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
// person's to send. Every exit an undelivered answer takes comes here:
// through withdrawQueuedMessage, a person's withdrawal from the worker's
// Incoming rows and a rest or an archive taking back what the worker held
// queued, session or not; and through giveBackToOwners, a session that
// ended or a provider that refused it, a failed Codex resend, a wholesale
// drop (End Session, a Stop, an archive, a revert, a cleared queue) and the
// boot.
//
// Returned by the app or a failure, the questions go where the worker's
// questions route now, the spawner when it can take them, unless this run
// did not record them, as for those the boot returns: they are the
// person's. Withdrawn by a
// person (byPerson), who overruled the agent, they go to the person, as a
// hand-back does: each item is marked with the revision the return leaves
// it at, inside the return's transaction, so a reconcile that reads the
// return reads the mark, and one that read the item before it neither
// takes nor consumes the mark.
func (a *App) returnWorkerAnswer(threadID, sendID string, byPerson bool) error {
	var mark func(map[string]int64)
	var marked map[string]int64
	if byPerson {
		mark = func(revisions map[string]int64) {
			marked = revisions
			a.workerPrompts.async.markToPerson(threadID, revisions)
		}
	}
	reopened, err := a.store.ReturnAsyncAnswer(threadID, sendID, mark)
	if err != nil {
		a.workerPrompts.async.unmarkToPerson(threadID, marked)
		return err
	}
	if len(reopened) > 0 {
		a.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": threadID})
	}
	return nil
}

func (r *asyncQuestionRoutes) markToPerson(threadID string, revisions map[string]int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.toPerson == nil {
		r.toPerson = make(map[workerPromptKey]int64)
	}
	for item, revision := range revisions {
		r.toPerson[workerPromptKey{workerID: threadID, requestID: item}] = revision
	}
}

// unmarkToPerson takes back the marks of a return that did not commit.
func (r *asyncQuestionRoutes) unmarkToPerson(threadID string, revisions map[string]int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for item, revision := range revisions {
		key := workerPromptKey{workerID: threadID, requestID: item}
		if r.toPerson[key] == revision {
			delete(r.toPerson, key)
		}
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
	if hold := prompt.remote; hold != nil {
		spawner.Title, spawner.ComputerID, spawner.ComputerName = hold.title, hold.computerID, hold.computerName
	} else if agent, err := a.store.GetThread(prompt.agentID); err == nil {
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
	written := func(text string) (usermessage.AgentMessage, error) { return usermessage.Message(spawner, text) }
	asked := map[string]int64{prompt.requestID: int64(prompt.occurrence)}
	return a.queueAsyncAnswers(prompt.workerID, sendID, answers, asked, written, agentNotice{
		startFailed: func(err error) string {
			return fmt.Sprintf("Thread %q answered this thread's questions, but this thread's agent could not start: %v", spawner.Title, err)
		},
	})
}
