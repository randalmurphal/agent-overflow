package def

import (
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"
)

// DecisionKind identifies the result of evaluating one phase gate.
type DecisionKind string

const (
	DecisionAdvance          DecisionKind = "advance"
	DecisionLoop             DecisionKind = "loop"
	DecisionPark             DecisionKind = "park"
	DecisionHuman            DecisionKind = "human"
	DecisionDone             DecisionKind = "done"
	DecisionFailed           DecisionKind = "failed"
	DecisionRetriesExhausted DecisionKind = "retries-exhausted"
	DecisionNoMatch          DecisionKind = "no-match"
)

// RouteDecision is the first route selected by EvaluateGate. RouteIndex is -1
// for exhausted/no-match results. LoopEdge is stable across process restarts
// and is the key callers use when reconstructing persisted loop counts.
//
// Max is the RESOLVED bound this traversal was measured against, never the
// authored form: a seeded `max: <ref>` is a number only once the run's
// variables exist, and the decision is persisted in the gate trace, so the
// trace states the budget the run actually got rather than the name it came
// from.
type RouteDecision struct {
	Kind       DecisionKind `json:"kind"`
	RouteIndex int          `json:"routeIndex"`
	Target     string       `json:"target,omitempty"`
	Feedback   []string     `json:"feedback,omitempty"`
	LoopEdge   string       `json:"loopEdge,omitempty"`
	Max        int          `json:"max,omitempty"`
	// Notify is the route's `notify:` decoration, carried onto the decision so
	// the persisted gate trace states that this traversal asked for a progress
	// wake. It is set only for the decisions the run CONTINUES through, which
	// is what makes the flag readable as "a progress wake was dispatched"
	// rather than as an authored intention nothing acted on.
	Notify bool `json:"notify,omitempty"`
	// Session is the route's `session:` mode, carried onto the decision so the
	// persisted gate trace states which one the gate ASKED for. Like Notify it
	// is set only where the mode can mean anything — a loop decision — so a
	// trace can never claim a session mode for a route that entered a different
	// phase. What the attempt actually ran under is a separate fact: a
	// continuation the engine could not honour falls back to a fresh session
	// with a note on the attempt, and the two attempts sharing one thread id is
	// the durable evidence that it did continue.
	Session RouteSession `json:"session,omitempty"`
}

// PredicateTrace records one predicate node that was actually evaluated.
// Nodes skipped by short-circuit evaluation are deliberately absent.
type PredicateTrace struct {
	RouteIndex int    `json:"routeIndex"`
	Path       string `json:"path"`
	Operator   string `json:"operator"`
	Ref        string `json:"ref,omitempty"`
	Value      any    `json:"value,omitempty"`
	Values     []any  `json:"values,omitempty"`
	Result     bool   `json:"result"`
	Absent     bool   `json:"absent,omitempty"`
}

// GateTrace is persisted verbatim with a completed phase attempt.
type GateTrace struct {
	Predicates     []PredicateTrace `json:"predicates"`
	ExhaustedLoops []string         `json:"exhaustedLoops,omitempty"`
	Decision       RouteDecision    `json:"decision"`
}

// GateEdgeKey returns the stable per-phase-route loop counter key.
func GateEdgeKey(phaseID string, routeIndex int) string {
	return fmt.Sprintf("%s:%d", phaseID, routeIndex)
}

