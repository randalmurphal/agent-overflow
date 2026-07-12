package def

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestEvaluateGatePredicatesAndOrderedRouting(t *testing.T) {
	tests := []struct {
		name      string
		predicate Predicate
		vars      map[string]any
		want      bool
	}{
		{"eq", Predicate{Eq: &Comparison{Ref: "value", Value: 2}}, map[string]any{"value": json.Number("2.0")}, true},
		{"neq", Predicate{Neq: &Comparison{Ref: "value", Value: "no"}}, map[string]any{"value": "yes"}, true},
		{"gt", Predicate{Gt: &Comparison{Ref: "value", Value: 2}}, map[string]any{"value": 3}, true},
		{"gte", Predicate{Gte: &Comparison{Ref: "value", Value: 3}}, map[string]any{"value": 3}, true},
		{"lt", Predicate{Lt: &Comparison{Ref: "value", Value: 4}}, map[string]any{"value": 3}, true},
		{"lte", Predicate{Lte: &Comparison{Ref: "value", Value: 3}}, map[string]any{"value": 3}, true},
		{"in", Predicate{In: &Membership{Ref: "value", Values: []any{"a", "b"}}}, map[string]any{"value": "b"}, true},
		{"exists", Predicate{Exists: "value"}, map[string]any{"value": false}, true},
		{"exists null is absent", Predicate{Exists: "value"}, map[string]any{"value": nil}, false},
		{"nested reference", Predicate{Eq: &Comparison{Ref: "phase.result.ok", Value: true}}, map[string]any{"phase.result": map[string]any{"ok": true}}, true},
		{"structured numeric equality", Predicate{Eq: &Comparison{Ref: "value", Value: map[string]any{"counts": []any{1, 2}}}}, map[string]any{"value": map[string]any{"counts": []any{json.Number("1.0"), json.Number("2")}}}, true},
		{"large integers stay distinct", Predicate{Eq: &Comparison{Ref: "value", Value: uint64(9007199254740993)}}, map[string]any{"value": json.Number("9007199254740992")}, false},
		{"absent comparison is false", Predicate{Neq: &Comparison{Ref: "missing", Value: "x"}}, nil, false},
		{"all", Predicate{All: []Predicate{{Exists: "a"}, {Exists: "b"}}}, map[string]any{"a": 1, "b": 2}, true},
		{"any", Predicate{Any: []Predicate{{Exists: "a"}, {Exists: "b"}}}, map[string]any{"b": 2}, true},
		{"not", Predicate{Not: &Predicate{Eq: &Comparison{Ref: "value", Value: false}}}, map[string]any{"value": true}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			phase := Phase{ID: "gate", Gate: Gate{Routes: []Route{
				{When: &tc.predicate, To: "matched"},
				{To: "fallback"},
			}}}
			decision, trace, err := EvaluateGate(phase, tc.vars, nil)
			if err != nil {
				t.Fatal(err)
			}
			wantTarget := "fallback"
			if tc.want {
				wantTarget = "matched"
			}
			if decision.Kind != DecisionAdvance || decision.Target != wantTarget {
				t.Fatalf("decision = %+v, want target %q", decision, wantTarget)
			}
			if len(trace.Predicates) == 0 || trace.Predicates[len(trace.Predicates)-1].Result != tc.want {
				t.Fatalf("trace does not record root result %v: %+v", tc.want, trace)
			}
		})
	}
}

func TestEvaluateGateShortCircuitsAndTracesOnlyEvaluatedPredicates(t *testing.T) {
	predicate := Predicate{All: []Predicate{
		{Eq: &Comparison{Ref: "ready", Value: false}},
		{Gt: &Comparison{Ref: "bad", Value: 1}},
	}}
	phase := Phase{ID: "review", Gate: Gate{Routes: []Route{{When: &predicate, To: "done"}}}}
	decision, trace, err := EvaluateGate(phase, map[string]any{"ready": true, "bad": "not-number"}, nil)
	if err != nil {
		t.Fatalf("short-circuited child was evaluated: %v", err)
	}
	if decision.Kind != DecisionNoMatch {
		t.Fatalf("decision = %+v", decision)
	}
	if len(trace.Predicates) != 2 || trace.Predicates[0].Path != "routes[0].when.all[0]" || trace.Predicates[1].Path != "routes[0].when" {
		t.Fatalf("trace = %+v, want first child and all root only", trace.Predicates)
	}
}

