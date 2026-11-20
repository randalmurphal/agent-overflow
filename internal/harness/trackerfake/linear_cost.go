package trackerfake

import (
	"github.com/vektah/gqlparser/v2/ast"
)

// Linear's points for a request, the X-Complexity it answers, follow
// the measurements in docs/references/tracker-api-measurements.md
// (Linear). Linear computes them from the document alone: a page of
// issues costs the same whatever its first, and a history page or a
// catalog nested in it the same for 1, 10, 20, 42 or 50 issues
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
//   - A page of issues (issues, searchIssues) costs 1, and 2 more when
//     its nodes select any object, whatever its first and however many
//     objects (fitted: issues(first: 10) with only IDs cost 2, with the
//     team's ID and name, the assignee's and the state's IDs 4; 42 issues
//     with card fields and labels 8; positions, IDs, sort orders and
//     team IDs, 4). The connections nested in its nodes are added once.
//   - A catalog under an object of an issue node, at any depth, costs a
//     fifth of its first times the cost of one node, rounded down, whatever
//     the page's first (fitted: issues(first: F) with team { members(first:
//     N) { nodes { id } } } cost 4 at N=1, 14 at N=50 and 54 at N=250 for F
//     of 1, 10 and 50, exactly what this gives).
//   - Labels nested in an issue cost 4 whatever their first (fitted:
//     two issues cost 3 more with labels than without; the fit charges
//     4 so the card page is 8).
//   - A history page nested in an issue costs 1 and 4 an entry of its
//     first (fitted: 203, 403 and 1,003 points at first 50, 100 and 250,
//     exactly what this gives).
//
// A connection's default first is 50 (documented).
const (
	linearRequestPoints      = 1
	linearMutationPoints     = 2
	linearIssuePagePoints    = 1
	linearIssueObjectsPoints = 2
	linearNestedCatalogShare = 5 // a catalog under an issue's object costs 1/5
	linearLabelsPoints       = 4
	linearHistoryPagePoints  = 1
	linearHistoryEntry       = 4
	linearDefaultFirst       = 50
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
	f, typ, sub := r.fieldOf(c)
	switch {
	case f == nil:
		return 0
	case isConnection(typ):
		return r.connectionCost(f, typ, sub, inIssue)
	case r.isObject(typ):
		return r.objectCost(typ, sub)
	}
	return 0
}

// fieldOf is the field c collects, its type and its merged selection;
// a nil field when the schema does not define it.
func (r *gqlRun) fieldOf(c collected) (*ast.Field, string, ast.SelectionSet) {
	f := c.fields[0]
	if f.Definition == nil {
		return nil, "", nil
	}
	var sub ast.SelectionSet
	for _, g := range c.fields {
		sub = append(sub, g.SelectionSet...)
	}
	return f, f.Definition.Type.Name(), sub
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
	first := r.first(f)
	node, nodes := r.nodeSelection(typ, sub)
	switch {
	case typ == "IssueConnection" || typ == "IssueSearchPayload":
		if inIssue && f.Name != "issues" {
			// children of an issue: a catalog of issues by first.
			return first * r.objectCost(node, nodes)
		}
		return linearIssuePagePoints + r.issueNodesCost(node, nodes)
	case inIssue && typ == "IssueLabelConnection":
		return linearLabelsPoints
	case typ == "IssueHistoryConnection":
		return linearHistoryPagePoints + first*linearHistoryEntry
	}
	return first * r.objectCost(node, nodes)
}

// issueNodesCost is what the node selection sel of an issue page adds,
// once whatever the page's first: its objects' 2, the catalogs under
// them and the connections of each issue.
func (r *gqlRun) issueNodesCost(typ string, sel ast.SelectionSet) int {
	cost, objects := 0, false
	for _, c := range r.collectFields(typ, sel) {
		switch f, ft, sub := r.fieldOf(c); {
		case f == nil:
		case isConnection(ft):
			cost += r.connectionCost(f, ft, sub, true)
		case r.isObject(ft):
			objects = true
			cost += r.nestedCatalogsCost(ft, sub)
		}
	}
	if objects {
		cost += linearIssueObjectsPoints
	}
	return cost
}

// nestedCatalogsCost is the catalogs under sel, an object of typ under an
// issue node, at any depth: each a fifth of its first times one node.
func (r *gqlRun) nestedCatalogsCost(typ string, sel ast.SelectionSet) int {
	cost := 0
	for _, c := range r.collectFields(typ, sel) {
		switch f, ft, sub := r.fieldOf(c); {
		case f == nil:
		case isConnection(ft):
			node, nodes := r.nodeSelection(ft, sub)
			cost += r.first(f) * r.objectCost(node, nodes) / linearNestedCatalogShare
		case r.isObject(ft):
			cost += r.nestedCatalogsCost(ft, sub)
		}
	}
	return cost
}

// first is the page size f asks for, the default when it names none.
func (r *gqlRun) first(f *ast.Field) int {
	if v, ok := fieldArgs(f, r.vars)["first"]; ok {
		return int(toFloat(v))
	}
	return linearDefaultFirst
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
