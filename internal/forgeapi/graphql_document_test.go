package forgeapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The documents this package's tests send.
var (
	testViewerDocument             = NewDocument(ForgeGitHub, "query Viewer { viewer { login } }")
	testRepoDocument               = NewDocument(ForgeGitHub, "query Repo($owner: String!) { repository(owner: $owner) { id } }")
	testProjectDocument            = NewDocument(ForgeGitLab, "query Project($fullPath: ID!) { project(fullPath: $fullPath) { id } }")
	testUpdateIssueCommentDocument = NewDocument(ForgeGitHub, "mutation UpdateIssueComment { updateIssueComment { id } }")
	testMutationDocument           = NewDocument(ForgeGitHub, "mutation M { m { id } }")
)

// A document's operation is the one named operation it defines, however
// the text opens; text without exactly one is refused.
func TestDocumentOperation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ text, op, err string }{
		{text: "query A { a }", op: "A"},
		{text: "# a comment\nquery A($x: In = {k: \"}\"}) @d { a(x: $x) { b } }", op: "A"},
		{text: "fragment F on T { a { b } }\nmutation M { m { ...F } }", op: "M"},
		{text: `"""described""" query A { a(s: "{") }`, op: "A"},
		{text: "{ a }", err: "no name"},
		{text: "query { a }", err: "no name"},
		{text: "query A { a } query B { b }", err: "2 operations"},
		{text: "fragment F on T { a }", err: "0 operations"},
		{text: "query A { a", err: "not closed"},
		{text: "type T { a: Int }", err: `starts with "type"`},
	} {
		op, err := documentOperation(tc.text)
		if op != tc.op || (tc.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("%q: %q, %v; want %q, %q", tc.text, op, err, tc.op, tc.err)
		}
	}
}

// NewDocument refuses a forge it does not know, text without one named
// operation and a second document of an operation, and registers the
// rest.
func TestNewDocument(t *testing.T) {
	t.Parallel()
	panics := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: no panic", name)
			}
		}()
		f()
	}
	panics("another forge", func() { NewDocument("bitbucket", "query Unique1 { a }") })
	panics("no operation name", func() { NewDocument(ForgeGitHub, "{ a }") })
	panics("an operation made twice", func() { NewDocument(ForgeGitHub, "query Viewer { viewer { id } }") })
	if d := testProjectDocument; d.Forge() != ForgeGitLab || d.Operation() != "Project" || !strings.HasPrefix(d.Text(), "query Project") {
		t.Errorf("document = %+v", d)
	}
	var registered int
	for _, d := range Documents() {
		if d == testViewerDocument || d == testProjectDocument {
			registered++
		}
	}
	if registered != 2 {
		t.Errorf("Documents() holds %d of the two", registered)
	}
}

// A document NewDocument did not make, or one of the other forge, is
// refused before anything is sent.
func TestGraphQLRefusesForeignDocuments(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	s := isolatedService(t, srv)
	if _, err := s.GitHub("github.com").GraphQL(t.Context(), Document{}, nil, nil); err == nil || !strings.Contains(err.Error(), "NewDocument") {
		t.Errorf("a zero document: %v", err)
	}
	if _, err := s.GitHub("github.com").GraphQL(t.Context(), testProjectDocument, nil, nil); err == nil || !strings.Contains(err.Error(), "gitlab GraphQL document sent to github") {
		t.Errorf("a GitLab document to GitHub: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("a refused document reached the server %d times", hits.Load())
	}
}
