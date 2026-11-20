package trackerfake

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/lexer"
)

// bs is one backslash. Escapes in these tests are built from it at run
// time, so no editor or tool decodes them while the file is written.
const bs = "\x5c"

// escapeTail is s with its last n characters written as \uXXXX escapes.
func escapeTail(s string, n int) string {
	var b strings.Builder
	b.WriteString(s[:len(s)-n])
	for _, r := range s[len(s)-n:] {
		fmt.Fprintf(&b, "%su%04x", bs, r)
	}
	return b.String()
}

// escapeForms decode as defined by the JSON and GraphQL string grammars,
// written independently of the code under test.
var escapeForms = regexp.MustCompile(regexp.QuoteMeta(bs) + `(u\{[0-9a-fA-F]+\}|u[dD][89abAB][0-9a-fA-F]{2}` + regexp.QuoteMeta(bs) + `u[dD][c-fC-F][0-9a-fA-F]{2}|u[0-9a-fA-F]{4}|["/bfnrt]|` + regexp.QuoteMeta(bs) + `)`)

// decodeEscapes is s with every JSON or GraphQL string escape decoded.
func decodeEscapes(s string) string {
	return escapeForms.ReplaceAllStringFunc(s, func(m string) string {
		e := m[1:]
		switch {
		case strings.HasPrefix(e, "u{"):
			v, _ := strconv.ParseUint(e[2:len(e)-1], 16, 32)
			return string(rune(v))
		case len(e) == 11:
			hi, _ := strconv.ParseUint(e[1:5], 16, 16)
			lo, _ := strconv.ParseUint(e[7:], 16, 16)
			return string(utf16.DecodeRune(rune(hi), rune(lo)))
		case e[0] == 'u':
			v, _ := strconv.ParseUint(e[1:], 16, 16)
			return string(rune(v))
		}
		return map[byte]string{'"': `"`, '/': "/", 'b': "\b", 'f': "\f", 'n': "\n", 'r': "\r", 't': "\t", bs[0]: bs}[e[0]]
	})
}

// holds reports whether s, read with its JSON strings decoded and then
// the escapes in them decoded again and again, holds secret.
func holds(t *testing.T, s, secret string) bool {
	t.Helper()
	return strings.Contains(decodeAll(unescape(t, s)), secret)
}

// decodeAll is s with its escapes decoded until none is left.
func decodeAll(s string) string {
	for {
		next := decodeEscapes(s)
		if next == s {
			return s
		}
		s = next
	}
}

// linearDoc is a Linear request body with no variables, so its entry
// keeps the document.
func linearDoc(op, query string) call {
	b, err := json.Marshal(map[string]any{"query": query, "operationName": op})
	if err != nil {
		panic(err)
	}
	return call{origin: linearOrigin, method: "POST", path: "/graphql", body: string(b)}
}