// EvaluateGate evaluates phase routes in order with first-match and
// short-circuit semantics. loopCounts holds each GateEdgeKey's spend against
// its bound: the traversals its caller attributes to the current entry of the
// loop's target phase, not to the run's whole lifetime (spec §4).
func EvaluateGate(phase Phase, vars map[string]any, loopCounts map[string]int) (RouteDecision, GateTrace, error) {
	trace := GateTrace{Predicates: make([]PredicateTrace, 0)}
	exhaustedLoop := false
	for routeIndex, route := range phase.Gate.Routes {
		matched := true
		if route.When != nil {
			var err error
			matched, _, err = evaluatePredicate(*route.When, vars, routeIndex, fmt.Sprintf("routes[%d].when", routeIndex), &trace)
			if err != nil {
				return RouteDecision{}, trace, fmt.Errorf("evaluate phase %q route %d: %w", phase.ID, routeIndex, err)
			}
		}
		if !matched {
			continue
		}

		decision := decisionForRoute(route, routeIndex)
		if decision.Kind == DecisionLoop {
			decision.LoopEdge = GateEdgeKey(phase.ID, routeIndex)
			// A bound that cannot be resolved is not an exhausted loop: the
			// definition and this run's variables cannot say how many
			// traversals are allowed at all, so the caller parks the attempt
			// rather than routing it somewhere the author never chose.
			max, err := route.Max.Resolve(vars)
			if err != nil {
				return RouteDecision{}, trace, fmt.Errorf("evaluate phase %q route %d: %w", phase.ID, routeIndex, err)
			}
			decision.Max = max
			if loopCounts[decision.LoopEdge] >= max {
				exhaustedLoop = true
				trace.ExhaustedLoops = append(trace.ExhaustedLoops, decision.LoopEdge)
				continue
			}
		}
		trace.Decision = decision
		return decision, trace, nil
	}

	kind := DecisionNoMatch
	if exhaustedLoop {
		kind = DecisionRetriesExhausted
	}
	decision := RouteDecision{Kind: kind, RouteIndex: -1}
	trace.Decision = decision
	return decision, trace, nil
}

// decisionForRoute maps one matched route onto its decision. A loop route's
// LoopEdge and resolved Max are stamped by EvaluateGate, which is the only
// place a variable context exists to resolve a seeded bound against.
//
// `notify:` survives onto exactly the two kinds where the run continues past
// the gate. Validation already refuses the decoration on a human/park route and
// reports it as inert on a route to done/failed, but a frozen snapshot is
// decoded and never re-validated, so the rule is enforced here as well: this is
// the one place that decides whether a progress wake is owed, and a decision
// that claimed one where the run rested would be a trace nobody could trust.
func decisionForRoute(route Route, routeIndex int) RouteDecision {
	decision := RouteDecision{RouteIndex: routeIndex}
	switch {
	case route.Loop != "":
		decision.Kind = DecisionLoop
		decision.Target = route.Loop
		decision.Feedback = append([]string(nil), route.Feedback...)
		// Resolved here rather than read off the route later, for the reason
		// `notify:` is: a frozen snapshot is decoded and never re-validated, so a
		// run started before the field was refused on other route kinds still
		// reaches the evaluator with one declared there. Setting it on the loop
		// decision alone is what makes the trace readable as "this re-entry asked
		// to continue the phase's session". Only the non-default mode is
		// recorded: absent means `fresh`, which is what every trace written
		// before the field existed already means.
		if route.EffectiveSession() == SessionContinue {
			decision.Session = SessionContinue
		}
	case route.Human != nil:
		decision.Kind = DecisionHuman
	case route.Park != "":
		decision.Kind = DecisionPark
		decision.Target = route.Park
	case route.To == "done":
		decision.Kind = DecisionDone
	case route.To == "failed":
		decision.Kind = DecisionFailed
	default:
		decision.Kind = DecisionAdvance
		decision.Target = route.To
	}
	decision.Notify = route.Notify && ContinuesPastGate(decision.Kind)
	return decision
}

// ContinuesPastGate reports whether a decision leaves the run running. It is
// what tells a progress wake (the run passed here) from a resting one (the run
// stopped here), and both the evaluator and the engine ask it rather than each
// keeping a list of kinds.
func ContinuesPastGate(kind DecisionKind) bool {
	return kind == DecisionAdvance || kind == DecisionLoop
}

