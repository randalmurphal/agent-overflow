package def

import "fmt"

// This file holds the predicate surface that is not bound to a phase gate: the
// structural rules one predicate node obeys wherever it appears, plus the two
// entry points a caller outside a workflow uses (§11 automation run-if
// conditions evaluate the same expression language against a variable context
// the scheduler assembles).
//
// The structural rules live here exactly once. A static validator and a runtime
// evaluator that disagreed about "is this predicate well formed" would let a
// definition validate and then fail mid-run, which is the drift this file
// exists to make impossible.

// predicateShapeIssue is one structural problem with a single predicate node,
// independent of where its references resolve.
type predicateShapeIssue struct {
	code    string
	message string
}

// predicateShapeIssues reports one node's structural problems. `structural`
// is false when the node does not carry exactly one operator: nothing further
// can be checked (and no child can be recursed into) without a subject, so
// callers stop there rather than reporting cascading nonsense.
func predicateShapeIssues(predicate Predicate) (issues []predicateShapeIssue, structural bool) {
	if countPredicateOperators(predicate) != 1 {
		return []predicateShapeIssue{{
			code:    "predicate.operator",
			message: "predicate must declare exactly one supported operator",
		}}, false
	}
	if predicate.All != nil && len(predicate.All) == 0 {
		issues = append(issues, predicateShapeIssue{code: "predicate.all", message: "all requires at least one predicate"})
	}
	if predicate.Any != nil && len(predicate.Any) == 0 {
		issues = append(issues, predicateShapeIssue{code: "predicate.any", message: "any requires at least one predicate"})
	}
	if predicate.In != nil && len(predicate.In.Values) == 0 {
		issues = append(issues, predicateShapeIssue{code: "predicate.in", message: "in requires at least one value"})
	}
	return issues, true
}

// ValidatePredicateShape reports the structural findings of a whole predicate
// tree evaluated outside a workflow graph. It checks shape only — operator
// count and non-empty `all` / `any` / `in` — because the variable context a
// standalone predicate reads is supplied by its caller, not declared by a
// workflow, so there is nothing to resolve references against.
//
// A phase gate's routes are validated by the workflow validator instead, which
// applies these same rules plus the reference, dominance, and type checks a
// declared context makes possible.
func ValidatePredicateShape(predicate Predicate, element string) []Finding {
	findings := make([]Finding, 0)
	issues, structural := predicateShapeIssues(predicate)
	for _, issue := range issues {
		findings = append(findings, finding(issue.code, element, issue.message))
	}
	if !structural {
		return findings
	}
	for index, child := range predicate.All {
		findings = append(findings, ValidatePredicateShape(child, fmt.Sprintf("%s.all[%d]", element, index))...)
	}
	for index, child := range predicate.Any {
		findings = append(findings, ValidatePredicateShape(child, fmt.Sprintf("%s.any[%d]", element, index))...)
	}
	if predicate.Not != nil {
		findings = append(findings, ValidatePredicateShape(*predicate.Not, element+".not")...)
	}
	return findings
}

// EvaluatePredicate evaluates one predicate against a variable context, with no
// route, loop, or trace machinery around it. It is the §11 run-if entry point:
// same operators, same absent-reference semantics (an unresolved reference is
// false, never an error), same numeric comparison rules as a phase gate.
//
// An error means the predicate itself is malformed or a comparison is
// impossible (a non-numeric value under an ordered operator) — a condition its
// caller must surface, never quietly treat as false.
func EvaluatePredicate(predicate Predicate, vars map[string]any) (bool, error) {
	trace := GateTrace{Predicates: make([]PredicateTrace, 0)}
	result, _, err := evaluatePredicate(predicate, vars, -1, "condition", &trace)
	if err != nil {
		return false, err
	}
	return result, nil
}
