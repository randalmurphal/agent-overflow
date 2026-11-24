package claudetui

import (
	"encoding/json"
	"testing"
)

// decision_log_test.go covers the debug-only "decision" stream wiring
// (debuglog.go decisionLog + reconstructor.debug). The hook is nil in
// production; these tests prove that when the Session wires it (event logger
// live), it fires at the right branch with the right fields — so a reproduction
// run can be traced request-by-request. They go red if a branch stops recording
// or records the wrong route/action.

// recordDecisions wires a recording debug hook onto rec and returns the captured
// slice pointer.
func recordDecisions(rec *reconstructor) *[]decisionLog {
	got := &[]decisionLog{}
	rec.debug = func(d decisionLog) { *got = append(*got, d) }
	return got
}

func decisionsOf(ds []decisionLog, event string) []decisionLog {
	var out []decisionLog
	for _, d := range ds {
		if d.Event == event {
			out = append(out, d)
		}
	}
	return out
}

// TestDecisionLogSubagentRouting pins the three route decisions resolveSubagentParent
// makes: a content match on the first request, a cache hit on the next, and the
// unmatched miss — the last being the silent forward-without-reconstruction the
// "in" envelope feed shows nothing for, so the decision line is the only trace.
func TestDecisionLogSubagentRouting(t *testing.T) {
	rec := newReconstructor(func(json.RawMessage) {})
	got := recordDecisions(rec)
	rec.launches = []agentLaunch{{toolUseID: "toolu_AGENT", prompt: "do the thing"}}

	if p := rec.resolveSubagentParent("aid-1", "ctx do the thing ctx"); p != "toolu_AGENT" {
		t.Fatalf("content-match parent = %q, want toolu_AGENT", p)
	}
	if p := rec.resolveSubagentParent("aid-1", "a later message"); p != "toolu_AGENT" {
		t.Fatalf("cache parent = %q, want toolu_AGENT", p)
	}
	if p := rec.resolveSubagentParent("aid-2", "nothing matches here"); p != "" {
		t.Fatalf("miss parent = %q, want empty", p)
	}

	routes := decisionsOf(*got, "route")
	if len(routes) != 3 {
		t.Fatalf("route decisions = %d, want 3: %+v", len(routes), routes)
	}
	if routes[0].Route != "subagent" || routes[0].Via != "content-match" ||
		routes[0].Parent != "toolu_AGENT" || routes[0].AgentID != "aid-1" {
		t.Errorf("content-match route = %+v, want subagent/content-match/toolu_AGENT/aid-1", routes[0])
	}
	if routes[1].Route != "subagent" || routes[1].Via != "cache" || routes[1].Parent != "toolu_AGENT" {
		t.Errorf("cache route = %+v, want subagent/cache/toolu_AGENT", routes[1])
	}
	if routes[2].Route != "subagent-unmatched" || routes[2].Via != "none" || routes[2].AgentID != "aid-2" {
		t.Errorf("unmatched route = %+v, want subagent-unmatched/none/aid-2", routes[2])
	}
}
