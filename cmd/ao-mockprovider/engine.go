package main

import (
	"log"
	"os"
	"strconv"
	"sync"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
)

// turnQueueCap bounds queued turn triggers. The app paces turns at
// human/agent speed; a full queue means the engine is wedged and
// dropping (loudly) beats blocking the stdin reader, which must stay
// live to route approval responses and detect EOF.
const turnQueueCap = 128

// protocolAdapter is the per-protocol seam the step engine calls for
// the one step whose wire shape differs between Claude and Codex.
type protocolAdapter interface {
	// sendApproval writes the protocol-native approval request. The
	// returned channel yields the decision (true = allow) exactly once
	// when the app responds; cancel unregisters the waiter after a
	// timeout so a late response is dropped instead of leaking.
	sendApproval(step *scenario.ApprovalStep, vars scenario.Vars) (decision <-chan bool, cancel func(), err error)
}

// gate is one open waitSignal/stall block. An unnamed gate matches any
// advance; a named gate matches advances with the same name or none.
type gate struct {
	name string
	ch   chan struct{}
}

// engine executes scenario steps. One goroutine (run) owns step
// execution; protocol adapters feed it turn triggers and the control
// poller feeds it live commands.
type engine struct {
	sc          *scenario.Scenario
	fixtureRoot string
	cwd         string
	w           *lineWriter
	rep         *reporter
	adapter     protocolAdapter
	// exitFn is os.Exit in production; tests substitute a recorder.
	exitFn func(code int)

	mu              sync.Mutex
	base            scenario.Vars // SESSION_ID / THREAD_ID / CWD
	turnSeq         int           // last begun user-turn number (1-based)
	doneReported    bool
	gate            *gate
	pendingAdvances []string

	turnCh chan int
}

func newEngine(sc *scenario.Scenario, fixtureRoot, cwd string, w *lineWriter, rep *reporter, base scenario.Vars) *engine {
	return &engine{
		sc:          sc,
		fixtureRoot: fixtureRoot,
		cwd:         cwd,
		w:           w,
		rep:         rep,
		exitFn:      os.Exit,
		base:        base,
		turnCh:      make(chan int, turnQueueCap),
	}
}

// beginTurn allocates the next 1-based user-turn number and returns it
// with a vars snapshot for that turn. Called by the protocol adapter
// the moment a turn trigger arrives so codex response templates see
// the new ${TURN}/${TURN_ID} before the turn's steps run.
func (e *engine) beginTurn() (int, scenario.Vars) {
	e.mu.Lock()
	e.turnSeq++
	n := e.turnSeq
	e.mu.Unlock()
	return n, e.varsForTurn(n)
}

// varsForTurn snapshots base vars plus the per-turn TURN / TURN_ID.
func (e *engine) varsForTurn(n int) scenario.Vars {
	e.mu.Lock()
	defer e.mu.Unlock()
	vars := make(scenario.Vars, len(e.base)+2)
	for k, v := range e.base {
		vars[k] = v
	}
	vars["TURN"] = strconv.Itoa(n)
	vars["TURN_ID"] = "turn-" + strconv.Itoa(n)
	return vars
}

// currentVars snapshots vars for the most recently begun turn — used
// by control "emit" commands and non-turn codex responses.
func (e *engine) currentVars() scenario.Vars {
	e.mu.Lock()
	n := e.turnSeq
	e.mu.Unlock()
	return e.varsForTurn(n)
}

// setThreadID rebinds ${THREAD_ID}/${SESSION_ID} when the app resumes
// a Codex thread (thread/resume echoes the requested id).
func (e *engine) setThreadID(id string) {
	e.mu.Lock()
	e.base["THREAD_ID"] = id
	e.base["SESSION_ID"] = id
	e.mu.Unlock()
}

// enqueueTurn hands a begun turn to the engine goroutine.
func (e *engine) enqueueTurn(n int) {
	select {
	case e.turnCh <- n:
	default:
		log.Printf("turn queue full; dropping turn %d — scenario and app are out of sync", n)
	}
}

