package def

import "fmt"

type workflowGraph struct {
	forward    [][]int
	reachable  []bool
	dominators []map[int]bool
}

func buildGraph(workflow Workflow, phaseIndex map[string]int, findings *[]Finding) workflowGraph {
	forward := make([][]int, len(workflow.Phases))
	addTarget := func(from int, routeIndex int, target string, kind string) {
		if target == "done" || target == "failed" || target == "" {
			return
		}
		to, ok := phaseIndex[target]
		if !ok {
			element := fmt.Sprintf("workflow %q phase %q route %d", workflow.ID, workflow.Phases[from].ID, routeIndex)
			*findings = append(*findings, finding("gate.target", element, fmt.Sprintf("%s target %q does not exist", kind, target)))
			return
		}
		forward[from] = append(forward[from], to)
	}
	for i, phase := range workflow.Phases {
		terminalRouteSeen := false
		for routeIndex, route := range phase.Gate.Routes {
			element := fmt.Sprintf("workflow %q phase %q route %d", workflow.ID, phase.ID, routeIndex)
			if terminalRouteSeen {
				*findings = append(*findings, finding("gate.route-order", element, "route is unreachable after an unconditional non-loop route"))
			}
			kinds := 0
			if route.To != "" {
				kinds++
			}
			if route.Loop != "" {
				kinds++
			}
			if route.Park != "" {
				kinds++
			}
			if route.Human != nil {
				kinds++
			}
			if kinds != 1 {
				*findings = append(*findings, finding("gate.route-kind", element, "route must declare exactly one of to, loop, park, or human"))
			}
			if route.Loop == "" && (!route.Max.IsZero() || len(route.Feedback) != 0) {
				*findings = append(*findings, finding("gate.route-fields", element, "max and feedback are valid only on loop routes"))
			}
			if route.Notify && (route.Human != nil || route.Park != "") {
				*findings = append(*findings, finding("gate.notify", element,
					"notify is not valid on a human or park route: parking already wakes the bound thread, and this would promise a second wake for the one event"))
			}
			*findings = append(*findings, loopRouteKnobFindings(workflow, phaseIndex, route, element)...)
			addTarget(i, routeIndex, route.To, "forward")
			if route.Human != nil {
				if route.Human.Approve == "" {
					*findings = append(*findings, finding("gate.human", element, "human route requires a non-empty approve target"))
				}
				addTarget(i, routeIndex, route.Human.Approve, "human approve")
				if route.Human.Reject == nil {
					*findings = append(*findings, finding("gate.human", element, "human route requires reject loop"))
				}
			}
			if route.Loop != "" {
				*findings = append(*findings, loopBoundShapeFindings(route.Max, element, fmt.Sprintf("loop to %q", route.Loop))...)
				if _, ok := phaseIndex[route.Loop]; !ok {
					*findings = append(*findings, finding("gate.target", element, fmt.Sprintf("loop target %q does not exist", route.Loop)))
				}
			}
			if route.Human != nil && route.Human.Reject != nil {
				reject := route.Human.Reject
				*findings = append(*findings, loopBoundShapeFindings(reject.Max, element, fmt.Sprintf("human reject loop to %q", reject.Loop))...)
				if _, ok := phaseIndex[reject.Loop]; !ok {
					*findings = append(*findings, finding("gate.target", element, fmt.Sprintf("human reject loop target %q does not exist", reject.Loop)))
				}
			}
			if route.When == nil && route.Loop == "" {
				terminalRouteSeen = true
			}
		}
	}
	reachable := make([]bool, len(workflow.Phases))
	if len(workflow.Phases) > 0 {
		walk(0, forward, reachable)
	}
	for i, phase := range workflow.Phases {
		if !reachable[i] {
			*findings = append(*findings, finding("graph.unreachable", fmt.Sprintf("workflow %q phase %q", workflow.ID, phase.ID), "phase is unreachable from the first phase"))
		}
	}
	dominators := computeDominators(forward, reachable)
	for i, phase := range workflow.Phases {
		for routeIndex, route := range phase.Gate.Routes {
			if route.To != "" {
				validateUnboundedCycle(workflow, phaseIndex, forward, i, routeIndex, route.To, findings)
			}
			if route.Human != nil {
				validateUnboundedCycle(workflow, phaseIndex, forward, i, routeIndex, route.Human.Approve, findings)
			}
			if route.Loop != "" {
				validateLoopAncestor(workflow, phaseIndex, dominators, i, routeIndex, route.Loop, findings)
			}
			if route.Human != nil && route.Human.Reject != nil {
				validateLoopAncestor(workflow, phaseIndex, dominators, i, routeIndex, route.Human.Reject.Loop, findings)
			}
		}
	}
	return workflowGraph{forward: forward, reachable: reachable, dominators: dominators}
}

