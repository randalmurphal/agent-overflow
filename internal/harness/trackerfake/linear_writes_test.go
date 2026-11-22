package trackerfake

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

// TestPrePassRefusesWhatNeverRuns: a field, argument, input member or
// directive the executor lacks is unhandled even where nothing would
// read it: under an empty list, or in a mutation that fails first.
func TestPrePassRefusesWhatNeverRuns(t *testing.T) {
	for _, c := range []struct{ name, query, want string }{
		{"field under an empty list", `query Q { issues { nodes { snoozedUntilAt } } }`, "field Issue.snoozedUntilAt"},
		{"argument under an empty list", `query Q { issues { nodes { children(last: 1) { nodes { id } } } } }`, "argument last"},
		{"null and some of an empty list", `query Q { issues(filter: {labels: {null: false, some: {name: {eq: "x"}}}}) { nodes { id } } }`, "other than some"},
		{"some and every of an empty list", `query Q { issues(filter: {labels: {some: {name: {eq: "x"}}, every: {name: {eq: "y"}}}}) { nodes { id } } }`, "other than some"},
		{"sort of an empty list", `query Q { issues(sort: [{title: {order: Ascending}}]) { nodes { id } } }`, "sort of Issue by title"},
		{"input member of a missing issue", `mutation Q { issueUpdate(id: "missing", input: {snoozedUntilAt: "2026-12-01T00:00:00Z"}) { success } }`, "input member IssueUpdateInput.snoozedUntilAt"},
		{"payload field after a write", `mutation Q { issueCreate(input: {teamId: "t1", title: "New"}) { success issue { snoozedUntilAt } } }`, "field Issue.snoozedUntilAt"},
		{"skip on a mutation", `mutation Q { issueCreate(input: {teamId: "t1", title: "New"}) @skip(if: true) { success } }`, "directive @skip"},
		{"include on a field", `query Q { viewer { id @include(if: false) } }`, "directive @include"},
		{"directive on a fragment", `query Q { viewer { ...F @include(if: true) } } fragment F on User { id }`, "directive @include"},
		{"one response key with two arguments", `query Q { issue(id: "i1") { id } ...F } fragment F on Query { issue(id: "i2") { title } }`, "differing arguments"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRig(t, testFixture())
			before := g.e.Snapshot()
			rec := g.do(linearReq("Q", c.query))
			if rec.Code != 501 || !strings.Contains(rec.Body.String(), c.want) {
				t.Fatalf("answered %d %s, want 501 naming %q", rec.Code, rec.Body, c.want)
			}
			if after := g.e.Snapshot(); len(after.Linear[0].Issues) != len(before.Linear[0].Issues) {
				t.Fatal("a refused document wrote")
			}
		})
	}
}

// TestSharedInputVariable: two mutations reading one $input both read
// all of it; reading a variable never changes it.
func TestSharedInputVariable(t *testing.T) {
	g := newRig(t, txnFixture())
	rec := g.do(call{origin: linearOrigin, method: "POST", path: "/graphql", body: linearBody("M",
		`mutation M($input: IssueUpdateInput!) { a: issueUpdate(id: "i1", input: $input) { success } b: issueUpdate(id: "i2", input: $input) { success } }`,
		map[string]any{"input": map[string]any{"title": "Changed"}})})
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "errors") {
		t.Fatalf("answered %d %s", rec.Code, rec.Body)
	}
	for _, is := range g.e.Snapshot().Linear[0].Issues {
		if is.Title != "Changed" {
			t.Errorf("issue %s kept title %q", is.ID, is.Title)
		}
	}
}

// nullBases are a valid input for each mutation that takes one, and the
// arguments around it; TestNullMembers fails for a mutation with none.
var nullBases = map[string]struct {
	args  string
	input map[string]any
}{
	"issueCreate":         {"", map[string]any{"teamId": "t1", "title": "New"}},
	"issueUpdate":         {`id: "i1", `, map[string]any{}},
	"issueBatchUpdate":    {`ids: ["i1"], `, map[string]any{}},
	"commentCreate":       {"", map[string]any{"issueId": "i1", "body": "Hello"}},
	"commentUpdate":       {`id: "c1", `, map[string]any{"body": "Edited"}},
	"issueRelationCreate": {"", map[string]any{"issueId": "i1", "relatedIssueId": "i2", "type": "related"}},
	"attachmentCreate":    {"", map[string]any{"issueId": "i1", "url": "https://example.com/a", "title": "A"}},
}

