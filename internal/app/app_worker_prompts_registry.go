package app

import (
	"sync"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/serialqueue"
	"agent-overflow/internal/triage"
)

// workerPromptRetention is how long an answered or settled prompt is kept so
// a late or repeated thread_reply is told what happened to it.
const workerPromptRetention = 24 * time.Hour

type workerPromptState int

const (
	// workerPromptRouted: the wake is queued or delivered and the prompt is
	// open.
	workerPromptRouted workerPromptState = iota
	// workerPromptAnswering: the agent's answer is being written to the
	// worker's provider.
	workerPromptAnswering
	// workerPromptAnswered: the agent answered it.
	workerPromptAnswered
	// workerPromptSettled: the user answered it in the worker, the worker
	// stopped waiting, or it went to the user or to another thread.
	workerPromptSettled
)

type workerPrompt struct {
	token     string
	workerID  string
	agentID   string
	requestID string
	// occurrence is the prompt's (provider.ApprovalRequest.Occurrence).
	occurrence provider.PromptOccurrence
	approval   *provider.ApprovalRequest
	question   *provider.UserInputRequest
	// async marks a Codex async question prompt: question holds its
	// unanswered questions, and its route is asyncQuestionRoutes'.
	async bool
	state workerPromptState
	// answer is what the agent sent, so a retry of the same answer is
	// recognized.
	answer   string
	closedAt time.Time
	// handBack marks a prompt something handed back while it was being
	// answered: an answer that does not land hands it back then
	// (endPromptAnswer).
	handBack bool
	// remote holds a prompt routed to a thread on a paired computer, nil
	// for one routed to a thread here.
	remote *remotePromptHold
}

func (p *workerPrompt) sendID() string { return workerPromptSendIDRoot + p.token }

func (p *workerPrompt) ref() triage.PromptRef {
	return triage.PromptRef{ThreadID: p.workerID, RequestID: p.requestID, Occurrence: p.occurrence}
}

// workerPromptKey names one prompt of one worker.
type workerPromptKey struct{ workerID, requestID string }

// workerPrompts is the registry of routed prompts and the ordered queue
// their deliveries and settlements run on. The queue keeps a prompt's
// resolution behind its delivery, and runs both off the provider event
// worker: delivering takes the agent's thread lock and can start its
// session.
type workerPrompts struct {
	mu      sync.Mutex
	byToken map[string]*workerPrompt
	// current is the token of each prompt's latest registration. A prompt
	// that moved to another thread, or that a later session asked again, is
	// registered again under a new token; the earlier one is kept only to
	// answer a late reply.
	current map[workerPromptKey]string
	queue   serialqueue.Queue
	// gate guards closed, which shutdown sets before it joins the queue
	// (drainWorkerPrompts): from then on nothing runs on it, so nothing
	// reads SQLite after it closes.
	gate   sync.Mutex
	closed bool
	// parked holds, by agent, the prompts of owned workers their agent did
	// not take, still routed to it, put to it again when its next turn
	// starts (parkWorkerPrompt).
	parked map[string][]workerPrompt
	// async is where each open async question prompt goes.
	async asyncQuestionRoutes
	// remote is the prompts paired computers' workers put to threads here.
	remote remoteWorkerPrompts
}

// run queues job on the prompts' serial queue, unless shutdown closed it:
// a prompt it would deliver or settle dies with the process, and the boot
// routes the async questions again.
func (w *workerPrompts) run(job func()) bool {
	w.gate.Lock()
	defer w.gate.Unlock()
	if w.closed {
		return false
	}
	w.queue.Go(job)
	return true
}

// add registers a prompt under a fresh token as the prompt's current
// registration, and returns the registration it replaced, closed if it was
// routed, whose wake its caller withdraws. It first forgets prompts closed
// past their retention and closes the routed ones whose prompt is gone, so
// the registry holds no more than the open prompts and a day of closed
// ones.
func (w *workerPrompts) add(prompt workerPrompt, open func(workerPrompt) bool) (registered workerPrompt, replaced *workerPrompt) {
	prompt.token = newThreadRequestToken()
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.byToken == nil {
		w.byToken = make(map[string]*workerPrompt)
	}
	if w.current == nil {
		w.current = make(map[workerPromptKey]string)
	}
	if previous := w.byToken[w.current[prompt.key()]]; previous != nil {
		if previous.state == workerPromptRouted {
			previous.state, previous.closedAt = workerPromptSettled, now
			previous.noteOwner()
		}
		retired := *previous
		replaced = &retired
	}
	for token, existing := range w.byToken {
		if existing.state == workerPromptRouted && !open(*existing) {
			existing.state, existing.closedAt = workerPromptSettled, now
			existing.noteOwner()
		}
		if !existing.closedAt.IsZero() && now.Sub(existing.closedAt) > workerPromptRetention {
			delete(w.byToken, token)
			if key := existing.key(); w.current[key] == token {
				delete(w.current, key)
			}
		}
	}
	stored := prompt
	w.byToken[stored.token] = &stored
	w.current[stored.key()] = stored.token
	stored.noteOwner()
	return stored, replaced
}

func (p *workerPrompt) noteOwner() {
	if p.remote != nil && p.remote.activity != nil {
		p.remote.activity.note(p.remote.device, p.remote.spawnToken, true)
	}
}

