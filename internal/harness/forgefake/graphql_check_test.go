package forgefake

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestParseGraphQLOperation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, doc string
		want      GraphQLOperation
	}{
		{
			name: "uses in arguments and directives",
			doc:  "query Q($a: ID!, $b: [String!] = [\"x\", \"y\"], $c: Boolean = true) { n(id: $a) { f(in: $b) @include(if: $c) g(id: $a) } }",
			want: GraphQLOperation{Kind: "query", Name: "Q", Variables: []GraphQLVariable{
				{Name: "a", Type: "ID!", NonNull: true}, {Name: "b", Type: "[String!]", HasDefault: true}, {Name: "c", Type: "Boolean", HasDefault: true},
			}, Uses: []string{"a", "b", "c"}},
		},
		{
			name: "dollars in strings, block strings and comments are not uses",
			doc:  "mutation M($a: String!) {\n  # $notUse\n  m(text: \"$no \\\" $no\", other: \"\"\"$no \\\"\"\" $no\"\"\", v: $a) { x }\n}",
			want: GraphQLOperation{Kind: "mutation", Name: "M", Variables: []GraphQLVariable{{Name: "a", Type: "String!", NonNull: true}}, Uses: []string{"a"}},
		},
		{
			name: "object default and a directive on the definition",
			doc:  "query Q($o: In = {k: [1, -2.5e3], s: \"$x\"} @deprecated(reason: \"r\"), $n: Int!) { f(o: $o, n: $n) }",
			want: GraphQLOperation{Kind: "query", Name: "Q", Variables: []GraphQLVariable{
				{Name: "o", Type: "In", HasDefault: true}, {Name: "n", Type: "Int!", NonNull: true},
			}, Uses: []string{"o", "n"}},
		},
		{
			name: "anonymous",
			doc:  "{ viewer { login } }",
			want: GraphQLOperation{Kind: "query"},
		},
		{
			name: "use in the operation's own directive",
			doc:  "query Q($a: Boolean!) @live(if: $a) { x }",
			want: GraphQLOperation{Kind: "query", Name: "Q", Variables: []GraphQLVariable{{Name: "a", Type: "Boolean!", NonNull: true}}, Uses: []string{"a"}},
		},
	}
	for _, tc := range cases {
		got, err := ParseGraphQLOperation(tc.doc)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got.Kind != tc.want.Kind || got.Name != tc.want.Name || !slices.Equal(got.Variables, tc.want.Variables) || !slices.Equal(got.Uses, tc.want.Uses) {
			t.Errorf("%s = %+v, want %+v", tc.name, got, tc.want)
		}
	}
	for name, doc := range map[string]string{
		"a fragment":           "query Q { ...F } fragment F on T { x }",
		"two operations":       "query A { x } query B { y }",
		"unclosed":             "query Q($a: ID!) { x(a: $a)",
		"unclosed string":      "query Q { x(s: \"abc) }",
		"no type":              "query Q($a) { x }",
		"not an operation":     "fragment F on T { x }",
		"a bare dollar":        "query Q { x(a: $) }",
		"unbalanced":           "query Q { x } }",
		"unclosed block":       "query Q { x(s: \"\"\"abc) }",
		"unexpected character": "query Q { x(a: %) }",
	} {
		if _, err := ParseGraphQLOperation(doc); err == nil {
			t.Errorf("%s: read without an error", name)
		}
	}
}

