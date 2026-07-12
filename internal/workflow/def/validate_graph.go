package def

import "fmt"

type workflowGraph struct {
	forward    [][]int
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
			if route.Loop == "" && (route.Max != 0 || len(route.Feedback) != 0) {
				*findings = append(*findings, finding("gate.route-fields", element, "max and feedback are valid only on loop routes"))
			}
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
				if route.Max < 1 {
					*findings = append(*findings, finding("gate.loop-max", element, fmt.Sprintf("loop to %q requires max >= 1", route.Loop)))
				}
				if _, ok := phaseIndex[route.Loop]; !ok {
					*findings = append(*findings, finding("gate.target", element, fmt.Sprintf("loop target %q does not exist", route.Loop)))
				}
			}
			if route.Human != nil && route.Human.Reject != nil {
				reject := route.Human.Reject
				if reject.Max < 1 {
					*findings = append(*findings, finding("gate.loop-max", element, fmt.Sprintf("human reject loop to %q requires max >= 1", reject.Loop)))
				}
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
	return workflowGraph{forward: forward, dominators: dominators}
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
