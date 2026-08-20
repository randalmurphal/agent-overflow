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
	// sendInterruptedTurn writes the protocol-native terminal frame for an
	// interrupted turn. The adapter has already acknowledged the inbound
	// interrupt before this is called.
	sendInterruptedTurn(vars scenario.Vars)
}

// gate is one open waitSignal/stall block. An unnamed gate matches any
// advance; a named gate matches advances with the same name or none.
type gate struct {
	turn int
	name string
	ch   chan struct{}
}

type scenarioTurn struct {
	abort   chan struct{}
	aborted bool
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
	activeTurn      int           // turn currently executing scenario steps
	turns           map[int]*scenarioTurn
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
		turns:       make(map[int]*scenarioTurn),
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
	e.turns[n] = &scenarioTurn{abort: make(chan struct{})}
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
	vars["TURN_ID"] = turnIDForNumber(n)
	return vars
}

// turnIDForNumber is the one place the ${TURN_ID} spelling lives, shared
// by the vars snapshot, the interrupt's turn-id match, and the
// thread/fork cut.
func turnIDForNumber(n int) string { return "turn-" + strconv.Itoa(n) }

// forkedTurnIDs resolves a `thread/fork` cut over the turns this mock has
// run, in wire order, and reports whether the anchor is forkable at all.
//
// An empty lastTurnID keeps every turn that has BEGUN — mid-turn that
// includes the in-flight one, which is what codex's own interrupted fork
// snapshot copies. A named anchor must be a turn that has already
// FINISHED: codex refuses a lastTurnId naming an in-progress turn, and
// ok=false is how that refusal reaches the adapter.
func (e *engine) forkedTurnIDs(lastTurnID string) ([]string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	through := e.turnSeq
	if lastTurnID != "" {
		through = 0
		for n := 1; n <= e.turnSeq; n++ {
			if lastTurnID != turnIDForNumber(n) {
				continue
			}
			if e.turns[n] != nil {
				// Begun and not yet finished — the refused anchor.
				return nil, false
			}
			through = n
			break
		}
		if through == 0 {
			return nil, false
		}
	}
	ids := make([]string, 0, through)
	for n := 1; n <= through; n++ {
		ids = append(ids, turnIDForNumber(n))
	}
	return ids, true
}

// currentTurn returns the most recently begun user-turn number (0 before the
// first). Used by reports that belong to a turn already under way rather than
// to one they begin, such as a mid-turn steer.
func (e *engine) currentTurn() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.turnSeq
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
		e.mu.Lock()
		delete(e.turns, n)
		e.mu.Unlock()
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
	e.startTurn(n)
	defer e.finishTurn(n)

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
			// Silent is an intentionally hung provider turn, not a completed
			// empty scenario. Keep ownership until interrupt so the adapter can
			// emit the same terminal sequence as any other in-flight turn.
			<-e.turnAbortSignal(n)
			e.adapter.sendInterruptedTurn(e.varsForTurn(n))
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
	vars := e.varsForTurn(n)
	if e.runSteps(vars, n, steps, true) {
		e.adapter.sendInterruptedTurn(vars)
		return
	}
	if n >= len(turns) {
		e.reportScenarioDone(n)
	}
}

func (e *engine) startTurn(n int) {
	e.mu.Lock()
	if e.turns[n] == nil {
		// Unit tests may execute runTurn directly instead of going through
		// beginTurn. Production turns always arrive through beginTurn.
		e.turns[n] = &scenarioTurn{abort: make(chan struct{})}
	}
	e.activeTurn = n
	e.mu.Unlock()
}

func (e *engine) finishTurn(n int) {
	e.mu.Lock()
	if e.activeTurn == n {
		e.activeTurn = 0
	}
	delete(e.turns, n)
	e.mu.Unlock()
}

// interruptTurn marks the current scenario turn aborted and releases a gate
// blocked by waitSignal or stall. An interrupt received while no turn exists is
// an out-of-band no-op and cannot poison the next turn. A non-empty turnID must
// match the selected Codex turn.
func (e *engine) interruptTurn(turnID string) bool {
	e.mu.Lock()
	n := e.activeTurn
	if n == 0 {
		for candidate := 1; candidate <= e.turnSeq; candidate++ {
			if e.turns[candidate] != nil {
				n = candidate
				break
			}
		}
	}
	if n == 0 || (turnID != "" && turnID != turnIDForNumber(n)) {
		e.mu.Unlock()
		return false
	}
	turn := e.turns[n]
	if turn == nil || turn.aborted {
		e.mu.Unlock()
		return false
	}
	turn.aborted = true
	close(turn.abort)
	if g := e.gate; g != nil && g.turn == n {
		e.gate = nil
		close(g.ch)
	}
	e.mu.Unlock()
	e.rep.report(control.Report{Kind: control.ReportTurnInterrupted, Turn: n})
	return true
}

func (e *engine) turnAborted(n int) bool {
	if n == 0 {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	turn := e.turns[n]
	return turn != nil && turn.aborted
}

func (e *engine) turnAbortSignal(n int) <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	turn := e.turns[n]
	if turn == nil {
		turn = &scenarioTurn{abort: make(chan struct{})}
		e.turns[n] = turn
	}
	return turn.abort
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
func (e *engine) openGate(turn int, name string) <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	if state := e.turns[turn]; state != nil && state.aborted {
		return nil
	}
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
	g := &gate{turn: turn, name: name, ch: make(chan struct{})}
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