// TestEntriesNeverHoldACredential sends a credential in each place a
// request can carry one, plainly and escaped; neither the log nor
// OnRequest's copy nor an unhandled answer holds it, read with every
// JSON or GraphQL escape decoded. (A write's answer holds what the client
// wrote, as the tracker's does.)
func TestEntriesNeverHoldACredential(t *testing.T) {
	jsonEscaped := escapeTail(testJiraToken, 4)
	gqlEscaped := escapeTail(testLinearKey, 4)
	if jsonEscaped == testJiraToken || !strings.Contains(jsonEscaped, bs+"u0030") || decodeEscapes(gqlEscaped) != testLinearKey {
		t.Fatalf("the escapes did not build: %q %q", jsonEscaped, gqlEscaped)
	}
	braced := testLinearKey[:len(testLinearKey)-1] + bs + "u{31}"
	text := func(s string) string {
		return `{"body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"` + s + `"}]}]}}`
	}
	issue := func(literal string) string { return `query Q { issue(id: ` + literal + `) { id } }` }
	for _, c := range []struct {
		name   string
		req    call
		secret string
	}{
		// A document that does not lex is left out, the omission named.
		{"raw in the body", jiraReq("POST", "/rest/api/3/issue/WEB-1/comment", text(testJiraToken)), testJiraToken},
		{"JSON-escaped in the body", jiraReq("POST", "/rest/api/3/issue/WEB-1/comment", text(jsonEscaped)), testJiraToken},
		{"a JSON-escaped body key", jiraReq("POST", "/rest/api/3/issue/WEB-1/comment", `{"`+jsonEscaped+`":1}`), testJiraToken},
		{"the query", jiraReq("GET", "/rest/api/3/myself?"+testJiraToken+"="+testJiraToken, ""), testJiraToken},
		{"the path", jiraReq("GET", "/rest/api/3/issue/"+testJiraToken, ""), testJiraToken},
		{"the path, percent-encoded", jiraReq("GET", "/rest/api/3/issue/jira-token-%30%30%30%31", ""), testJiraToken},
		{"the path of an unhandled request", jiraReq("GET", "/rest/api/3/none/jira-token-%30%30%30%31", ""), testJiraToken},
		{"raw in the variables", call{origin: linearOrigin, method: "POST", path: "/graphql",
			body: linearBody("Issue", `query Issue($id: String!) { issue(id: $id) { id } }`, map[string]any{"id": testLinearKey})}, testLinearKey},
		{"JSON-escaped in the variables", call{origin: linearOrigin, method: "POST", path: "/graphql",
			body: `{"query":"query Issue($id: String!) { issue(id: $id) { id } }","operationName":"Issue","variables":{"id":"` + gqlEscaped + `"}}`}, testLinearKey},
		{"a JSON-escaped variable key", call{origin: linearOrigin, method: "POST", path: "/graphql",
			body: `{"query":"query Viewer { viewer { id } }","operationName":"Viewer","variables":{"` + gqlEscaped + `":1}}`}, testLinearKey},
		{"raw in a GraphQL string", linearDoc("Q", issue(`"`+testLinearKey+`"`)), testLinearKey},
		{"escaped in a GraphQL string", linearDoc("Q", issue(`"`+gqlEscaped+`"`)), testLinearKey},
		{"brace-escaped in a GraphQL string", linearDoc("Q", issue(`"`+braced+`"`)), testLinearKey},
		{"in a GraphQL block string", linearDoc("Q", issue(`"""`+testLinearKey+`"""`)), testLinearKey},
		{"escaped in a GraphQL comment", linearDoc("Q", issue(`"i1"`)+" # "+gqlEscaped), testLinearKey},
		{"escaped in a document that does not lex", linearDoc("Q", issue(`"`+gqlEscaped)), testLinearKey},
		{"escaped in the operation name", linearDoc(gqlEscaped, issue(`"i1"`)), testLinearKey},
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
				if holds(t, string(raw), c.secret) || !strings.Contains(string(raw), "redacted") && len(r.Omitted) == 0 {
					t.Errorf("the %s entry holds the credential: %s", what, raw)
				}
			}
			if strings.Contains(c.name, "does not lex") && !slices.ContainsFunc(logged[0].Omitted, func(s string) bool { return strings.HasPrefix(s, "body query:") }) {
				t.Errorf("the document was logged, omitted %v: %s", logged[0].Omitted, logged[0].Body)
			}
			if rec.Code == 501 && holds(t, rec.Body.String(), c.secret) {
				t.Errorf("the unhandled answer holds the credential: %s", rec.Body)
			}
		})
	}
}

// unescape is s with every JSON string decoded, so a JSON-escaped
// credential reads as itself.
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