func TestEvaluateGateTraceContentAndOrder(t *testing.T) {
	first := Predicate{Exists: "missing"}
	second := Predicate{All: []Predicate{
		{Eq: &Comparison{Ref: "count", Value: 2}},
		{In: &Membership{Ref: "status", Values: []any{"ready", "done"}}},
	}}
	phase := Phase{ID: "review", Gate: Gate{Routes: []Route{
		{When: &first, To: "failed"},
		{When: &second, To: "done"},
	}}}
	decision, trace, err := EvaluateGate(phase, map[string]any{"count": json.Number("2"), "status": "ready"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Kind != DecisionDone || decision.RouteIndex != 1 {
		t.Fatalf("decision = %+v", decision)
	}
	want := []PredicateTrace{
		{RouteIndex: 0, Path: "routes[0].when", Operator: "exists", Ref: "missing", Result: false},
		{RouteIndex: 1, Path: "routes[1].when.all[0]", Operator: "eq", Ref: "count", Value: 2, Result: true},
		{RouteIndex: 1, Path: "routes[1].when.all[1]", Operator: "in", Ref: "status", Values: []any{"ready", "done"}, Result: true},
		{RouteIndex: 1, Path: "routes[1].when", Operator: "all", Result: true},
	}
	if !reflect.DeepEqual(trace.Predicates, want) {
		t.Fatalf("trace predicates =\n%+v\nwant\n%+v", trace.Predicates, want)
	}
}

func TestEvaluateGateDecisionKinds(t *testing.T) {
	tests := []struct {
		name  string
		route Route
		kind  DecisionKind
	}{
		{"advance", Route{To: "next"}, DecisionAdvance},
		{"done", Route{To: "done"}, DecisionDone},
		{"failed", Route{To: "failed"}, DecisionFailed},
		{"park", Route{Park: "review"}, DecisionPark},
		{"human", Route{Human: &HumanRoute{Approve: "done"}}, DecisionHuman},
		{"loop", Route{Loop: "build", Max: 2, Feedback: []string{"check.reason"}}, DecisionLoop},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			phase := Phase{ID: "gate", Gate: Gate{Routes: []Route{tc.route}}}
			decision, trace, err := EvaluateGate(phase, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Kind != tc.kind || !reflect.DeepEqual(trace.Decision, decision) {
				t.Fatalf("decision/trace = %+v / %+v, want %q", decision, trace.Decision, tc.kind)
			}
		})
	}
}

func TestEvaluateGateLoopExhaustionFallsThrough(t *testing.T) {
	phase := Phase{ID: "review", Gate: Gate{Routes: []Route{
		{Loop: "build", Max: 2},
		{To: "done"},
	}}}
	edge := GateEdgeKey("review", 0)
	decision, trace, err := EvaluateGate(phase, nil, map[string]int{edge: 2})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Kind != DecisionDone || !reflect.DeepEqual(trace.ExhaustedLoops, []string{edge}) {
		t.Fatalf("fall-through = %+v, trace = %+v", decision, trace)
	}

	phase.Gate.Routes = phase.Gate.Routes[:1]
	decision, trace, err = EvaluateGate(phase, nil, map[string]int{edge: 2})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Kind != DecisionRetriesExhausted || trace.Decision.Kind != DecisionRetriesExhausted {
		t.Fatalf("exhausted decision = %+v, trace = %+v", decision, trace)
	}
}

func TestEvaluateGateDistinguishesNoMatch(t *testing.T) {
	predicate := Predicate{Eq: &Comparison{Ref: "ready", Value: true}}
	phase := Phase{ID: "review", Gate: Gate{Routes: []Route{{When: &predicate, To: "done"}}}}
	decision, trace, err := EvaluateGate(phase, map[string]any{"ready": false}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Kind != DecisionNoMatch || len(trace.ExhaustedLoops) != 0 {
		t.Fatalf("no-match decision = %+v, trace = %+v", decision, trace)
	}
}

func TestEvaluateGateRejectsRuntimeTypeMismatch(t *testing.T) {
	predicate := Predicate{Gt: &Comparison{Ref: "count", Value: 1}}
	phase := Phase{ID: "review", Gate: Gate{Routes: []Route{{When: &predicate, To: "done"}}}}
	if _, _, err := EvaluateGate(phase, map[string]any{"count": "many"}, nil); err == nil {
		t.Fatal("expected a visible runtime type error")
	}
}

func TestEvaluateGateRejectsMalformedPredicate(t *testing.T) {
	predicate := Predicate{Eq: &Comparison{Ref: "count", Value: 1}, Exists: "count"}
	phase := Phase{ID: "review", Gate: Gate{Routes: []Route{{When: &predicate, To: "done"}}}}
	if _, _, err := EvaluateGate(phase, map[string]any{"count": 1}, nil); err == nil {
		t.Fatal("expected malformed predicate error")
	}
}

func TestEvaluateGateCombinatorsUseBooleanResultOfAbsentComparison(t *testing.T) {
	tests := []Predicate{
		{Not: &Predicate{Eq: &Comparison{Ref: "missing", Value: "x"}}},
		{Not: &Predicate{All: []Predicate{
			{Eq: &Comparison{Ref: "present", Value: true}},
			{Eq: &Comparison{Ref: "missing", Value: "x"}},
		}}},
	}
	for index, predicate := range tests {
		phase := Phase{ID: "review", Gate: Gate{Routes: []Route{{When: &predicate, To: "done"}}}}
		decision, trace, err := EvaluateGate(phase, map[string]any{"present": true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Kind != DecisionDone {
			t.Fatalf("case %d decision = %+v, want done", index, decision)
		}
		if !trace.Predicates[len(trace.Predicates)-1].Absent {
			t.Fatalf("case %d trace did not preserve absence: %+v", index, trace.Predicates)
		}
	}
}

func TestEvaluateGateAnyCanMatchPresentAlternativeAfterAbsence(t *testing.T) {
	predicate := Predicate{Any: []Predicate{
		{Eq: &Comparison{Ref: "missing", Value: true}},
		{Eq: &Comparison{Ref: "present", Value: true}},
	}}
	phase := Phase{ID: "review", Gate: Gate{Routes: []Route{{When: &predicate, To: "done"}}}}
	decision, _, err := EvaluateGate(phase, map[string]any{"present": true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Kind != DecisionDone {
		t.Fatalf("decision = %+v, want done", decision)
	}
}