func evaluatePredicate(predicate Predicate, vars map[string]any, routeIndex int, path string, trace *GateTrace) (bool, bool, error) {
	if issues, _ := predicateShapeIssues(predicate); len(issues) > 0 {
		return false, false, fmt.Errorf("%s", issues[0].message)
	}
	var (
		result bool
		absent bool
		err    error
	)
	switch {
	case predicate.Eq != nil:
		result, absent, err = evaluateComparison(vars, *predicate.Eq, compareEqual)
	case predicate.Neq != nil:
		result, absent, err = evaluateComparison(vars, *predicate.Neq, func(left, right any) (bool, error) {
			equal, err := compareEqual(left, right)
			return !equal, err
		})
	case predicate.Gt != nil:
		result, absent, err = evaluateOrdered(vars, *predicate.Gt, func(cmp int) bool { return cmp > 0 })
	case predicate.Gte != nil:
		result, absent, err = evaluateOrdered(vars, *predicate.Gte, func(cmp int) bool { return cmp >= 0 })
	case predicate.Lt != nil:
		result, absent, err = evaluateOrdered(vars, *predicate.Lt, func(cmp int) bool { return cmp < 0 })
	case predicate.Lte != nil:
		result, absent, err = evaluateOrdered(vars, *predicate.Lte, func(cmp int) bool { return cmp <= 0 })
	case predicate.In != nil:
		value, present := LookupVariable(vars, predicate.In.Ref)
		absent = !present
		if !absent {
			for _, candidate := range predicate.In.Values {
				var equal bool
				equal, err = compareEqual(value, candidate)
				if err != nil || equal {
					result = equal
					break
				}
			}
		}
	case predicate.Exists != "":
		_, result = LookupVariable(vars, predicate.Exists)
	case predicate.All != nil:
		result = true
		for i, child := range predicate.All {
			var childResult, childAbsent bool
			childResult, childAbsent, err = evaluatePredicate(child, vars, routeIndex, fmt.Sprintf("%s.all[%d]", path, i), trace)
			if err != nil || !childResult {
				result = false
				absent = childAbsent
				break
			}
		}
	case predicate.Any != nil:
		sawAbsent := false
		for i, child := range predicate.Any {
			var childResult, childAbsent bool
			childResult, childAbsent, err = evaluatePredicate(child, vars, routeIndex, fmt.Sprintf("%s.any[%d]", path, i), trace)
			sawAbsent = sawAbsent || childAbsent
			if err != nil || childResult {
				result = childResult
				absent = false
				break
			}
			absent = sawAbsent
		}
	case predicate.Not != nil:
		var childResult bool
		childResult, absent, err = evaluatePredicate(*predicate.Not, vars, routeIndex, path+".not", trace)
		result = !childResult
	default:
		err = fmt.Errorf("predicate must declare exactly one supported operator")
	}
	if err != nil {
		return false, false, err
	}
	entry := predicateTraceEntry(predicate)
	entry.RouteIndex = routeIndex
	entry.Path = path
	entry.Result = result
	entry.Absent = absent
	trace.Predicates = append(trace.Predicates, entry)
	return result, absent, nil
}

func predicateTraceEntry(predicate Predicate) PredicateTrace {
	switch {
	case predicate.Eq != nil:
		return PredicateTrace{Operator: "eq", Ref: predicate.Eq.Ref, Value: predicate.Eq.Value}
	case predicate.Neq != nil:
		return PredicateTrace{Operator: "neq", Ref: predicate.Neq.Ref, Value: predicate.Neq.Value}
	case predicate.Gt != nil:
		return PredicateTrace{Operator: "gt", Ref: predicate.Gt.Ref, Value: predicate.Gt.Value}
	case predicate.Gte != nil:
		return PredicateTrace{Operator: "gte", Ref: predicate.Gte.Ref, Value: predicate.Gte.Value}
	case predicate.Lt != nil:
		return PredicateTrace{Operator: "lt", Ref: predicate.Lt.Ref, Value: predicate.Lt.Value}
	case predicate.Lte != nil:
		return PredicateTrace{Operator: "lte", Ref: predicate.Lte.Ref, Value: predicate.Lte.Value}
	case predicate.In != nil:
		return PredicateTrace{Operator: "in", Ref: predicate.In.Ref, Values: append([]any(nil), predicate.In.Values...)}
	case predicate.Exists != "":
		return PredicateTrace{Operator: "exists", Ref: predicate.Exists}
	case predicate.All != nil:
		return PredicateTrace{Operator: "all"}
	case predicate.Any != nil:
		return PredicateTrace{Operator: "any"}
	case predicate.Not != nil:
		return PredicateTrace{Operator: "not"}
	default:
		return PredicateTrace{}
	}
}

func countPredicateOperators(predicate Predicate) int {
	count := 0
	for _, present := range []bool{
		predicate.Eq != nil, predicate.Neq != nil, predicate.Gt != nil,
		predicate.Gte != nil, predicate.Lt != nil, predicate.Lte != nil,
		predicate.In != nil, predicate.Exists != "", predicate.All != nil,
		predicate.Any != nil, predicate.Not != nil,
	} {
		if present {
			count++
		}
	}
	return count
}

