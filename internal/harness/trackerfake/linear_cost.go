package trackerfake

import (
	"github.com/vektah/gqlparser/v2/ast"
)

// Linear's points for a request, the X-Complexity it answers, follow
// the measurements in docs/references/tracker-api-measurements.md
// (Linear). Linear computes them from the document alone: a page of
// issues costs the same whatever it returns and whatever its first, and
// a history page nested in it the same for 1, 20 or 42 issues
// (measured), so the fake does too (linearCost).
//
//   - A request costs 1, and each mutation field in it 2 whatever its
//     payload (fitted to two points: three aliased issueUpdate cost 7,
//     issueBatchUpdate of three issues 3; one mutation costs 2 to 8,
//     measured).
//   - An object costs 1 and a scalar nothing (fitted: labels by ID cost
//     4 for one ID and 10 for three, teams by key 9 for two keys, each
//     exactly what this gives).
//   - A catalog connection, any connection not of issues, costs its
//     first times the cost of one node (measured: charged by first).
//   - A page of issues (issues, searchIssues) costs 3 whatever it
//     selects of each issue, plus the connections nested in its nodes,
//     each once (fitted: 48 issues with id and updatedAt cost 4, 42
//     issues with card fields and labels 8).
//   - Labels nested in an issue cost 4 whatever their first (fitted:
//     two issues cost 3 more with labels than without; the fit charges
//     4 so the card page is 8).
//   - A history page nested in an issue costs 4 an entry of its first
//     (fitted: 203, 403 and 1,003 points at first 50, 100 and 250, which
//     this gives as 204, 404 and 1,004).
//
// A connection's default first is 50 (documented).
const (
	linearRequestPoints   = 1
	linearMutationPoints  = 2
	linearIssuePagePoints = 3
	linearLabelsPoints    = 4
	linearHistoryEntry    = 4
	linearDefaultFirst    = 50
)

// linearCost is the points op costs, by the rules above.
func (r *gqlRun) linearCost(op *ast.OperationDefinition) int {
	cost := linearRequestPoints
	if op.Operation == ast.Mutation {
		return cost + linearMutationPoints*len(r.collectFields("Mutation", op.SelectionSet))
	}
	for _, c := range r.collectFields("Query", op.SelectionSet) {
		cost += r.fieldCost(c, false)
	}
	return cost
}

// fieldCost is the points of the fields c collects. inIssue reports a
// field of an issue, whose nested labels and history follow their own
// rules.
func (r *gqlRun) fieldCost(c collected, inIssue bool) int {
	f := c.fields[0]
	if f.Definition == nil {
		return 0
	}
	typ := f.Definition.Type.Name()
	var sub ast.SelectionSet
	for _, g := range c.fields {
		sub = append(sub, g.SelectionSet...)
	}
	switch {
	case isConnection(typ):
		return r.connectionCost(f, typ, sub, inIssue)
	case r.isObject(typ):
		return r.objectCost(typ, sub)
	}
	return 0
}

// objectCost is one object of typ under the selection sub.
func (r *gqlRun) objectCost(typ string, sub ast.SelectionSet) int {
	cost := 1
	for _, c := range r.collectFields(typ, sub) {
		cost += r.fieldCost(c, typ == "Issue" || typ == "IssueSearchResult")
	}
	return cost
}

// connectionCost is a connection field f of typ under the selection sub.
func (r *gqlRun) connectionCost(f *ast.Field, typ string, sub ast.SelectionSet, inIssue bool) int {
	first := linearDefaultFirst
	if v, ok := fieldArgs(f, r.vars)["first"]; ok {
		first = int(toFloat(v))
	}
	node, nodes := r.nodeSelection(typ, sub)
	switch {
	case typ == "IssueConnection" || typ == "IssueSearchPayload":
		if inIssue && f.Name != "issues" {
			// children of an issue: a catalog of issues by first.
			return first * r.objectCost(node, nodes)
		}
		cost := linearIssuePagePoints
		for _, c := range r.collectFields(node, nodes) {
			if g := c.fields[0]; g.Definition != nil && isConnection(g.Definition.Type.Name()) {
				cost += r.fieldCost(c, true)
			}
		}
		return cost
	case inIssue && typ == "IssueLabelConnection":
		return linearLabelsPoints
	case typ == "IssueHistoryConnection":
		return first * linearHistoryEntry
	}
	return first * r.objectCost(node, nodes)
}

// nodeSelection is the node type of connection typ and the selection of
// its nodes under sub.
func (r *gqlRun) nodeSelection(typ string, sub ast.SelectionSet) (string, ast.SelectionSet) {
	var node string
	var nodes ast.SelectionSet
	for _, c := range r.collectFields(typ, sub) {
		if g := c.fields[0]; g.Name == "nodes" && g.Definition != nil {
			node = g.Definition.Type.Name()
			for _, h := range c.fields {
				nodes = append(nodes, h.SelectionSet...)
			}
		}
	}
	return node, nodes
}

// isObject reports a schema type that is an object, interface or union:
// one that costs a point.
func (r *gqlRun) isObject(typ string) bool {
	def := r.s.Types[typ]
	return def != nil && (def.Kind == ast.Object || def.Kind == ast.Interface || def.Kind == ast.Union)
}
