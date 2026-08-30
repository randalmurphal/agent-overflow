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

// maxPendingAdvances bounds the per-turn buffered-advance backlog. The
// buffer exists only to tolerate an advance command that RACES the gate
// it targets by a few milliseconds; a dozen of them queued behind one
// gate is a driving mistake, not a race, and an unbounded slice on a
// soak-length process is a leak. Past the cap an advance is discarded
// with a report rather than parked where nothing will ever read it.
const maxPendingAdvances = 16

// gate is one open waitSignal/stall block. An unnamed gate matches any
// advance; a named gate matches advances with the same name or none.
type gate struct {
	turn int
	name string
	ch   chan struct{}
}

// pendingAdvance is one buffered advance command, stamped with the turn
// it arrived during.
//
// The stamp is what keeps the race tolerance from becoming a leak. An
// advance that matched nothing is parked so a gate opening microseconds
// later still sees it — but only within the turn it was issued in.
// Without the stamp, an unnamed advance stranded in turn 1 silently
// released the FIRST gate of turn 4, three turns and minutes later,
// which reads as a mock that skipped a step for no reason.
type pendingAdvance struct {
	turn int
	name string
}

type scenarioTurn struct {
	abort   chan struct{}
	done    chan struct{}
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

	mu   sync.Mutex
	base scenario.Vars // SESSION_ID / THREAD_ID / CWD
	// turnVars holds extra ${VAR} bindings scoped to one turn, set by an
	// adapter that knows something about the turn the scenario file cannot —
	// the text of a provider-queued message, which only exists at dispatch
	// time. Merged over base in varsForTurn and dropped with the turn.
	turnVars   map[int]scenario.Vars
	turnSeq    int // last begun user-turn number (1-based)
	activeTurn int // turn currently executing scenario steps
	turns      map[int]*scenarioTurn
	// doneTurns records which turn numbers already reported scenario_done.
	// Per TURN, not per process: under afterTurns:repeatLast every turn
	// past the last scripted one runs the same steps and finishes just as
	// completely, so a once-per-process latch left turns 2..N reporting
	// nothing and a per-turn await hanging forever.
	doneTurns       map[int]bool
	gate            *gate
	pendingAdvances []pendingAdvance

	turnCh chan int

	// startupDelay fires once, immediately before the adapter writes the
	// first frame proving the provider is up. sync.Once rather than a
	// flag because both adapters call it from their stdin goroutine while
	// the engine goroutine is already running.
	startupDelay sync.Once
}

// awaitStartupDelay blocks for the scenario's startupDelayMs the FIRST
// time an adapter is about to announce itself, and returns immediately
// every time after. See scenario.Scenario.StartupDelayMs.
func (e *engine) awaitStartupDelay() {
	e.startupDelay.Do(func() {
		if e.sc.StartupDelayMs > 0 {
			log.Printf("startupDelayMs: holding the first provider frame for %dms", e.sc.StartupDelayMs)
			sleepMs(e.sc.StartupDelayMs)
		}
	})
}

// providerVersion is the version the mock claims to be: the scenario's
// override when it declares one, otherwise the mock's own (which sits
// above every version gate the app applies).
func (e *engine) providerVersion() string {
	if v := e.sc.ProviderVersion; v != "" {
		return v
	}
	return mockVersionNumber
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
		turnVars:    make(map[int]scenario.Vars),
		turns:       make(map[int]*scenarioTurn),
		doneTurns:   make(map[int]bool),
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
	e.turns[n] = &scenarioTurn{abort: make(chan struct{}), done: make(chan struct{})}
	e.mu.Unlock()
	return n, e.varsForTurn(n)
}

// varsForTurn snapshots base vars plus the per-turn TURN / TURN_ID.
func (e *engine) varsForTurn(n int) scenario.Vars {
	e.mu.Lock()
	defer e.mu.Unlock()
	vars := make(scenario.Vars, len(e.base)+2+len(e.turnVars[n]))
	for k, v := range e.base {
		vars[k] = v
	}
	for k, v := range e.turnVars[n] {
		vars[k] = v
	}
	vars["TURN"] = strconv.Itoa(n)
	vars["TURN_ID"] = turnIDForNumber(n)
	return vars
}