// loopRouteKnobFindings checks the two per-round knobs a loop route may carry:
// `session:` (continue the target phase's own session instead of starting a
// cold one) and `prompt:` (render a different body for the one attempt this
// route creates).
//
// Both are refused outside a `loop:` route, and for the same reason: a forward,
// park, or human route enters a phase from OUTSIDE its cycle, where there is no
// previous round of that phase to continue and no per-round question to narrow.
// Refused rather than ignored, because a knob that silently does nothing is one
// the author only discovers by watching a run behave as though it were absent.
//
// Both additionally require a target that runs ONE session of its own: a
// `driver: tool` phase runs a command, a `shape: call` phase delegates to a
// child workflow, and a `shape: fan-out` phase runs no turn at all — its units
// and its join each hold their own session and their own prompt. Neither knob
// has anything to name in those cases.
func loopRouteKnobFindings(workflow Workflow, phaseIndex map[string]int, route Route, element string) []Finding {
	var findings []Finding
	if route.Loop == "" {
		if route.Session != "" {
			findings = append(findings, finding("gate.session", element,
				"session is valid only on a loop route: every other route enters a phase from outside its cycle, where there is no previous round of that phase to continue"))
		}
		if route.Prompt != "" {
			findings = append(findings, finding("gate.prompt", element,
				"prompt is valid only on a loop route: it overrides the body of the phase a round re-enters, and every other route enters a phase for the first time this cycle"))
		}
		return findings
	}
	if route.Session != "" && route.Session != SessionFresh && route.Session != SessionContinue {
		findings = append(findings, finding("gate.session", element,
			fmt.Sprintf("session must be %q or %q", SessionFresh, SessionContinue)))
	}
	if route.Session != SessionContinue && route.Prompt == "" {
		return findings
	}
	index, ok := phaseIndex[route.Loop]
	if !ok {
		// The target does not exist; `gate.target` already says so, and blaming a
		// knob for it would report one mistake twice.
		return findings
	}
	target := workflow.Phases[index]
	if phaseRunsOwnSession(target) {
		return findings
	}
	if route.Session == SessionContinue {
		findings = append(findings, finding("gate.session", element,
			fmt.Sprintf("session: continue needs a loop target that runs one session of its own; phase %q %s", target.ID, describeSessionlessPhase(target))))
	}
	if route.Prompt != "" {
		findings = append(findings, finding("gate.prompt", element,
			fmt.Sprintf("prompt overrides the body of the phase this loop re-enters, and phase %q %s", target.ID, describeSessionlessPhase(target))))
	}
	return findings
}

// phaseRunsOwnSession reports whether one phase attempt is one agent turn on one
// provider session — the shape both loop-route knobs address.
func phaseRunsOwnSession(phase Phase) bool {
	return !phase.IsCall() && phase.EffectiveShape() == ShapeSingle && phase.Driver == DriverAgent
}

// describeSessionlessPhase completes the sentence "phase %q …" for a target
// neither knob can address, naming which of the three shapes it is so the author
// is not left to work out why.
func describeSessionlessPhase(phase Phase) string {
	switch {
	case phase.IsCall():
		return "delegates to a child workflow and runs no prompt or session of its own"
	case phase.EffectiveShape() == ShapeFanOut:
		return "is a fan-out: its units and its join each carry their own prompt and their own session"
	default:
		return "runs a command rather than a model turn"
	}
}

