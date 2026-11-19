package app

import (
	"errors"
	"sync"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/threadtools"
)

// errThreadResting refuses an app-written message for a thread a Stop is
// bringing to rest, as errThreadArchived refuses one for an archived thread.
// Its caller keeps what the message was for, or settles it as undeliverable:
// a worker's prompt goes to the user, an outside send is refused.
var errThreadResting = errors.New("the thread is being stopped")

// errSessionEnded refuses work a call starts once the session it
// authenticated as has ended.
var errSessionEnded = errors.New("the calling session has ended")

// threadRestFence holds the threads being brought to rest. While a Stop
// holds a thread (hold), from the start of the rest to the end of its
// cascade, the thread takes no app-written message (registerQueueItem) and
// starts no session for one (startAgentNoticeSession), so a worker's prompt,
// an answer or a completion arriving meanwhile does not start it again; a
// person's own message is not held. While a Stop or an archive has closed a
// thread's admission (close), it starts no work of its own: every API that
// admits work a thread starts (a spawn, send, ask or reminder, an assistant,
// a loop or watch, a remote command, a workflow run's start, binding or
// resumption) admits it through admit, and close waits out the admissions
// already under way, so what the rest reads after close is everything the
// thread started.
type threadRestFence struct {
	mu      sync.Mutex
	changed *sync.Cond
	// threads counts, by thread, the holds on its app-written messages.
	threads map[string]int
	// closed counts, by thread, the closures of its admission.
	closed map[string]int
	// admitting counts, by thread, the admissions under way.
	admitting map[string]int
}

func (f *threadRestFence) init() {
	if f.changed == nil {
		f.threads = make(map[string]int)
		f.closed = make(map[string]int)
		f.admitting = make(map[string]int)
		f.changed = sync.NewCond(&f.mu)
	}
}

// hold fences threadID's app-written messages until the returned release
// runs. Holds nest.
func (f *threadRestFence) hold(threadID string) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	f.threads[threadID]++
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.threads[threadID]--; f.threads[threadID] <= 0 {
			delete(f.threads, threadID)
		}
	}
}

// close closes threadID's admission until the returned reopen runs, and
// returns once every admission already under way for it has finished.
// Closures nest. An admission may wait on any lock, so close must not be
// called with one held.
func (f *threadRestFence) close(threadID string) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	f.closed[threadID]++
	for f.admitting[threadID] > 0 {
		f.changed.Wait()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.closed[threadID]--; f.closed[threadID] <= 0 {
				delete(f.closed, threadID)
			}
		})
	}
}

// admit admits one piece of work threadID starts, refused with
// errThreadResting while its admission is closed, and with
// errSessionEnded when current, the check that the session the work
// belongs to is still the thread's, says it is not. current runs under the
// fence, so a rest, which ends the session only while admission is closed,
// either waits for the admission or is seen by it. done runs once the work
// is recorded where a rest reads it.
func (f *threadRestFence) admit(threadID string, current func() bool) (done func(), err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if f.closed[threadID] > 0 {
		return nil, errThreadResting
	}
	if current != nil && !current() {
		return nil, errSessionEnded
	}
	f.admitting[threadID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.admitting[threadID]--; f.admitting[threadID] <= 0 {
				delete(f.admitting, threadID)
			}
			f.changed.Broadcast()
		})
	}, nil
}

// admitWork is threadRestFence.admit with the refusal an agent reads.
// session is the token of the provider session the call authenticated as
// (threadtools.Caller.Session, transport.CallerScope.Session), empty for a
// call no session made, such as a person's: work a session starts is
// admitted only while that session is still threadID's, so a call that
// passed authentication before a rest ended its session starts nothing
// however late it gets here, and one from a session started since does.
func (a *App) admitWork(threadID, session string) (func(), error) {
	if hook := a.admitWorkForTest; hook != nil {
		hook(threadID)
	}
	// A thread a delete claimed starts nothing; one admitted before the
	// claim is drained by the delete's close (familyDelete.verify).
	if err := a.store.CheckThreadNotDeleting(threadID); err != nil {
		return nil, errorsx.Public(threadtools.CodeInvalidRequest,
			"This thread is being deleted and cannot start new work.", err)
	}
	// A thread under an assistant starts nothing while assistants are off;
	// one admitted before they went off is drained by the pause's close.
	if err := a.refusePausedAssistantThread(threadID); err != nil {
		return nil, err
	}
	var current func() bool
	if session != "" {
		current = func() bool {
			live, ok := a.sessionManager().get(threadID)
			return ok && live.Token == session
		}
	}
	done, err := a.resting.admit(threadID, current)
	switch {
	case errors.Is(err, errSessionEnded):
		return nil, errorsx.Public(threadtools.CodeInvalidRequest,
			"The agent session that made this call has ended, so it cannot start new work.", err)
	case err != nil:
		return nil, errorsx.Public(threadtools.CodeInvalidRequest,
			"This thread is being stopped and cannot start new work now.", err)
	}
	return done, nil
}

// holds reports whether threadID is being brought to rest.
func (f *threadRestFence) holds(threadID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.threads[threadID] > 0
}
