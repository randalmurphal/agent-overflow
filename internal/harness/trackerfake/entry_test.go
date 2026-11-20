package trackerfake

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEntriesNeverHoldACredential sends a credential in each place a
// request can carry one; neither the log nor OnRequest's copy nor an
// unhandled answer repeats it, in any encoding. (A write's answer holds
// what the client wrote, as the tracker's does.)
func TestEntriesNeverHoldACredential(t *testing.T) {
	escapedJira := `jira-token-0001`
	escapedLinear := `lin_api_key0001`
	text := func(s string) string {
		return `{"body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"` + s + `"}]}]}}`
	}
	for _, c := range []struct {
		name   string
		req    call
		secret string
	}{
		{"raw in the body", jiraReq("POST", "/rest/api/3/issue/WEB-1/comment", text(testJiraToken)), testJiraToken},
		{"escaped in the body", jiraReq("POST", "/rest/api/3/issue/WEB-1/comment", text(escapedJira)), testJiraToken},
		{"a body key", jiraReq("POST", "/rest/api/3/issue/WEB-1/comment", `{"`+escapedJira+`":1}`), testJiraToken},
		{"the query", jiraReq("GET", "/rest/api/3/myself?"+testJiraToken+"="+testJiraToken, ""), testJiraToken},
		{"the path", jiraReq("GET", "/rest/api/3/issue/"+testJiraToken, ""), testJiraToken},
		{"raw in the variables", call{origin: linearOrigin, method: "POST", path: "/graphql",
			body: linearBody("Issue", `query Issue($id: String!) { issue(id: $id) { id } }`, map[string]any{"id": testLinearKey})}, testLinearKey},
		{"escaped in the variables", call{origin: linearOrigin, method: "POST", path: "/graphql",
			body: `{"query":"query Issue($id: String!) { issue(id: $id) { id } }","operationName":"Issue","variables":{"id":"` + escapedLinear + `"}}`}, testLinearKey},
		{"a variable key", call{origin: linearOrigin, method: "POST", path: "/graphql",
			body: `{"query":"query Viewer { viewer { id } }","operationName":"Viewer","variables":{"` + escapedLinear + `":1}}`}, testLinearKey},
	} {
		t.Run(c.name, func(t *testing.T) {
			var notified []Request
			g := newRig(t, testFixture())
			g.e.opts.OnRequest = func(r Request) { notified = append(notified, r) }
			rec := g.do(c.req)
			logged := g.e.Requests(0).Requests
			if len(logged) != 1 || len(notified) != 1 {
				t.Fatalf("logged %d, notified %d", len(logged), len(notified))
			}
			for what, r := range map[string]Request{"log": logged[0], "OnRequest": notified[0]} {
				raw, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				var decoded any
				if err := json.Unmarshal(raw, &decoded); err != nil {
					t.Fatal(err)
				}
				plain, err := json.Marshal(decoded)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(unescape(t, string(plain)), c.secret) || !strings.Contains(string(raw), "redacted") {
					t.Errorf("the %s entry holds the credential: %s", what, raw)
				}
			}
			if rec.Code == 501 && strings.Contains(unescape(t, rec.Body.String()), c.secret) {
				t.Errorf("the unhandled answer repeats the credential: %s", rec.Body)
			}
		})
	}
}

// unescape is s with every JSON string decoded, so an escaped credential
// reads as itself.
func unescape(t *testing.T, s string) string {
	t.Helper()
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return s
	}
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			b.WriteString(v + " ")
		case []any:
			for _, x := range v {
				walk(x)
			}
		case map[string]any:
			for k, x := range v {
				b.WriteString(k + " ")
				walk(x)
			}
		}
	}
	walk(v)
	return b.String()
}

// TestEntryBound holds every member at the bound and one byte over: a
// text over it is cut to it, a JSON value over it left out, and Omitted
// says so.
func TestEntryBound(t *testing.T) {
	at := strings.Repeat("x", maxLoggedMember)
	over := at + "y"
	jsonOf := func(n int) json.RawMessage {
		return json.RawMessage(`{"a":"` + strings.Repeat("x", n-len(`{"a":""}`)) + `"}`)
	}
	for _, c := range []struct {
		name string
		set  func(r *Request, s string, j json.RawMessage)
		get  func(r Request) string
		json bool
	}{
		{"path", func(r *Request, s string, _ json.RawMessage) { r.Path = s }, func(r Request) string { return r.Path }, false},
		{"operation", func(r *Request, s string, _ json.RawMessage) { r.Operation = s }, func(r Request) string { return r.Operation }, false},
		{"detail", func(r *Request, s string, _ json.RawMessage) { r.Detail = s }, func(r Request) string { return r.Detail }, false},
		{"query key", func(r *Request, s string, _ json.RawMessage) { r.Query = map[string][]string{s: {"v"}} }, func(r Request) string {
			for k := range r.Query {
				return k
			}
			return ""
		}, false},
		{"query value", func(r *Request, s string, _ json.RawMessage) { r.Query = map[string][]string{"k": {s}} }, func(r Request) string { return r.Query.Get("k") }, false},
		{"file name", func(r *Request, s string, _ json.RawMessage) { r.Files = []string{s} }, func(r Request) string { return r.Files[0] }, false},
		{"body", func(r *Request, _ string, j json.RawMessage) { r.Body = j }, func(r Request) string { return string(r.Body) }, true},
		{"variables", func(r *Request, _ string, j json.RawMessage) { r.Variables = j }, func(r Request) string { return string(r.Variables) }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var in Request
			c.set(&in, at, jsonOf(maxLoggedMember))
			if got := entryOf(in, nil); len(c.get(got)) != maxLoggedMember || len(got.Omitted) != 0 {
				t.Fatalf("at the bound: kept %d bytes, omitted %v", len(c.get(got)), got.Omitted)
			}
			c.set(&in, over, jsonOf(maxLoggedMember+1))
			got := entryOf(in, nil)
			want := maxLoggedMember
			if c.json {
				want = 0
			}
			if len(c.get(got)) != want || len(got.Omitted) != 1 {
				t.Fatalf("over the bound: kept %d bytes, omitted %v", len(c.get(got)), got.Omitted)
			}
		})
	}
}

// TestLoggedVariablesAreBounded: a Linear request's variables over the
// bound are answered and left out of its entry.
func TestLoggedVariablesAreBounded(t *testing.T) {
	g := newRig(t, testFixture())
	rec := g.do(call{origin: linearOrigin, method: "POST", path: "/graphql",
		body: linearBody("Issue", `query Issue($id: String!) { issue(id: $id) { id } }`, map[string]any{"id": strings.Repeat("x", maxLoggedMember)})})
	if rec.Code != 200 {
		t.Fatalf("answered %d %s", rec.Code, rec.Body)
	}
	r := g.e.Requests(0).Requests[0]
	if len(r.Variables) != 0 || len(r.Omitted) != 1 || !strings.HasPrefix(r.Omitted[0], "variables:") {
		t.Fatalf("logged %d bytes of variables, omitted %v", len(r.Variables), r.Omitted)
	}
}
