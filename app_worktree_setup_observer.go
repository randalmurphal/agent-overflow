package main

import (
	"bytes"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/worktreesetup"
)

// The emission half of a chat-thread worktree setup run. The lifecycle half —
// kickoff, settlement, cancellation, the snapshot, and the durable column —
// lives in app_worktree_setup.go.
//
// Everything a run pushes onto the `worktree:setup` channel goes through the
// observer below, including the started frame. One emitter is what guarantees
// a frame cannot disagree with the record it describes, and it is what lets
// the sequence a chunk carries and its position on the wire be decided in the
// same critical section.

// worktreeSetupObserver is the run's single emitter. It owns the coalescing
// buffer that turns a byte stream into whole-line frames and the per-run
// sequence those frames carry.
type worktreeSetupObserver struct {
	app *App
	run *worktreeSetupRun

	// mu guards the coalescing buffer AND serialises emission, so a frame's
	// sequence and its position on the wire cannot disagree.
	mu          sync.Mutex
	pending     []byte
	pendingStep int
	timer       *time.Timer
}

func newWorktreeSetupObserver(app *App, run *worktreeSetupRun) *worktreeSetupObserver {
	return &worktreeSetupObserver{app: app, run: run, pendingStep: -1}
}

// RunStarted announces the run. It reports the record's own wire steps rather
// than converting the ones handed in: both come from ResolveSteps over the same
// config, and emitting the record's copy is what guarantees a client's step
// list is the one the snapshot RPC would hand it.
func (o *worktreeSetupObserver) RunStarted(_ []worktreesetup.Step) {
	o.app.emitEvent(worktreeSetupChannel, WorktreeSetupEvent{
		Phase:        worktreeSetupPhaseStarted,
		ThreadID:     o.run.threadID,
		RunID:        o.run.id,
		WorktreePath: o.run.worktreePath,
		Steps:        o.run.steps,
		StartedAt:    o.run.startedAt,
	})
}

func (o *worktreeSetupObserver) StepStarted(index int) {
	o.app.setWorktreeSetupStepStatus(o.run, index, worktreeSetupStepRunning)
	o.app.emitEvent(worktreeSetupChannel, WorktreeSetupEvent{
		Phase:     worktreeSetupPhaseStepStarted,
		ThreadID:  o.run.threadID,
		RunID:     o.run.id,
		StepIndex: index,
	})
}

func (o *worktreeSetupObserver) Output(index int, chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	// The tail is written first and unconditionally: the snapshot reads it,
	// and a client that never sees a single frame still gets the transcript.
	if _, err := o.run.tail.Write(chunk); err != nil {
		log.Printf("thread %s: worktree setup output tail: %v", o.run.threadID, err)
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pendingStep != index {
		o.flushLocked()
		o.pendingStep = index
	}
	o.pending = append(o.pending, chunk...)
	if bytes.IndexByte(chunk, '\n') >= 0 || len(o.pending) >= worktreeSetupFlushBytes {
		o.flushLocked()
		return
	}
	// Output that never emits a newline (a progress bar redrawing on \r)
	// still has to reach the panel, so a partial buffer is time-bounded.
	if o.timer == nil {
		o.timer = time.AfterFunc(worktreeSetupFlushInterval, o.flushAfterTimeout)
		return
	}
	o.timer.Reset(worktreeSetupFlushInterval)
}

func (o *worktreeSetupObserver) StepFinished(index int, err error) {
	o.mu.Lock()
	o.flushLocked()
	o.mu.Unlock()

	status := worktreeSetupStepSucceeded
	message := ""
	if err != nil {
		status = worktreeSetupStepFailed
		message = err.Error()
	}
	o.app.setWorktreeSetupStepStatus(o.run, index, status)
	o.app.emitEvent(worktreeSetupChannel, WorktreeSetupEvent{
		Phase:     worktreeSetupPhaseStepFinished,
		ThreadID:  o.run.threadID,
		RunID:     o.run.id,
		StepIndex: index,
		State:     status,
		Error:     message,
	})
}

func (o *worktreeSetupObserver) RunFinished(err error) {
	o.mu.Lock()
	if o.timer != nil {
		o.timer.Stop()
		o.timer = nil
	}
	o.flushLocked()
	o.mu.Unlock()
	o.app.finishThreadWorktreeSetup(o.run, err)
}

// flushAfterTimeout is the timer's callback. It is a no-op when the buffer was
// already drained, which is what makes a timer that fires after the run
// settled harmless.
func (o *worktreeSetupObserver) flushAfterTimeout() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.flushLocked()
}

// flushLocked emits whatever is buffered. Callers hold o.mu.
func (o *worktreeSetupObserver) flushLocked() {
	if o.timer != nil {
		o.timer.Stop()
	}
	if len(o.pending) == 0 {
		return
	}
	chunk := string(o.pending)
	o.pending = o.pending[:0]
	o.app.emitEvent(worktreeSetupChannel, WorktreeSetupEvent{
		Phase:     worktreeSetupPhaseOutput,
		ThreadID:  o.run.threadID,
		RunID:     o.run.id,
		StepIndex: o.pendingStep,
		Seq:       o.run.seq.Add(1),
		Chunk:     chunk,
	})
}

func (a *App) setWorktreeSetupStepStatus(run *worktreeSetupRun, index int, status string) {
	a.worktreeSetupMu.Lock()
	defer a.worktreeSetupMu.Unlock()
	if index < 0 || index >= len(run.statuses) {
		return
	}
	run.statuses[index] = status
}
