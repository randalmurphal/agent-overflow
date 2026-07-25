package engine

import (
	"errors"
	"fmt"

	"agent-overflow/internal/store"
)

// This file is the command-loop boundary: every public Engine method turns into
// one of the command values below, and the loop is the only place they are
// executed. Keeping the plumbing here leaves engine.go about the engine's state
// and lifecycle.

type response struct {
	err    error
	starts []*runnerStartFuture
}

type runnerStartFuture struct {
	key  RunKey
	done chan response
}
type initCommand struct{ reply chan response }
type closeCommand struct{ reply chan response }
type syncCommand struct{ reply chan response }
type startCommand struct {
	item          store.WorkItem
	waitForStarts bool
	reply         chan response
}
type cancelCommand struct {
	itemID string
	reply  chan response
}
type parkDispositionCommand struct {
	itemID string
	reply  chan response
}
type rerunFailedCommand struct {
	itemID string
	reply  chan response
}
type resolveDispositionCommand struct {
	itemID string
	reply  chan response
}
type resumeCommand struct {
	itemID      string
	targetPhase string
	reply       chan response
}
type answerCommand struct {
	itemID string
	answer string
	reply  chan response
}
type takeoverCommand struct {
	itemID string
	reply  chan response
}
type completeTakeoverCommand struct {
	itemID string
	reply  chan response
}
type pauseCommand struct {
	paused        bool
	waitForStarts bool
	reply         chan response
}
type pauseStateCommand struct {
	result *bool
	reply  chan response
}
type humanGateCommand struct {
	itemID   string
	decision HumanDecision
	note     string
	reply    chan response
}
type retryUnitCommand struct {
	itemID string
	unitID string
	note   string
	reply  chan response
}
type dropUnitCommand struct {
	itemID string
	unitID string
	note   string
	reply  chan response
}
type takeoverUnitCommand struct {
	itemID string
	unitID string
	reply  chan response
}
type completionCommand struct {
	key     RunKey
	outcome Outcome
}
type runnerStartCommand struct {
	key    RunKey
	future *runnerStartFuture
	err    error
}

func (e *Engine) request(command any) error {
	e.lifeMu.Lock()
	if !e.started {
		e.lifeMu.Unlock()
		return fmt.Errorf("workflow engine: not started")
	}
	_, closeRequest := command.(closeCommand)
	if e.closing && !closeRequest {
		e.lifeMu.Unlock()
		return fmt.Errorf("workflow engine: closing")
	}
	reply := make(chan response, 1)
	switch command := command.(type) {
	case initCommand:
		command.reply = reply
		e.commands <- command
	case closeCommand:
		command.reply = reply
		e.commands <- command
	case syncCommand:
		command.reply = reply
		e.commands <- command
	case startCommand:
		command.reply = reply
		e.commands <- command
	case cancelCommand:
		command.reply = reply
		e.commands <- command
	case parkDispositionCommand:
		command.reply = reply
		e.commands <- command
	case rerunFailedCommand:
		command.reply = reply
		e.commands <- command
	case resolveDispositionCommand:
		command.reply = reply
		e.commands <- command
	case resumeCommand:
		command.reply = reply
		e.commands <- command
	case answerCommand:
		command.reply = reply
		e.commands <- command
	case takeoverCommand:
		command.reply = reply
		e.commands <- command
	case completeTakeoverCommand:
		command.reply = reply
		e.commands <- command
	case pauseCommand:
		command.reply = reply
		e.commands <- command
	case pauseStateCommand:
		command.reply = reply
		e.commands <- command
	case humanGateCommand:
		command.reply = reply
		e.commands <- command
	case retryUnitCommand:
		command.reply = reply
		e.commands <- command
	case dropUnitCommand:
		command.reply = reply
		e.commands <- command
	case takeoverUnitCommand:
		command.reply = reply
		e.commands <- command
	default:
		e.lifeMu.Unlock()
		return fmt.Errorf("workflow engine: unsupported command %T", command)
	}
	e.lifeMu.Unlock()
	return waitEngineResponse(<-reply)
}

func waitEngineResponse(result response) error {
	errs := []error{result.err}
	for _, start := range result.starts {
		errs = append(errs, waitEngineResponse(<-start.done))
	}
	return errors.Join(errs...)
}

func (e *Engine) commandResponse(err error) response {
	starts := append([]*runnerStartFuture(nil), e.commandStarts...)
	e.commandStarts = nil
	return response{err: err, starts: starts}
}

func (e *Engine) itemCommandResponse(itemID string, err error) response {
	starts := make([]*runnerStartFuture, 0, len(e.commandStarts))
	for _, start := range e.commandStarts {
		if start.key.ItemID == itemID {
			starts = append(starts, start)
		}
	}
	e.commandStarts = nil
	return response{err: err, starts: starts}
}

func (e *Engine) syncResponse() response {
	starts := make([]*runnerStartFuture, 0, len(e.inflightStarts))
	for start := range e.inflightStarts {
		starts = append(starts, start)
	}
	return response{starts: starts}
}

func settleRunnerStart(start *runnerStartFuture, result response) {
	select {
	case start.done <- result:
	default:
	}
}

