package forgeapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewRefusesUnsafeConstruction(t *testing.T) {
	t.Parallel()
	real := &testSource{tokens: []string{testSecret}}
	for name, opts := range map[string]Options{
		"isolated base with a real token source": {Version: "v", Isolated: &Isolated{BaseURL: "http://[::1]:1", Token: "fixed"}, TokenSource: real},
		"isolated without a fake":                {Version: "v", Isolated: &Isolated{}},
		"isolated base that is not http":         {Version: "v", Isolated: &Isolated{BaseURL: "ftp://x", Token: "fixed"}},
		"no token source":                        {Version: "v"},
		"no version":                             {TokenSource: real},
	} {
		if s, err := New(opts); err == nil || s != nil {
			t.Errorf("%s: New = %v, %v; want a construction error", name, s, err)
		}
	}
	if real.readCount() != 0 {
		t.Fatalf("a refused construction read %d tokens", real.readCount())
	}
	live, err := New(Options{Version: "v", TokenSource: real})
	if err != nil || live.Isolated() {
		t.Fatalf("production New = %v isolated %v", err, live != nil && live.Isolated())
	}
	live.Close()
}

func TestGitLabBaseFromGlabConfig(t *testing.T) {
	t.Parallel()
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	apiHost := strings.TrimPrefix(srv.URL, "http://")
	for _, tc := range []struct {
		info   SourceInfo
		header string
		want   string
	}{
		{SourceInfo{Header: AuthPrivateToken, APIHost: apiHost, APIProtocol: "http"}, "Private-Token", testSecret},
		{SourceInfo{Header: AuthBearer, APIHost: apiHost, APIProtocol: "http"}, "Authorization", "Bearer " + testSecret},
	} {
		s, err := New(Options{Version: "test", TokenSource: &testSource{tokens: []string{testSecret}, info: tc.info}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.GitLab("gl.example").Do(t.Context(), Request{Path: "version"}); err != nil {
			t.Fatal(err)
		}
		s.Close()
		req, _ := got.last()
		if req.URL.Path != "/api/v4/version" || req.Header.Get(tc.header) != tc.want {
			t.Fatalf("request %s headers %v", req.URL.Path, req.Header)
		}
	}
	s, err := New(Options{Version: "test", TokenSource: &testSource{tokens: []string{testSecret}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rest, gql, err := s.defaultBase(ForgeGitHub, "github.com", SourceInfo{})
	if err != nil || rest.String() != "https://api.github.com/" || gql.String() != "https://api.github.com/graphql" {
		t.Fatalf("github.com bases = %v %v", rest, gql)
	}
	rest, gql, _ = s.defaultBase(ForgeGitHub, "ghe.example:8443", SourceInfo{})
	if rest.String() != "https://ghe.example:8443/api/v3/" || gql.String() != "https://ghe.example:8443/api/graphql" {
		t.Fatalf("GHE bases = %v %v", rest, gql)
	}
	rest, _, _ = s.defaultBase(ForgeGitLab, "gl.example:8443", SourceInfo{})
	if rest.String() != "https://gl.example:8443/api/v4/" {
		t.Fatalf("GitLab default base = %v", rest)
	}
	if _, _, err := s.defaultBase(ForgeGitLab, "gl.example", SourceInfo{APIProtocol: "ftp"}); err == nil {
		t.Fatal("an ftp api_protocol was accepted")
	}
}