func (p *workerPrompt) key() workerPromptKey {
	return workerPromptKey{workerID: p.workerID, requestID: p.requestID}
}

// get is the registration under token as it stands.
func (w *workerPrompts) get(token string) (workerPrompt, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	prompt := w.byToken[token]
	if prompt == nil {
		return workerPrompt{}, false
	}
	return *prompt, true
}

// settle closes the current registration of a worker's prompt, for a
// resolution in the worker of occurrence upTo. Occurrences only grow, so a
// registration of a later prompt under the same request id is not upTo's
// to close, and zero, a resolution that named no prompt, closes nothing.
// Settled is false when it is not routed, including when the agent is
// answering or answered it: that answer is what resolved it.
func (w *workerPrompts) settle(workerID, requestID string, upTo provider.PromptOccurrence) (workerPrompt, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	prompt := w.byToken[w.current[workerPromptKey{workerID: workerID, requestID: requestID}]]
	if prompt == nil || prompt.occurrence > upTo || prompt.state != workerPromptRouted {
		return workerPrompt{}, false
	}
	prompt.state, prompt.closedAt = workerPromptSettled, time.Now()
	prompt.noteOwner()
	return *prompt, true
}

// settleWorker closes every routed registration of the prompts workerID's
// session asked, which leaves its async questions.
func (w *workerPrompts) settleWorker(workerID string) []workerPrompt {
	w.mu.Lock()
	defer w.mu.Unlock()
	var closed []workerPrompt
	now := time.Now()
	for _, prompt := range w.byToken {
		if prompt.workerID != workerID || prompt.async || prompt.state != workerPromptRouted {
			continue
		}
		prompt.state, prompt.closedAt = workerPromptSettled, now
		prompt.noteOwner()
		closed = append(closed, *prompt)
	}
	return closed
}

// settleToken closes the registration under token when it is routed. One
// being answered is marked to be handed back if its answer does not land
// (endPromptAnswer).
func (w *workerPrompts) settleToken(token string) (workerPrompt, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	prompt := w.byToken[token]
	if prompt != nil && prompt.state == workerPromptAnswering {
		prompt.handBack = true
	}
	if prompt == nil || prompt.state != workerPromptRouted {
		return workerPrompt{}, false
	}
	prompt.state, prompt.closedAt = workerPromptSettled, time.Now()
	prompt.noteOwner()
	return *prompt, true
}

// beginAnswer claims a routed prompt for the agent's answer.
func (w *workerPrompts) beginAnswer(token, answer string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	prompt := w.byToken[token]
	if prompt == nil {
		return errWorkerPromptClosed
	}
	switch prompt.state {
	case workerPromptRouted:
		prompt.state, prompt.answer = workerPromptAnswering, answer
		prompt.noteOwner()
		return nil
	case workerPromptAnswering:
		return errWorkerPromptAnswering
	case workerPromptAnswered:
		if prompt.answer == answer {
			return errWorkerPromptRepeated
		}
		return errWorkerPromptAnswered
	default:
		return errWorkerPromptClosed
	}
}

// endAnswer records how the agent's answer ended, for endPromptAnswer
// alone. An answer that did not reach the worker leaves the prompt routed,
// unless a later registration replaced this one meanwhile: then the prompt
// is that one's. It returns the registration, whether it is routed again,
// and whether a hand back was asked for meanwhile.
func (w *workerPrompts) endAnswer(token string, state workerPromptState) (workerPrompt, bool, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	prompt := w.byToken[token]
	if prompt == nil {
		return workerPrompt{}, false, false
	}
	if state == workerPromptRouted && w.current[prompt.key()] != token {
		state = workerPromptSettled
	}
	prompt.state = state
	prompt.noteOwner()
	handBack := prompt.handBack
	prompt.handBack = false
	if state == workerPromptRouted {
		prompt.answer = ""
		return *prompt, true, handBack
	}
	prompt.closedAt = time.Now()
	return *prompt, false, false
}

// park keeps prompt, a closed registration, for its agent's next turn, and
// forgets the parked prompts that are no longer open or whose registration
// a later one replaced, so it holds no more than the open prompts.
func (w *workerPrompts) park(prompt workerPrompt, open func(workerPrompt) bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.parked == nil {
		w.parked = make(map[string][]workerPrompt)
	}
	for agent, held := range w.parked {
		kept := held[:0]
		for _, p := range held {
			if w.current[p.key()] == p.token && open(p) {
				kept = append(kept, p)
			}
		}
		if len(kept) == 0 {
			delete(w.parked, agent)
		} else {
			w.parked[agent] = kept
		}
	}
	w.parked[prompt.agentID] = append(w.parked[prompt.agentID], prompt)
}

// hasParked reports whether agentID has a parked prompt.
func (w *workerPrompts) hasParked(agentID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.parked[agentID]) > 0
}

// takeParked removes and returns agentID's parked prompts whose
// registration is still their prompt's current one.
func (w *workerPrompts) takeParked(agentID string) []workerPrompt {
	w.mu.Lock()
	defer w.mu.Unlock()
	held := w.parked[agentID]
	delete(w.parked, agentID)
	var out []workerPrompt
	for _, p := range held {
		if w.current[p.key()] == p.token {
			out = append(out, p)
		}
	}
	return out
}
