package def

// This file holds the structural rules one predicate node obeys wherever it
// appears, exactly once. A static validator and a runtime
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
