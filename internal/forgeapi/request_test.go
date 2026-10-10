package forgeapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestIsolatedRequestsReachTheFakeBase(t *testing.T) {
	t.Parallel()
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		switch {
		case r.URL.Path == "/github/graphql":
			fmt.Fprint(w, `{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","message":"Could not resolve"}]}`)
		default:
			fmt.Fprint(w, `{"ok":true}`)
		}
	}))
	defer srv.Close()
	s := isolatedService(t, srv)

	var out struct{ OK bool }
	if _, err := s.GitHub("ghe.example:8443").JSON(t.Context(), Request{Path: "repos/o/r/pulls/1", Query: url.Values{"per_page": {"100"}}}, &out); err != nil || !out.OK {
		t.Fatalf("JSON = %+v, %v", out, err)
	}
	req, _ := got.last()
	if req.URL.RequestURI() != "/github/rest/repos/o/r/pulls/1?per_page=100" || req.Host != "ghe.example:8443" {
		t.Fatalf("request = %s Host %s", req.URL.RequestURI(), req.Host)
	}
	if req.Header.Get("Authorization") != "Bearer "+testSecret || req.Header.Get("User-Agent") != "agent-overflow/test" ||
		req.Header.Get("Accept") != "application/vnd.github+json" || req.Header.Get("X-GitHub-Api-Version") == "" {
		t.Fatalf("GitHub headers = %v", req.Header)
	}

	if _, err := s.GitLab("gitlab.com").Do(t.Context(), Request{Path: "projects/group%2Fsub%2Frepo/merge_requests/7"}); err != nil {
		t.Fatal(err)
	}
	req, _ = got.last()
	if req.URL.EscapedPath() != "/gitlab/api/v4/projects/group%2Fsub%2Frepo/merge_requests/7" || req.Host != "gitlab.com" {
		t.Fatalf("GitLab request = %s Host %s", req.URL.EscapedPath(), req.Host)
	}
	if req.Header.Get("Private-Token") != testSecret || req.Header.Get("Authorization") != "" || req.Header.Get("Accept") != "application/json" {
		t.Fatalf("GitLab headers = %v", req.Header)
	}

	var data struct{ Repository *struct{} }
	_, err := s.GitHub("github.com").GraphQL(t.Context(), "Repo", "query Repo($owner: String!) { repository(owner: $owner) { id } }", map[string]any{"owner": "o"}, &data)
	var gql *GraphQLError
	if !errors.As(err, &gql) || gql.Problems[0].Type != "NOT_FOUND" {
		t.Fatalf("GraphQL err = %v", err)
	}
	req, body := got.last()
	var payload struct {
		Query         string
		OperationName string
		Variables     map[string]any
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Variables["owner"] != "o" || strings.Contains(payload.Query, `"o"`) || payload.OperationName != "Repo" {
		t.Fatalf("GraphQL payload = %s (%v)", body, err)
	}
	if req.Method != http.MethodPost || req.URL.Path != "/github/graphql" || req.Host != "github.com" {
		t.Fatalf("GraphQL request = %s %s Host %s", req.Method, req.URL.Path, req.Host)
	}

	if _, err := s.GitHub("github.com").Do(t.Context(), Request{Path: "https://github.com/user-attachments/assets/abc?jwt=signed", Attachment: true}); err != nil {
		t.Fatal(err)
	}
	req, _ = got.last()
	if req.URL.RequestURI() != "/github/absolute/github.com/user-attachments/assets/abc?jwt=signed" || req.Header.Get("Authorization") == "" {
		t.Fatalf("attachment request = %s auth %q", req.URL.RequestURI(), req.Header.Get("Authorization"))
	}
}

func TestRequestRefusals(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	s := isolatedService(t, srv)
	gh := s.GitHub("github.com")
	for _, tc := range []struct {
		name string
		r    Request
		want error
	}{
		{"absolute URL without Attachment", Request{Path: "https://evil.example/x"}, ErrAbsoluteURL},
		{"scheme-relative URL", Request{Path: "//evil.example/x"}, ErrAbsoluteURL},
		{"http attachment", Request{Path: "http://github.com/x", Attachment: true}, nil},
		{"leading slash", Request{Path: "/repos/o/r"}, nil},
		{"dot segments", Request{Path: "repos/../../etc"}, nil},
		{"caller Authorization", Request{Path: "user", Header: http.Header{"Authorization": {"Bearer x"}}}, nil},
		{"caller Private-Token", Request{Path: "user", Header: http.Header{"private-token": {"x"}}}, nil},
	} {
		_, err := gh.Do(t.Context(), tc.r)
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: err = %v, want a refusal (%v)", tc.name, err, tc.want)
		}
	}
	if _, err := s.GitHub("bad host/").Do(t.Context(), Request{Path: "user"}); err == nil {
		t.Error("an invalid host was accepted")
	}
	if hits.Load() != 0 {
		t.Fatalf("a refused request reached the server %d times", hits.Load())
	}
}