// drainDeferred runs the follow-up work transitions queued during the command
// just processed — a finished child run re-entering the call phase that was
// waiting on it. Draining until empty is deliberate: completing a parent can
// finish it too, and that completion has to reach *its* parent in the same
// serialized pass. Depth is bounded by MaxCallDepth, because that is what bounds
// how deep a call tree can be.
func (e *Engine) drainDeferred() {
	drained := false
	for len(e.deferred) > 0 {
		pending := e.deferred
		e.deferred = nil
		drained = true
		for _, work := range pending {
			if err := work.run(); err != nil {
				e.emitError(work.itemID, err)
			}
		}
	}
	if drained {
		// A settled call phase releases nothing of its own, but the phase it
		// advanced into may free or need capacity like any other.
		_ = e.startWaiting() // startWaiting emits item-scoped failures itself.
	}
}

func (e *Engine) loop() {
	defer close(e.done)
	for command := range e.commands {
		e.commandStarts = nil
		var err error
		switch command := command.(type) {
		case initCommand:
			err = e.rebuild()
			if err == nil {
				err = e.startWaiting()
			}
			command.reply <- e.commandResponse(err)
		case startCommand:
			err = e.startNewItem(command.item, nil)
			if command.waitForStarts {
				command.reply <- e.itemCommandResponse(command.item.ID, err)
			} else {
				e.commandStarts = nil
				command.reply <- response{err: err}
			}
		case cancelCommand:
			err = e.cancel(command.itemID)
			e.commandStarts = nil
			command.reply <- response{err: err}
		case parkDispositionCommand:
			err = e.parkDisposition(command.itemID)
			e.commandStarts = nil
			command.reply <- response{err: err}
		case rerunFailedCommand:
			err = e.rerunFailed(command.itemID)
			command.reply <- e.itemCommandResponse(command.itemID, err)
		case resolveDispositionCommand:
			err = e.resolveDisposition(command.itemID)
			e.commandStarts = nil
			command.reply <- response{err: err}
		case resumeCommand:
			err = e.resume(command.itemID, command.targetPhase)
			command.reply <- e.itemCommandResponse(command.itemID, err)
		case answerCommand:
			err = e.answer(command.itemID, command.answer)
			command.reply <- e.itemCommandResponse(command.itemID, err)
		case takeoverCommand:
			err = errors.Join(e.takeOver(command.itemID), e.startWaiting())
			e.commandStarts = nil
			command.reply <- response{err: err}
		case completeTakeoverCommand:
			err = e.completeTakeover(command.itemID)
			command.reply <- e.itemCommandResponse(command.itemID, err)
		case pauseCommand:
			if e.paused != command.paused {
				e.paused = command.paused
				e.emitEngineState()
			}
			if !e.paused {
				startErr := e.startWaiting()
				if command.waitForStarts {
					err = startErr
				}
			}
			if command.waitForStarts {
				command.reply <- e.commandResponse(err)
			} else {
				e.commandStarts = nil
				command.reply <- response{err: err}
			}
		case pauseStateCommand:
			*command.result = e.paused
			command.reply <- response{}
		case humanGateCommand:
			err = errors.Join(e.resolveHumanGate(command.itemID, command.decision, command.note), e.startWaiting())
			command.reply <- e.itemCommandResponse(command.itemID, err)
		case retryUnitCommand:
			err = e.retryUnit(command.itemID, command.unitID, command.note)
			command.reply <- e.itemCommandResponse(command.itemID, err)
		case dropUnitCommand:
			err = e.dropUnit(command.itemID, command.unitID, command.note)
			command.reply <- e.itemCommandResponse(command.itemID, err)
		case takeoverUnitCommand:
			// A taken-over unit releases its provider slot; hand it to the
			// longest-waiting work before answering.
			err = errors.Join(e.takeOverUnit(command.itemID, command.unitID), e.startWaiting())
			e.commandStarts = nil
			command.reply <- response{err: err}
		case runnerStartCommand:
			err = e.finishRunnerStart(command)
			if err != nil {
				e.emitError(command.key.ItemID, err)
			}
			_ = e.startWaiting() // startWaiting reports item-scoped failures itself.
			delete(e.inflightStarts, command.future)
			e.commandStarts = nil
			settleRunnerStart(command.future, response{err: err})
		case completionCommand:
			if err = e.complete(command.key, command.outcome); err != nil {
				e.emitError(command.key.ItemID, err)
			}
			_ = e.startWaiting() // startWaiting emits item-scoped asynchronous errors itself.
		case syncCommand:
			e.drainDeferred()
			command.reply <- e.syncResponse()
		case closeCommand:
			for _, item := range e.items {
				if item.runnerStarting && item.runnerStartCancel != nil {
					item.runnerStartCancel()
				}
				if item.fan == nil {
					continue
				}
				for _, unit := range item.fan.all() {
					if unit.runnerStarting {
						clearUnitStart(unit)
					}
				}
			}
			for start := range e.inflightStarts {
				settleRunnerStart(start, response{err: fmt.Errorf("workflow engine closed before runner startup settled")})
				delete(e.inflightStarts, start)
			}
			command.reply <- response{}
			return
		}
		e.drainDeferred()
	}
}
