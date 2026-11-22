package trackerfake

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestFieldCollection: a selection set is answered once per response
// key, its fields gathered from the fragments it spreads, and the
// sub-selections of one key's fields merged.
func TestFieldCollection(t *testing.T) {
	for _, c := range []struct {
		name, query string
		created     int
		want        []string
	}{
		{"one mutation spread twice runs once",
			`mutation Q { ...C ...C } fragment C on Mutation { issueCreate(input: {teamId: "t1", title: "Once"}) { success issue { title } } }`,
			1, []string{`"success":true`, `"title":"Once"`}},
		{"one mutation written twice runs once",
			`mutation Q { issueCreate(input: {teamId: "t1", title: "Once"}) { success } issueCreate(input: {teamId: "t1", title: "Once"}) { issue { title } } }`,
			1, []string{`"success":true`, `"title":"Once"`}},
		{"one field in a fragment and outside it",
			`query Q { issue(id: "i1") { id team { id } } ...F } fragment F on Query { issue(id: "i1") { title team { name } } }`,
			0, []string{`"id":"i1"`, `"title":"First"`, `"team":{"id":"t1","name":"Web"}`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRig(t, txnFixture())
			rec := g.do(linearReq("Q", c.query))
			body := rec.Body.String()
			created := len(g.e.Snapshot().Linear[0].Issues) - len(txnFixture().Linear[0].Issues)
			if rec.Code != 200 || created != c.created || strings.Contains(body, "errors") {
				t.Fatalf("answered %d, created %d issues, want %d: %s", rec.Code, created, c.created, body)
			}
			for _, w := range c.want {
				if !strings.Contains(body, w) {
					t.Errorf("the answer lacks %s: %s", w, body)
				}
			}
		})
	}
}

// TestNullPropagation: an error leaves its field null, and a non-null
// field left null makes its parent null in turn, up to the first field
// that may be null, or the whole data.
func TestNullPropagation(t *testing.T) {
	for _, c := range []struct {
		name, query string
		// data is the answer's data, as JSON.
		data string
	}{
		{"a missing issue in a root fragment", `query Q { viewer { id } ...F } fragment F on Query { issue(id: "missing") { id } }`, `null`},
		{"a page too large in a list item", `query Q { issues { nodes { id labels(first: 300) { nodes { id } } } } }`, `null`},
		{"a page too large under a field that may be null", `query Q { issue(id: "i2") { id parent { id labels(first: 300) { nodes { id } } } } }`,
			`{"issue":{"id":"i2","parent":null}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := txnFixture()
			f.Linear[0].Issues[1].Parent = "i1"
			g := newRig(t, f)
			rec := g.do(linearReq("Q", c.query))
			var out struct {
				Data   json.RawMessage
				Errors []any
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if rec.Code != 200 || string(out.Data) != c.data || len(out.Errors) != 1 {
				t.Fatalf("answered %d %s, want data %s and one error", rec.Code, rec.Body, c.data)
			}
		})
	}
}