func TestGraphQLRequestProblems(t *testing.T) {
	t.Parallel()
	codes := func(problems []GraphQLProblem) []string {
		var out []string
		for _, p := range problems {
			out = append(out, p.Code+" "+p.Variable)
		}
		return out
	}
	cases := []struct {
		name, doc string
		vars      map[string]any
		want      []string
	}{
		{"valid", "query Q($a: ID!, $b: String) { f(a: $a, b: $b) }", map[string]any{"a": "1"}, nil},
		{"a nullable sent as null", "query Q($a: ID!, $b: String) { f(a: $a, b: $b) }", map[string]any{"a": "1", "b": nil}, nil},
		{"a non-null with a default left out", "query Q($a: Int! = 3) { f(a: $a) }", nil, nil},
		{
			name: "used but not declared (ReactionTarget's shape)",
			doc:  "query ReactionTarget($id: ID!) { node(id: $id) { ... on IssueComment { reactionGroups @include(if: $wantReactions) { content } } } }",
			vars: map[string]any{"id": "IC_1"},
			want: []string{"variableNotDefined wantReactions"},
		},
		{"declared but not used", "query Q($a: ID!, $b: ID) { f(a: $a) }", map[string]any{"a": "1"}, []string{"variableNotUsed b"}},
		{"a non-null left out", "query Q($a: ID!) { f(a: $a) }", map[string]any{}, []string{"variableMismatch a"}},
		{"a non-null sent as null", "query Q($a: ID!) { f(a: $a) }", map[string]any{"a": nil}, []string{"variableMismatch a"}},
		{"sent but not declared", "query Q($a: ID!) { f(a: $a) }", map[string]any{"a": "1", "z": true, "y": 1}, []string{"variableNotDeclared y", "variableNotDeclared z"}},
		{"declared twice", "query Q($a: ID, $a: ID) { f(a: $a) }", nil, []string{"variableNotUnique a"}},
		{"unreadable", "query Q { ...F } fragment F on T { x }", nil, []string{"parseError "}},
	}
	for _, tc := range cases {
		if got := codes(GraphQLRequestProblems(tc.doc, tc.vars)); !slices.Equal(got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A request the forges' validation refuses is answered as they answer
// it (200, the errors, no data), runs nothing, and is recorded as
// unhandled so a spec's expectEveryForgeCallHandled fails on it.
func TestGraphQLValidation(t *testing.T) {
	t.Parallel()
	e, _ := seeded(t, Options{})
	srv := testServer(t, e)
	tick := map[string]any{"owner": "acme", "name": "widgets", "number": 7, "wantDetail": true, "wantThreads": false, "wantEvents": false, "wantChecks": false, "wantReactions": false}
	doc := graphQLDocument(t, "PRTick")
	send := func(path, query, op string, vars map[string]any) httpAnswer {
		return httpRequest(t, srv, "POST", "github.com", path, mustJSON(t, map[string]any{"query": query, "operationName": op, "variables": vars}), nil)
	}
	withUndeclared := map[string]any{"wantCI": true}
	for k, v := range tick {
		withUndeclared[k] = v
	}
	cases := []struct {
		name, path, query, op string
		vars                  map[string]any
		code                  string
	}{
		{"used but not declared", "/github/graphql", "query ReactionTarget($id: ID!) { node(id: $id) { ... on IssueComment { reactionGroups @include(if: $wantReactions) { content } } } }", "ReactionTarget", map[string]any{"id": "IC_ao1"}, "variableNotDefined"},
		{"declared but not used", "/github/graphql", strings.Replace(doc, "(", "($wantCI: Boolean, ", 1), "PRTick", tick, "variableNotUsed"},
		{"sent but not declared", "/github/graphql", doc, "PRTick", withUndeclared, "variableNotDeclared"},
		{"a non-null left out", "/github/graphql", "query ReactionTarget($id: ID!) { node(id: $id) { id } }", "ReactionTarget", map[string]any{}, "variableMismatch"},
		{"GitLab", "/gitlab/api/graphql", "query MRNoteReactions($fullPath: ID!, $iid: String!, $after: String) { project(fullPath: $fullPath) { mergeRequest(iid: $iid) { notes(after: $after, first: $first) { nodes { id } } } } }", "MRNoteReactions", map[string]any{"fullPath": "acme/widgets", "iid": "1"}, "variableNotDefined"},
	}
	for _, tc := range cases {
		got := send(tc.path, tc.query, tc.op, tc.vars)
		var body struct {
			Data   json.RawMessage `json:"data"`
			Errors []struct {
				Message    string `json:"message"`
				Extensions struct {
					Code string `json:"code"`
				} `json:"extensions"`
			} `json:"errors"`
		}
		if got.status != 200 || json.Unmarshal([]byte(got.body), &body) != nil || body.Data != nil || len(body.Errors) != 1 || body.Errors[0].Extensions.Code != tc.code {
			t.Errorf("%s = %d %s, want 200 with one %s error and no data", tc.name, got.status, got.body, tc.code)
		}
	}
	invocations := e.Invocations(0).Invocations
	if len(invocations) != len(cases) {
		t.Fatalf("recorded %d invocations, want %d", len(invocations), len(cases))
	}
	for i, inv := range invocations {
		if !inv.Unhandled || inv.Route != "" || !strings.Contains(inv.Detail, "invalid") || !strings.Contains(inv.Detail, cases[i].op) {
			t.Errorf("%s recorded %+v, want unhandled with the reason", cases[i].name, inv)
		}
	}
}