// TestNullMembers sends null for every member the schema lets be null,
// of every mutation input the executor reads. None panics, none is
// unhandled, the engine keeps serving, and an issue member whose Issue
// field the schema declares non-null is refused with nothing changed.
func TestNullMembers(t *testing.T) {
	s := linearSchema(t)
	for _, name := range slices.Sorted(maps.Keys(mutations)) {
		spec := mutations[name]
		if spec.input == "" {
			continue
		}
		base, ok := nullBases[name]
		if !ok {
			t.Errorf("no null base for %s", name)
			continue
		}
		for _, m := range s.Types[spec.input].Fields {
			if m.Type.NonNull || !slices.Contains(linearInputs[spec.input], m.Name) {
				continue
			}
			t.Run(name+"."+m.Name, func(t *testing.T) {
				f := txnFixture()
				f.Linear[0].Comments = []LinearComment{{ID: "c1", Issue: "i1", User: "u1", Body: "Hi"}}
				g := newRig(t, f)
				before, err := json.Marshal(g.e.Snapshot())
				if err != nil {
					t.Fatal(err)
				}
				input := maps.Clone(base.input)
				input[m.Name] = nil
				rec := g.do(call{origin: linearOrigin, method: "POST", path: "/graphql", body: linearBody("M",
					"mutation M($input: "+spec.input+"!) { "+name+"("+base.args+"input: $input) { success } }", map[string]any{"input": input})})
				if rec.Code != 200 || len(g.e.Unhandled()) > 0 {
					t.Fatalf("answered %d %s", rec.Code, rec.Body)
				}
				if !g.e.mu.TryLock() {
					t.Fatal("the engine's lock is held")
				}
				g.e.mu.Unlock()
				refused := strings.Contains(rec.Body.String(), `"errors"`)
				if member := findIssueMember(m.Name); member != nil {
					after, err := json.Marshal(g.e.Snapshot())
					if err != nil {
						t.Fatal(err)
					}
					wantRefused := (&gqlRun{s: s}).nonNull("Issue", member.field)
					if refused != wantRefused || (refused && string(after) != string(before)) {
						t.Fatalf("refused %t, want %t; answered %s", refused, wantRefused, rec.Body)
					}
				}
				if rec := g.do(linearReq("Viewer", viewerQuery)); rec.Code != 200 {
					t.Fatalf("the next request answered %d", rec.Code)
				}
			})
		}
	}
}

// TestRuntimeCatchesTableDrift: an argument or input member a table
// lists but no resolver reads is unhandled when it is sent, so the
// pre-pass's tables cannot promise more than the resolvers do.
func TestRuntimeCatchesTableDrift(t *testing.T) {
	savedArgs, savedInputs := connectionArgs, linearInputs["IssueUpdateInput"]
	savedFields := maps.Clone(issueFields)
	t.Cleanup(func() {
		connectionArgs, linearInputs["IssueUpdateInput"] = savedArgs, savedInputs
		clear(issueFields)
		maps.Copy(issueFields, savedFields)
	})
	connectionArgs = append(slices.Clone(savedArgs), "last")
	linearInputs["IssueUpdateInput"] = append(slices.Clone(savedInputs), "snoozedUntilAt")
	// Fields whose table value is not what their filter compares.
	issueFields["title"] = func(issueObj) any { return 1.0 }
	issueFields["assignee"] = func(issueObj) any { return "u1" }
	issueFields["labels"] = func(issueObj) any { return []string{"l1"} }
	// A field whose comparators compare an operand kind not implemented.
	issueFields["slaStatus"] = func(issueObj) any { return nil }
	g := newRig(t, txnFixture())
	for _, q := range []string{
		`query Q { issues(last: 1) { nodes { id } } }`,
		`mutation Q { issueUpdate(id: "i1", input: {title: "Changed", snoozedUntilAt: "2026-12-01T00:00:00Z"}) { success } }`,
		`query Q { issues(filter: {title: {eq: "First"}}) { nodes { id } } }`,
		`query Q { issues(filter: {assignee: {id: {eq: "u1"}}}) { nodes { id } } }`,
		`query Q { issues(filter: {labels: {some: {id: {eq: "l1"}}}}) { nodes { id } } }`,
		`query Q { issues(filter: {slaStatus: {null: true}}) { nodes { id } } }`,
	} {
		if rec := g.do(linearReq("Q", q)); rec.Code != 501 {
			t.Errorf("%s answered %d %s", q, rec.Code, rec.Body)
		}
	}
	if g.e.Snapshot().Linear[0].Issues[0].Title != "First" {
		t.Error("the unhandled update applied")
	}
}

// TestEmptyStateIDRefused: an empty ID clears a reference as a null
// does, so an issue's state, which it always has, refuses one.
func TestEmptyStateIDRefused(t *testing.T) {
	g := newRig(t, txnFixture())
	rec := g.do(linearReq("M", `mutation M { issueUpdate(id: "i1", input: {stateId: ""}) { success } }`))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "stateId must not be null") || g.e.Snapshot().Linear[0].Issues[0].State != "s1" {
		t.Fatalf("answered %d %s", rec.Code, rec.Body)
	}
}

func findIssueMember(name string) *issueMember {
	for i := range issueMembers {
		if issueMembers[i].name == name {
			return &issueMembers[i]
		}
	}
	return nil
}
