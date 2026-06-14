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

// TestDecisionLogBackgroundCompletion pins the bg_completion decisions a
// backgrounded-task resume produces: a terminal <task-notification> emits once,
// and the same notification recurring in conversation history records "deduped"
// (not a second completion). Also asserts the resume is a main route that
// (re)opened a settled loop (Init true).
func TestDecisionLogBackgroundCompletion(t *testing.T) {
	h := newParserHarness(t)
	got := recordDecisions(h.rec)

	notif := taskNotificationXML("a2ed59bfa0ba9b279", "toolu_AGENT", "completed", "/tmp/out.txt", "done")
	h.runMain(t, bgResumeReqBody(notif), endTurnSSE())
	h.runMain(t, bgResumeReqBody(notif), endTurnSSE()) // recurs in history → deduped

	bg := decisionsOf(*got, "bg_completion")
	if len(bg) != 2 {
		t.Fatalf("bg_completion decisions = %d, want 2 (emitted, deduped): %+v", len(bg), bg)
	}
	if bg[0].Action != "emitted" || bg[0].TaskID != "a2ed59bfa0ba9b279" ||
		bg[0].ToolUseID != "toolu_AGENT" || bg[0].Status != "completed" {
		t.Errorf("first bg = %+v, want emitted/a2ed.../toolu_AGENT/completed", bg[0])
	}
	if bg[1].Action != "deduped" || bg[1].TaskID != "a2ed59bfa0ba9b279" {
		t.Errorf("second bg = %+v, want deduped/a2ed...", bg[1])
	}

	if routes := decisionsOf(*got, "route"); len(routes) == 0 || routes[0].Route != "main" || !routes[0].Init {
		t.Fatalf("first route = %+v, want main with init=true", routes)
	}
	if tc := decisionsOf(*got, "turn_close"); len(tc) != 2 || tc[0].Route != "main" || tc[0].Stop != "end_turn" {
		t.Fatalf("turn_close decisions = %+v, want 2 main/end_turn", tc)
	}
}