// setTurnVars binds extra ${VAR} values for one turn. Called before
// enqueueTurn so the turn's very first step already sees them.
func (e *engine) setTurnVars(n int, extra scenario.Vars) {
	if len(extra) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	bound := e.turnVars[n]
	if bound == nil {
		bound = make(scenario.Vars, len(extra))
		e.turnVars[n] = bound
	}
	for k, v := range extra {
		bound[k] = v
	}
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

// turnStatus reports what this process knows about turnID out of its own
// turn ledger: whether it began that turn at all, and whether the turn has
// finished.
//
// The began=false answer is IGNORANCE, not evidence, and the adapters have
// to treat it that way: the mock keeps no rollout, so a thread it resumed
// has history it cannot see. Cold and fork-fallback rollbacks land there via
// a throwaway resume whose ledger starts empty. See
// codex_revert.go#revertAnchorState.
func (e *engine) turnStatus(turnID string) (began, finished bool) {
	if turnID == "" {
		return false, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for n := 1; n <= e.turnSeq; n++ {
		if turnID == turnIDForNumber(n) {
			return true, e.turns[n] == nil
		}
	}
	return false, false
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
		e.turns[n] = &scenarioTurn{abort: make(chan struct{}), done: make(chan struct{})}
	}
	e.activeTurn = n
	e.mu.Unlock()
}

func (e *engine) finishTurn(n int) {
	e.mu.Lock()
	if e.activeTurn == n {
		e.activeTurn = 0
	}
	turn := e.turns[n]
	delete(e.turns, n)
	delete(e.turnVars, n)
	// The dedupe is within-turn only, so the entry dies with the turn —
	// a soak runs unboundedly many of them.
	delete(e.doneTurns, n)
	dropped := e.dropTurnAdvancesLocked(n)
	if turn != nil {
		close(turn.done)
	}
	e.mu.Unlock()
	e.reportDroppedAdvances(n, dropped)
}

// shutdownTurn interrupts a matching live turn and waits until its adapter has
// emitted the terminal frame. thread/revert uses this stronger boundary;
// unlike turn/interrupt, its response is sent only after shutdown and history
// persistence are complete upstream.
func (e *engine) shutdownTurn(turnID string) bool {
	e.mu.Lock()
	var turn *scenarioTurn
	for n := 1; n <= e.turnSeq; n++ {
		if turnID == turnIDForNumber(n) {
			turn = e.turns[n]
			break
		}
	}
	e.mu.Unlock()
	if turn == nil {
		return false
	}
	e.interruptTurn(turnID)
	<-turn.done
	return true
}

// reportDroppedAdvances says on the control channel what a turn boundary
// threw away. An advance that outlived its turn is a command a test
// issued and nothing consumed; leaving that silent is how a scenario
// appears to skip a gate for no reason.
func (e *engine) reportDroppedAdvances(turn int, dropped []pendingAdvance) {
	for _, pending := range dropped {
		log.Printf("advance %q discarded at the end of turn %d (its gate never opened)", pending.name, pending.turn)
		e.rep.report(control.Report{
			Kind:   control.ReportAdvanceBuffered,
			Turn:   turn,
			Gate:   pending.name,
			Detail: control.AdvanceDroppedDetail,
		})
	}
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
	// An interrupt ends the turn, so its buffered advances end with it —
	// otherwise a stranded advance survives the abort and releases the
	// first gate of whatever turn comes next.
	dropped := e.dropTurnAdvancesLocked(n)
	e.mu.Unlock()
	e.rep.report(control.Report{Kind: control.ReportTurnInterrupted, Turn: n})
	e.reportDroppedAdvances(n, dropped)
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
		turn = &scenarioTurn{abort: make(chan struct{}), done: make(chan struct{})}
		e.turns[n] = turn
	}
	return turn.abort
}

