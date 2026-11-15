package app

import (
	"context"
	"sync"

	"agent-overflow/internal/errorsx"
)

// writeAttempts holds the attempts of one kind of write this process
// received, each named by an id its client minted: every running one and
// the outcomes of the last writeAttemptsKept finished, then the ids alone
// of the writeAttemptIDsKept before those (forgotten). Once an id leaves
// that too (overflowed), an id it does not hold may be one it received, so
// it can no longer say an attempt never arrived.
//
// It is how a client that lost a write's answer, to a disconnect or to its
// own wait running out, learns what the write did: a repeat of the id waits
// for and answers with the first arrival's outcome instead of writing
// again, and the write's outcome method reports it. Process memory only: a
// restarted backend knows none of them, which its client tells by the
// hello's launchId.
type writeAttempts[T any] struct {
	mu         sync.Mutex
	byID       map[string]*writeAttempt[T]
	finished   []string
	forgotten  map[string]string // id to key
	forgetting []string
	overflowed bool
}

// writeAttemptsKept bounds the finished attempts whose outcome a log keeps;
// writeAttemptIDsKept the ones after those whose id it keeps.
const (
	writeAttemptsKept   = 256
	writeAttemptIDsKept = 4096
)

// writeAttempt is one write. key names what it wrote to, so an id reused
// for another target is not answered with this one's outcome.
type writeAttempt[T any] struct {
	id, key string
	done    chan struct{}
	result  T
	err     error
	// forgotten marks an attempt begin made for an id whose outcome was
	// dropped: it ran before, and nothing says what it did.
	forgotten bool
}

// attemptRefusals are a write's answers to a repeated id it does not run:
// one its first arrival used for another key (other), one whose outcome
// was dropped (forgotten). panicked is the outcome of a write that
// panicked.
type attemptRefusals struct {
	other, forgotten, panicked error
}

// run runs write as attempt id to key and records its outcome. A repeated
// id writes nothing: it waits on ctx for the first arrival's outcome and
// answers with it, or with refuse's answer. A panic in write still settles
// the attempt, so nothing waits on it forever.
func (l *writeAttempts[T]) run(ctx context.Context, id, key string, refuse attemptRefusals, write func() (T, error)) (result T, err error) {
	attempt, repeated := l.begin(id, key)
	if repeated {
		var zero T
		switch {
		case attempt.key != key:
			return zero, refuse.other
		case attempt.forgotten:
			return zero, refuse.forgotten
		}
		return attempt.wait(ctx)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			var zero T
			l.finish(attempt, zero, refuse.panicked)
			panic(recovered)
		}
		l.finish(attempt, result, err)
	}()
	return write()
}

// begin records attempt id to key, or returns the one already recorded
// under it (repeated). An id whose outcome was dropped returns a finished
// attempt marked forgotten, so a repeat writes nothing.
func (l *writeAttempts[T]) begin(id, key string) (attempt *writeAttempt[T], repeated bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if prior, ok := l.byID[id]; ok {
		return prior, true
	}
	if forgottenKey, ok := l.forgotten[id]; ok {
		attempt := &writeAttempt[T]{id: id, key: forgottenKey, done: make(chan struct{}), forgotten: true}
		close(attempt.done)
		return attempt, true
	}
	if l.byID == nil {
		l.byID = make(map[string]*writeAttempt[T])
	}
	attempt = &writeAttempt[T]{id: id, key: key, done: make(chan struct{})}
	l.byID[id] = attempt
	return attempt, false
}

// finish records what attempt did and releases everything waiting for it.
func (l *writeAttempts[T]) finish(attempt *writeAttempt[T], result T, err error) {
	attempt.result, attempt.err = result, err
	l.mu.Lock()
	defer l.mu.Unlock()
	l.finished = append(l.finished, attempt.id)
	for len(l.finished) > writeAttemptsKept {
		dropped := l.finished[0]
		if l.forgotten == nil {
			l.forgotten = make(map[string]string)
		}
		l.forgotten[dropped] = l.byID[dropped].key
		l.forgetting = append(l.forgetting, dropped)
		delete(l.byID, dropped)
		l.finished = l.finished[1:]
	}
	for len(l.forgetting) > writeAttemptIDsKept {
		delete(l.forgotten, l.forgetting[0])
		l.forgetting = l.forgetting[1:]
		l.overflowed = true
	}
	close(attempt.done)
}

// find returns attempt id to key, or reports it forgotten: received and its
// outcome dropped, or not held once ids have overflowed, so it may have
// been received. Neither means this process never received it.
func (l *writeAttempts[T]) find(id, key string) (attempt *writeAttempt[T], forgotten bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if attempt, ok := l.byID[id]; ok {
		if attempt.key == key {
			return attempt, false
		}
		return nil, false
	}
	if forgottenKey, ok := l.forgotten[id]; ok {
		return nil, forgottenKey == key
	}
	return nil, l.overflowed
}

// The states of an attempt's outcome (attemptOutcome).
const (
	attemptDone      = "done"
	attemptFailed    = "failed"
	attemptUnknown   = "unknown"   // this process never received it
	attemptForgotten = "forgotten" // it may have, and no longer keeps the outcome
)

// attemptOutcome is what a recorded attempt did, as every outcome method
// answers it: the result when it is done, a public code and message when
// it failed.
type attemptOutcome[T any] struct {
	state         string
	result        T
	code, message string
}

// attemptFailure is the code and message a failed attempt answers with
// when its error has no public ones of its own.
type attemptFailure struct {
	code, message string
}

// outcome answers what attempt id to key did, waiting on ctx for one still
// running. A failure answers with its error's public code and message
// (errorsx.PublicDetails), else with failed's: never the raw cause, which
// the transport logged on the host when the write answered its caller.
func (l *writeAttempts[T]) outcome(ctx context.Context, id, key string, failed attemptFailure) (attemptOutcome[T], error) {
	attempt, forgotten := l.find(id, key)
	switch {
	case forgotten:
		return attemptOutcome[T]{state: attemptForgotten}, nil
	case attempt == nil:
		return attemptOutcome[T]{state: attemptUnknown}, nil
	}
	result, err := attempt.wait(ctx)
	if ctx.Err() != nil {
		return attemptOutcome[T]{}, ctx.Err()
	}
	if err == nil {
		return attemptOutcome[T]{state: attemptDone, result: result}, nil
	}
	code, message, ok := errorsx.PublicDetails(err)
	if !ok {
		code, message = failed.code, failed.message
	}
	return attemptOutcome[T]{state: attemptFailed, code: code, message: message}, nil
}

// wait returns the attempt's outcome once it has one.
func (attempt *writeAttempt[T]) wait(ctx context.Context) (T, error) {
	select {
	case <-attempt.done:
		return attempt.result, attempt.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}
