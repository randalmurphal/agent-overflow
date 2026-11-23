package trackerfake

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// fullLinear is a workspace with a record in every connection a filter
// reaches and each issue relation set, so a filter meets data.
func fullLinear() Fixture {
	f := testFixture()
	f.Jira = nil
	w := pagingLinear()
	i1 := &w.Issues[0]
	i1.Assignee, i1.Cycle, i1.Project, i1.DueDate = "u1", "cy1", w.Projects[0].ID, "2026-01-01"
	i1.CompletedAt, i1.Description = testStart, "Some"
	f.Linear[0] = w
	return f
}

// unreadOperands are operand forms the schema admits and the executor
// does not read, by scalar. Every operand of another scalar is read.
var unreadOperands = map[string][]string{
	"DateTimeOrDuration":     {"-P1D", "P2W", "2026-01-01"},
	"TimelessDateOrDuration": {"P2W", "-P1D", "2026-01-01T00:00:00.000Z"},
}

// TestEveryFilterComparator sends every comparator and member the schema
// defines for every filter the executor reads, and for each comparator
// it implements each operand form it does not read, to a workspace with
// nothing to filter and to one with a record everywhere: what the tables
// implement is answered on both, anything else is unhandled on both.
func TestEveryFilterComparator(t *testing.T) {
	s := linearSchema(t)
	rigs := map[string]*rig{"empty": newRig(t, emptyLinear()), "full": newRig(t, fullLinear())}
	probes := filterProbes(s)
	if len(probes) < 100 {
		t.Fatalf("only %d probes", len(probes))
	}
	handled, forms := 0, 0
	for _, p := range probes {
		type want struct {
			filter  map[string]any
			handled bool
		}
		wants := []want{{p.filter, p.handled}}
		if p.handled && p.op != "null" {
			handled++
			for _, form := range unreadOperands[p.scalar] {
				var operand any = form
				if p.list {
					operand = []any{form}
				}
				wants = append(wants, want{p.at(map[string]any{p.op: operand}), false})
				forms++
			}
		}
		for _, w := range wants {
			for name, g := range rigs {
				body := linearBody("P", "query P($f: "+p.rootFilter+") { "+p.doc+" }", map[string]any{"f": w.filter})
				rec := g.do(call{origin: linearOrigin, method: "POST", path: "/graphql", body: body})
				if got := rec.Code == 200; got != w.handled || strings.Contains(rec.Body.String(), "panicked") {
					f, _ := json.Marshal(w.filter)
					t.Errorf("%s, %s with filter %s: answered %d, want handled %t: %.300s", name, p.doc, f, rec.Code, w.handled, rec.Body)
				}
			}
		}
	}
	for scalar := range unreadOperands {
		if operandKinds[scalar] == nil {
			t.Errorf("unread operands of %s, which has no comparators", scalar)
		}
	}
	if forms == 0 {
		t.Fatal("no operand form was refused")
	}
	t.Logf("%d probes, %d of them implemented comparators, %d unread operand forms", len(probes), handled, forms)
}

// matchRep is a field compared on an operand kind: records A and B,
// seeded by seed, hold a and b, a before b. For a null rep, A's is set
// and B's unset.
type matchRep struct {
	list, filterType, key string
	ids                   [2]string
	a, b                  any
	seed                  func(w *LinearWorkspace)
}

func issueRep(key string, a, b any, set func(is *LinearIssue, i int)) matchRep {
	return matchRep{list: "issues", filterType: "IssueFilter", key: key, ids: [2]string{"i1", "i2"}, a: a, b: b,
		seed: func(w *LinearWorkspace) {
			for i := range w.Issues {
				set(&w.Issues[i], i)
			}
		}}
}

var (
	later = testStart.Add(time.Hour)
	// matchReps has a field for each operand kind; nullReps for each kind
	// a nullable comparator compares.
	matchReps = map[string]matchRep{
		"String": issueRep("title", "Alpha", "Beta", func(is *LinearIssue, i int) { is.Title = []string{"Alpha", "Beta"}[i] }),
		"ID":     issueRep("id", "i1", "i2", func(*LinearIssue, int) {}),
		"Float":  issueRep("number", 1, 2, func(is *LinearIssue, i int) { is.Number = float64(i + 1) }),
		"DateTimeOrDuration": issueRep("updatedAt", linearTime(testStart), linearTime(later),
			func(is *LinearIssue, i int) { is.UpdatedAt = []time.Time{testStart, later}[i] }),
		"TimelessDateOrDuration": issueRep("dueDate", "2026-01-01", "2026-02-01",
			func(is *LinearIssue, i int) { is.DueDate = []string{"2026-01-01", "2026-02-01"}[i] }),
		"Boolean": {list: "users", filterType: "UserFilter", key: "admin", ids: [2]string{"u1", "u2"}, a: true, b: false,
			seed: func(w *LinearWorkspace) {
				w.Users = []LinearUser{{ID: "u1", Name: "User 1", Admin: true}, {ID: "u2", Name: "User 2"}}
			}},
	}
	nullReps = map[string]matchRep{
		"String": issueRep("description", nil, nil, func(is *LinearIssue, i int) { is.Description = []string{"Some", ""}[i] }),
		"Float": issueRep("estimate", nil, nil, func(is *LinearIssue, i int) {
			if i == 0 {
				is.Estimate = new(float64)
			}
		}),
		"DateTimeOrDuration": issueRep("completedAt", nil, nil, func(is *LinearIssue, i int) {
			if i == 0 {
				is.CompletedAt = testStart
			}
		}),
		"TimelessDateOrDuration": issueRep("dueDate", nil, nil, func(is *LinearIssue, i int) {
			if i == 0 {
				is.DueDate = "2026-01-01"
			}
		}),
	}
	// matchOps are each comparator's operand, from A's and B's values, and
	// the record it matches: 0 for A, 1 for B.
	matchOps = map[string]struct {
		operand func(a, b any) any
		match   int
	}{
		"eq":           {func(a, _ any) any { return a }, 0},
		"neq":          {func(a, _ any) any { return a }, 1},
		"in":           {func(a, _ any) any { return []any{a} }, 0},
		"nin":          {func(a, _ any) any { return []any{a} }, 1},
		"gt":           {func(a, _ any) any { return a }, 1},
		"gte":          {func(_, b any) any { return b }, 1},
		"lt":           {func(_, b any) any { return b }, 0},
		"lte":          {func(a, _ any) any { return a }, 0},
		"eqIgnoreCase": {func(a, _ any) any { return strings.ToUpper(a.(string)) }, 0},
	}
)