// run is the engine goroutine: onStart steps, then queued turns forever
// (process lifetime is owned by stdin EOF / exit steps / signals).
func (e *engine) run() {
	e.runSteps(e.varsForTurn(0), 0, e.sc.OnStart, true)
	if len(e.sc.Turns) == 0 {
		e.reportScenarioDone(0)
	}
	for n := range e.turnCh {
		e.runTurn(n)
	}
}

func (e *engine) runTurn(n int) {
	turns := e.sc.Turns
	var steps []scenario.Step
	var label string
	switch {
	case n <= len(turns):
		steps, label = turns[n-1].Steps, turns[n-1].Label
	default:
		switch e.sc.AfterTurns {
		case "silent":
			e.rep.report(control.Report{Kind: control.ReportTurnStarted, Turn: n, Detail: "afterTurns:silent"})
			return
		case "exit":
			e.rep.report(control.Report{Kind: control.ReportTurnStarted, Turn: n, Detail: "afterTurns:exit"})
			e.terminate(0)
			return
		default: // "" and "repeatLast"
			if len(turns) == 0 {
				log.Printf("turn %d arrived but scenario %q has no turns; ignoring", n, e.sc.Name)
				return
			}
			steps, label = turns[len(turns)-1].Steps, turns[len(turns)-1].Label
		}
	}
	e.rep.report(control.Report{Kind: control.ReportTurnStarted, Turn: n, Detail: label})
	e.runSteps(e.varsForTurn(n), n, steps, true)
	if n >= len(turns) {
		e.reportScenarioDone(n)
	}
}

func (e *engine) reportScenarioDone(turn int) {
	e.mu.Lock()
	already := e.doneReported
	e.doneReported = true
	e.mu.Unlock()
	if !already {
		e.rep.report(control.Report{Kind: control.ReportScenarioDone, Turn: turn})
	}
}

// handleCommand executes one live control-channel command. Runs on the
// poll goroutine.
func (e *engine) handleCommand(cmd control.Command) {
	switch cmd.Type {
	case control.CommandAdvance:
		e.advance(cmd.Name)
	case control.CommandEmit:
		vars := e.currentVars()
		for _, line := range cmd.Lines {
			e.w.writeLine(vars.Substitute(line), 0, 0)
		}
	case control.CommandExit:
		e.terminate(cmd.Code)
	default:
		log.Printf("unknown control command type %q (ignored)", cmd.Type)
	}
}

// advance releases the open gate when it matches, otherwise buffers the
// advance so a command racing the gate's opening is not lost.
func (e *engine) advance(name string) {
	e.mu.Lock()
	if g := e.gate; g != nil && gateMatches(g.name, name) {
		e.gate = nil
		e.mu.Unlock()
		close(g.ch)
		return
	}
	e.pendingAdvances = append(e.pendingAdvances, name)
	e.mu.Unlock()
	log.Printf("advance %q buffered (no matching open gate yet)", name)
}

// openGate registers a gate for the engine goroutine to block on. A
// buffered matching advance is consumed instead (nil return = don't
// block).
func (e *engine) openGate(name string) <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, pending := range e.pendingAdvances {
		if gateMatches(name, pending) {
			e.pendingAdvances = append(e.pendingAdvances[:i], e.pendingAdvances[i+1:]...)
			return nil
		}
	}
	if e.gate != nil {
		// Steps run on one goroutine, so two open gates means a bug here.
		log.Printf("BUG: opening gate %q while gate %q is already open", name, e.gate.name)
	}
	g := &gate{name: name, ch: make(chan struct{})}
	e.gate = g
	return g.ch
}

// gateMatches: an empty advance releases any gate, an unnamed gate is
// released by any advance, otherwise names must agree.
func gateMatches(gateName, advanceName string) bool {
	return advanceName == "" || gateName == "" || advanceName == gateName
}

// terminate reports and exits. The exiting report is synchronous so it
// lands before the process dies.
func (e *engine) terminate(code int) {
	e.rep.report(control.Report{Kind: control.ReportExiting, Detail: strconv.Itoa(code)})
	e.exitFn(code)
}