func evaluateComparison(vars map[string]any, comparison Comparison, compare func(any, any) (bool, error)) (bool, bool, error) {
	value, present := LookupVariable(vars, comparison.Ref)
	if !present {
		return false, true, nil
	}
	result, err := compare(value, comparison.Value)
	return result, false, err
}

func evaluateOrdered(vars map[string]any, comparison Comparison, accept func(int) bool) (bool, bool, error) {
	value, present := LookupVariable(vars, comparison.Ref)
	if !present {
		return false, true, nil
	}
	left, ok := number(value)
	if !ok {
		return false, false, fmt.Errorf("reference %q has non-numeric value %T", comparison.Ref, value)
	}
	right, ok := number(comparison.Value)
	if !ok {
		return false, false, fmt.Errorf("comparison for %q has non-numeric value %T", comparison.Ref, comparison.Value)
	}
	return accept(left.Cmp(right)), false, nil
}

func compareEqual(left, right any) (bool, error) {
	leftNumber, leftNumeric := number(left)
	rightNumber, rightNumeric := number(right)
	if leftNumeric || rightNumeric {
		if !leftNumeric || !rightNumeric {
			return false, nil
		}
		return leftNumber.Cmp(rightNumber) == 0, nil
	}
	leftArray, leftIsArray := left.([]any)
	rightArray, rightIsArray := right.([]any)
	if leftIsArray || rightIsArray {
		if !leftIsArray || !rightIsArray || len(leftArray) != len(rightArray) {
			return false, nil
		}
		for index := range leftArray {
			equal, err := compareEqual(leftArray[index], rightArray[index])
			if err != nil || !equal {
				return false, err
			}
		}
		return true, nil
	}
	leftObject, leftIsObject := left.(map[string]any)
	rightObject, rightIsObject := right.(map[string]any)
	if leftIsObject || rightIsObject {
		if !leftIsObject || !rightIsObject || len(leftObject) != len(rightObject) {
			return false, nil
		}
		for key, leftValue := range leftObject {
			rightValue, ok := rightObject[key]
			if !ok {
				return false, nil
			}
			equal, err := compareEqual(leftValue, rightValue)
			if err != nil || !equal {
				return false, err
			}
		}
		return true, nil
	}
	return reflect.DeepEqual(left, right), nil
}

func number(value any) (*big.Rat, bool) {
	var text string
	switch value := value.(type) {
	case int:
		text = strconv.FormatInt(int64(value), 10)
	case int8:
		text = strconv.FormatInt(int64(value), 10)
	case int16:
		text = strconv.FormatInt(int64(value), 10)
	case int32:
		text = strconv.FormatInt(int64(value), 10)
	case int64:
		text = strconv.FormatInt(value, 10)
	case uint:
		text = strconv.FormatUint(uint64(value), 10)
	case uint8:
		text = strconv.FormatUint(uint64(value), 10)
	case uint16:
		text = strconv.FormatUint(uint64(value), 10)
	case uint32:
		text = strconv.FormatUint(uint64(value), 10)
	case uint64:
		text = strconv.FormatUint(value, 10)
	case float32:
		text = strconv.FormatFloat(float64(value), 'g', -1, 32)
	case float64:
		text = strconv.FormatFloat(value, 'g', -1, 64)
	case json.Number:
		text = string(value)
	default:
		return nil, false
	}
	result, ok := new(big.Rat).SetString(text)
	return result, ok
}

// LookupVariable resolves an exact or dotted reference from the flat
// namespaced runtime context. Nil values are absent optionals.
func LookupVariable(vars map[string]any, ref string) (any, bool) {
	if value, ok := vars[ref]; ok {
		return value, value != nil
	}
	parts := strings.Split(ref, ".")
	for prefixLength := len(parts) - 1; prefixLength > 0; prefixLength-- {
		value, ok := vars[strings.Join(parts[:prefixLength], ".")]
		if !ok || value == nil {
			continue
		}
		for _, field := range parts[prefixLength:] {
			object, objectOK := value.(map[string]any)
			if !objectOK {
				ok = false
				break
			}
			value, ok = object[field]
			if !ok || value == nil {
				break
			}
		}
		if ok && value != nil {
			return value, true
		}
	}
	return nil, false
}