// TestEveryComparatorMatchesOne runs each comparator of each operand
// kind, null included, on two records of which exactly one matches, so
// a comparator that matches nothing, or everything, fails.
func TestEveryComparatorMatchesOne(t *testing.T) {
	needNull := map[string]bool{}
	for _, p := range filterProbes(linearSchema(t)) {
		if p.handled && p.op == "null" && p.scalar != "" {
			needNull[p.scalar] = true
		}
	}
	run := func(t *testing.T, rep matchRep, cond map[string]any, want string) {
		t.Helper()
		f := txnFixture()
		f.Jira = nil
		rep.seed(&f.Linear[0])
		g := newRig(t, f)
		body := linearBody("P", "query P($f: "+rep.filterType+") { "+rep.list+"(filter: $f) { nodes { id } } }", map[string]any{"f": map[string]any{rep.key: cond}})
		rec := g.do(call{origin: linearOrigin, method: "POST", path: "/graphql", body: body})
		if got := ids(t, rec.Body.String(), "data."+rep.list+".nodes.[].id"); rec.Code != 200 || !slices.Equal(got, []string{want}) {
			c, _ := json.Marshal(cond)
			t.Errorf("%s %s answered %d with %v, want [%s]: %.300s", rep.key, c, rec.Code, got, want, rec.Body)
		}
	}
	for scalar, kind := range operandKinds {
		rep, ok := matchReps[scalar]
		if !ok {
			t.Errorf("no field compares %s", scalar)
			continue
		}
		for op := range kind.ops {
			m, ok := matchOps[op]
			if !ok {
				t.Errorf("no expectation for %s on %s", op, scalar)
				continue
			}
			t.Run(scalar+"/"+op, func(t *testing.T) { run(t, rep, map[string]any{op: m.operand(rep.a, rep.b)}, rep.ids[m.match]) })
		}
		if !needNull[scalar] {
			continue
		}
		nr, ok := nullReps[scalar]
		if !ok {
			t.Errorf("no nullable field compares %s", scalar)
			continue
		}
		t.Run(scalar+"/null", func(t *testing.T) {
			run(t, nr, map[string]any{"null": true}, nr.ids[1])
			run(t, nr, map[string]any{"null": false}, nr.ids[0])
		})
	}
}

// TestRelationAndLogicFilters: a nested filter passes no record whose
// relation is unset, null reads the relation itself, some passes a
// record with one matching item, and and or combine.
func TestRelationAndLogicFilters(t *testing.T) {
	f := txnFixture()
	f.Jira = nil
	f.Linear[0].Issues[0].Assignee, f.Linear[0].Issues[0].Labels = "u1", []string{"l1"}
	g := newRig(t, f)
	for _, c := range []struct {
		filter string
		want   []string
	}{
		{`{assignee: {id: {neq: "none"}}}`, []string{"i1"}},
		{`{assignee: {null: true}}`, []string{"i2"}},
		{`{assignee: {null: false}}`, []string{"i1"}},
		{`{labels: {some: {id: {eq: "l1"}}}}`, []string{"i1"}},
		{`{and: [{title: {eq: "First"}}, {id: {eq: "i2"}}]}`, nil},
		{`{and: [{title: {eq: "First"}}, {id: {eq: "i1"}}]}`, []string{"i1"}},
		{`{or: [{id: {eq: "none"}}, {id: {eq: "i2"}}]}`, []string{"i2"}},
	} {
		rec := g.do(linearReq("Q", "query Q { issues(filter: "+c.filter+") { nodes { id } } }"))
		if got := ids(t, rec.Body.String(), "data.issues.nodes.[].id"); rec.Code != 200 || !slices.Equal(got, c.want) {
			t.Errorf("%s answered %d with %v, want %v: %.300s", c.filter, rec.Code, got, c.want, rec.Body)
		}
	}
}