// reportScenarioDone posts scenario_done once per TURN.
//
// Once per PROCESS is what this used to be, and under the default
// afterTurns:repeatLast — where turns 2..N re-run the last scripted turn
// and finish exactly as turn 1 did — that latch meant every turn after
// the first reported nothing at all. A test awaiting the boundary for
// turn 2 waited forever on a mock that had already finished.
//
// The dedupe is still needed per turn: runTurn can reach the report from
// more than one path within one turn number.
func (e *engine) reportScenarioDone(turn int) {
	e.mu.Lock()
	already := e.doneTurns[turn]
	e.doneTurns[turn] = true
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
//
// Both outcomes report, because "my advance did nothing" is otherwise
// unobservable from outside the process: the buffered case looks
// identical to a command that was never delivered.
func (e *engine) advance(name string) {
	e.mu.Lock()
	if g := e.gate; g != nil && gateMatches(g.name, name) {
		e.gate = nil
		released := g.name
		e.mu.Unlock()
		close(g.ch)
		e.rep.report(control.Report{Kind: control.ReportAdvanceReleased, Turn: g.turn, Gate: released})
		return
	}
	turn := e.currentTurnLocked()
	openGate := ""
	if e.gate != nil {
		openGate = e.gate.name
	}
	dropped := len(e.pendingAdvances) >= maxPendingAdvances
	if !dropped {
		e.pendingAdvances = append(e.pendingAdvances, pendingAdvance{turn: turn, name: name})
	}
	e.mu.Unlock()

	rep := control.Report{Kind: control.ReportAdvanceBuffered, Turn: turn, Gate: name, OpenGate: openGate}
	if dropped {
		rep.Detail = control.AdvanceDroppedDetail
		log.Printf("advance %q DISCARDED: %d advances already buffered for turn %d", name, maxPendingAdvances, turn)
	} else {
		log.Printf("advance %q buffered for turn %d (no matching open gate yet)", name, turn)
	}
	e.rep.report(rep)
}

// currentTurnLocked is the turn an out-of-band command belongs to: the
// one executing steps, or — between enqueue and pickup — the last one
// begun. Callers hold mu.
func (e *engine) currentTurnLocked() int {
	if e.activeTurn != 0 {
		return e.activeTurn
	}
	return e.turnSeq
}

// dropTurnAdvancesLocked discards every buffered advance from turn n or
// earlier. Called at both ends a turn can finish through (completion and
// interrupt), which is what bounds the race tolerance to one turn.
// Callers hold mu.
func (e *engine) dropTurnAdvancesLocked(n int) []pendingAdvance {
	kept := e.pendingAdvances[:0]
	var dropped []pendingAdvance
	for _, pending := range e.pendingAdvances {
		if pending.turn <= n {
			dropped = append(dropped, pending)
			continue
		}
		kept = append(kept, pending)
	}
	e.pendingAdvances = kept
	return dropped
}

// openGate registers a gate for the engine goroutine to block on. A
// buffered matching advance from THIS turn is consumed instead (nil
// return = don't block).
func (e *engine) openGate(turn int, name string) <-chan struct{} {
	e.mu.Lock()
	if state := e.turns[turn]; state != nil && state.aborted {
		e.mu.Unlock()
		return nil
	}
	for i, pending := range e.pendingAdvances {
		if pending.turn != turn || !gateMatches(name, pending.name) {
			continue
		}
		e.pendingAdvances = append(e.pendingAdvances[:i], e.pendingAdvances[i+1:]...)
		e.mu.Unlock()
		e.rep.report(control.Report{Kind: control.ReportAdvanceReleased, Turn: turn, Gate: name})
		return nil
	}
	if e.gate != nil {
		// Steps run on one goroutine, so two open gates means a bug here.
		log.Printf("BUG: opening gate %q while gate %q is already open", name, e.gate.name)
	}
	g := &gate{turn: turn, name: name, ch: make(chan struct{})}
	e.gate = g
	e.mu.Unlock()
	return g.ch
}

// gateMatches: an empty advance releases any gate, an unnamed gate is
// released by any advance, otherwise names must agree.
//
// The unnamed-advance tolerance is deliberate and stays SCOPED TO ONE
// TURN by the caller: "release whichever gate opens next" is a useful
// thing for a driving test to say about the turn it is watching, and a
// dangerous thing to let carry into the next one.
func gateMatches(gateName, advanceName string) bool {
	return advanceName == "" || gateName == "" || advanceName == gateName
}

// terminate reports and exits. The exiting report is synchronous so it
// lands before the process dies.
func (e *engine) terminate(code int) {
	e.rep.report(control.Report{Kind: control.ReportExiting, Detail: strconv.Itoa(code)})
	e.exitFn(code)
}