// gateNotifyReports is the informational half of the `notify:` decoration: a
// route to `done` or `failed` ends the run, and a resting run already wakes its
// bound thread with the fuller message, so the decoration adds nothing there.
//
// It is a Report rather than a Finding because the definition is not wrong —
// the same `to:` route shape carries the decoration legitimately one line
// above — and it is not silence because a declaration nothing acts on is the
// kind of dead line an author only discovers by watching for a wake that never
// arrives.
func gateNotifyReports(workflow Workflow) []Finding {
	var reports []Finding
	for _, phase := range workflow.Phases {
		for routeIndex, route := range phase.Gate.Routes {
			if !route.Notify || (route.To != "done" && route.To != "failed") {
				continue
			}
			reports = append(reports, finding("gate.notify-terminal",
				fmt.Sprintf("workflow %q phase %q route %d", workflow.ID, phase.ID, routeIndex),
				fmt.Sprintf("notify does nothing on a route to %q: the run rests there and its resting wake already reports it", route.To)))
		}
	}
	return reports
}

func validateUnboundedCycle(workflow Workflow, phaseIndex map[string]int, edges [][]int, from, routeIndex int, target string, findings *[]Finding) {
	to, ok := phaseIndex[target]
	if !ok {
		return
	}
	seen := make([]bool, len(edges))
	walk(to, edges, seen)
	if seen[from] {
		element := fmt.Sprintf("workflow %q phase %q route %d", workflow.ID, workflow.Phases[from].ID, routeIndex)
		*findings = append(*findings, finding("gate.unbounded-cycle", element, fmt.Sprintf("forward target %q creates a cycle; use a bounded loop route", target)))
	}
}

func validateLoopAncestor(workflow Workflow, phaseIndex map[string]int, dominators []map[int]bool, from, routeIndex int, target string, findings *[]Finding) {
	to, ok := phaseIndex[target]
	if !ok {
		return
	}
	element := fmt.Sprintf("workflow %q phase %q route %d", workflow.ID, workflow.Phases[from].ID, routeIndex)
	if to == from || from >= len(dominators) || !dominators[from][to] {
		*findings = append(*findings, finding("gate.loop-ancestor", element, fmt.Sprintf("loop target %q is not a strict ancestor", target)))
	}
}

func walk(node int, edges [][]int, seen []bool) {
	if seen[node] {
		return
	}
	seen[node] = true
	for _, next := range edges[node] {
		walk(next, edges, seen)
	}
}

func computeDominators(edges [][]int, reachable []bool) []map[int]bool {
	n := len(edges)
	preds := make([][]int, n)
	for from, targets := range edges {
		for _, to := range targets {
			preds[to] = append(preds[to], from)
		}
	}
	dom := make([]map[int]bool, n)
	for node := 0; node < n; node++ {
		dom[node] = map[int]bool{}
		if node == 0 {
			dom[node][0] = true
			continue
		}
		if reachable[node] {
			for candidate := 0; candidate < n; candidate++ {
				if reachable[candidate] {
					dom[node][candidate] = true
				}
			}
		}
	}
	changed := true
	for changed {
		changed = false
		for node := 1; node < n; node++ {
			if !reachable[node] {
				continue
			}
			next := map[int]bool{node: true}
			reachablePreds := make([]int, 0, len(preds[node]))
			for _, pred := range preds[node] {
				if reachable[pred] {
					reachablePreds = append(reachablePreds, pred)
				}
			}
			if len(reachablePreds) > 0 {
				for candidate := range dom[reachablePreds[0]] {
					inAll := true
					for _, pred := range reachablePreds[1:] {
						if !dom[pred][candidate] {
							inAll = false
							break
						}
					}
					if inAll {
						next[candidate] = true
					}
				}
			}
			if !sameSet(dom[node], next) {
				dom[node] = next
				changed = true
			}
		}
	}
	return dom
}

func sameSet(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if !b[key] {
			return false
		}
	}
	return true
}