// TestScrubTextReadsEveryEscape: each escape form the JSON and GraphQL
// string grammars define, in any character of a secret, is read as that
// character.
func TestScrubTextReadsEveryEscape(t *testing.T) {
	secret := "key/0\"1\\2\t3\U0001F600z"
	forms := map[string]func(r rune) string{
		"plain":    func(r rune) string { return string(r) },
		"lower":    func(r rune) string { return unit(r, "%04x") },
		"upper":    func(r rune) string { return unit(r, "%04X") },
		"braced":   func(r rune) string { return fmt.Sprintf("%su{%x}", bs, r) },
		"one-char": shortEscape,
		// Quoted again, as %q of an escaped text: each backslash and quote
		// escaped.
		"lower, quoted again":    func(r rune) string { return requote(unit(r, "%04x")) },
		"braced, quoted again":   func(r rune) string { return requote(fmt.Sprintf("%su{%x}", bs, r)) },
		"one-char, quoted again": func(r rune) string { return requote(shortEscape(r)) },
	}
	for name, form := range forms {
		t.Run(name, func(t *testing.T) {
			for i, r := range []rune(secret) {
				spelled := []rune(secret)
				written := string(spelled[:i]) + form(r) + string(spelled[i+1:])
				if decodeAll(form(r)) != string(r) {
					t.Fatalf("the test's own spelling %q does not decode to %q", form(r), r)
				}
				if got := scrubText("a "+written+" b", []string{secret}); got != "a "+redacted+" b" {
					t.Errorf("%q scrubbed to %q", written, got)
				}
			}
		})
	}
	if got := scrubText(bs+"u0030 "+bs+"/x", []string{"long-secret"}); got != bs+"u0030 "+bs+"/x" {
		t.Errorf("text with no secret changed: %q", got)
	}
}

// unit is r as \u escapes, a surrogate pair above the BMP.
func unit(r rune, format string) string {
	if r > 0xFFFF {
		hi, lo := utf16.EncodeRune(r)
		return bs + "u" + fmt.Sprintf(format, hi) + bs + "u" + fmt.Sprintf(format, lo)
	}
	return bs + "u" + fmt.Sprintf(format, r)
}

func requote(s string) string {
	return strings.NewReplacer(bs, bs+bs, `"`, bs+`"`).Replace(s)
}

func shortEscape(r rune) string {
	if e, ok := map[rune]string{'"': `"`, '/': "/", '\\': bs, '\b': "b", '\f': "f", '\n': "n", '\r': "r", '\t': "t"}[r]; ok {
		return bs + e
	}
	return string(r)
}

// TestScrubDocument: a document's string literals are judged by the
// value the lexer reads and written again so the document still lexes;
// the rest is scrubbed as text, across tokens too; a document whose
// literal spans do not lex back is left out.
func TestScrubDocument(t *testing.T) {
	secrets := []string{testLinearKey, "ab=cdefgh", `xy"zwvuts`}
	escaped := escapeTail(testLinearKey, 4)
	for _, c := range []struct {
		name, doc string
		ok        bool
		want      string
	}{
		{"an escaped literal", `{ issue(id: "x` + escaped + `") { id } }`, true, `{ issue(id: "x` + redacted + `") { id } }`},
		{"a block string", `{ issue(id: """` + testLinearKey + `""") { id } }`, true, `{ issue(id: "` + redacted + `") { id } }`},
		{"across tokens", `{ a(b: ab=cdefgh) }`, true, `{ a(b: ` + redacted + `) }`},
		{"across a literal's edge", `{ a(b: xy"zwvuts") }`, true, `{ a(b: ` + redacted + `") }`},
		{"a comment", "{ a } # " + escaped, true, "{ a } # " + redacted},
		{"nothing to replace", `{ issue(id: "` + bs + `u0030") { id } } # ` + bs + `n`, true, `{ issue(id: "` + bs + `u0030") { id } } # ` + bs + `n`},
		{"a literal whose span is short", `{ a(b: """` + testLinearKey + `"""") }`, false, ""},
		{"no lex", `{ a(b: "` + escaped + ` }`, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := scrubDocument(c.doc, secrets)
			if ok != c.ok || got != c.want {
				t.Fatalf("scrubbed to %q, %v; want %q, %v", got, ok, c.want, c.ok)
			}
			if ok && !strings.HasPrefix(c.name, "across") && !lexes(got) {
				t.Errorf("the scrubbed document does not lex: %q", got)
			}
		})
	}
}

func lexes(doc string) bool {
	lex := lexer.New(&ast.Source{Input: doc})
	for {
		tok, err := lex.ReadToken()
		if err != nil {
			return false
		}
		if tok.Kind == lexer.EOF {
			return true
		}
	}
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
