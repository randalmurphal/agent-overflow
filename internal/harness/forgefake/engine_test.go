package forgefake

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/harness/control"
)

const testFixture = `{
  "viewer": "octo",
  "repos": [
    {
      "forge": "github",
      "project": "acme/widgets",
      "attachments": [{"url": "https://github.com/user-attachments/assets/abc", "contentType": "image/png", "base64": "iVBORw0K"}],
      "pulls": [{
        "number": 7,
        "title": "Add widgets",
        "body": "see ![shot](https://github.com/user-attachments/assets/abc)",
        "author": "alice",
        "diff": "diff --git a/w.go b/w.go\n--- a/w.go\n+++ b/w.go\n@@ -1,2 +1,2 @@\n-old\n+new\n+more\n",
        "comments": [{"body": "looks good"}],
        "threads": [{"path": "w.go", "line": 2, "comments": [{"body": "nit"}, {"body": "fixed", "author": "alice"}]}],
        "reviews": [{"author": "bob", "state": "APPROVED"}],
        "ci": {"jobs": [{"name": "test", "status": "success", "log": "ok"}]}
      }]
    },
    {
      "forge": "gitlab",
      "project": "grp/sub/tool",
      "attachments": [{"secret": "0123456789abcdef0123456789abcdef", "filename": "diagram.svg", "contentType": "image/svg+xml", "text": "<svg/>"}],
      "pulls": [{"number": 3, "title": "Fix tool", "reviews": [{"author": "carol", "state": "APPROVED"}]}]
    }
  ]
}`

// seedJSON decodes a fixture the way the harness wire does (strictly)
// and seeds it.
func seedJSON(e *Engine, raw string) (Fixture, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var fixture Fixture
	if err := decoder.Decode(&fixture); err != nil {
		return Fixture{}, err
	}
	if decoder.More() {
		return Fixture{}, errors.New("trailing document")
	}
	return e.Seed(fixture)
}

func seeded(t *testing.T, opts Options) (*Engine, Fixture) {
	t.Helper()
	e := New(opts)
	fixture, err := seedJSON(e, testFixture)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	return e, fixture
}

func handle(e *Engine, cli string, args ...string) control.ForgeResult {
	return e.Handle(control.ForgeCall{CLI: cli, Args: args, Cwd: "/work"})
}

func decode[T any](t *testing.T, result control.ForgeResult) T {
	t.Helper()
	if result.ExitCode != 0 {
		t.Fatalf("exit %d: %s", result.ExitCode, result.Stderr)
	}
	var out T
	if err := json.Unmarshal(result.Stdout, &out); err != nil {
		t.Fatalf("decode %q: %v", result.Stdout, err)
	}
	return out
}

func TestSeedFillsGeneratedIDsAndDefaults(t *testing.T) {
	_, fixture := seeded(t, Options{})
	gh, gl := fixture.Repos[0], fixture.Repos[1]
	if gh.Host != "github.com" || gl.Host != "gitlab.com" || gl.ID == 0 {
		t.Fatalf("hosts/ids not defaulted: %+v %+v", gh, gl)
	}
	pull := gh.Pulls[0]
	if pull.State != "open" || pull.HeadRef != "feature" || pull.BaseRef != "main" || len(pull.HeadSHA) != 40 {
		t.Fatalf("pull defaults: %+v", pull)
	}
	if pull.Comments[0].ID == 0 || pull.Comments[0].Author != "alice" || pull.Threads[0].ID == "" || pull.Threads[0].Side != "right" {
		t.Fatalf("comment/thread defaults: %+v %+v", pull.Comments[0], pull.Threads[0])
	}
	if pull.CI.ID == 0 || pull.CI.Status != "success" || pull.CI.Jobs[0].ID == 0 || pull.CI.Jobs[0].StartedAt == "" {
		t.Fatalf("ci defaults: %+v", pull.CI)
	}
	if fixture.Viewer != "octo" {
		t.Fatalf("viewer = %q", fixture.Viewer)
	}
}

func TestSeedRejectsMalformedFixtures(t *testing.T) {
	cases := map[string]string{
		"no repos":           `{"repos":[]}`,
		"bad forge":          `{"repos":[{"forge":"bitbucket","project":"a/b"}]}`,
		"github project":     `{"repos":[{"forge":"github","project":"a/b/c"}]}`,
		"duplicate repo":     `{"repos":[{"forge":"github","project":"a/b"},{"forge":"github","project":"A/B"}]}`,
		"duplicate pull":     `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x"},{"number":1,"title":"y"}]}]}`,
		"line thread":        `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","threads":[{"path":"f","comments":[{"body":"b"}]}]}]}]}`,
		"gitlab verdict":     `{"repos":[{"forge":"gitlab","project":"g/r","pulls":[{"number":1,"title":"x","reviews":[{"author":"a","state":"CHANGES_REQUESTED"}]}]}]}`,
		"job status":         `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","ci":{"jobs":[{"name":"j","status":"green"}]}}]}]}`,
		"gitlab upload key":  `{"repos":[{"forge":"gitlab","project":"g/r","attachments":[{"secret":"nothex","filename":"a.png","text":"x"}]}]}`,
		"github upload key":  `{"repos":[{"forge":"github","project":"a/b","attachments":[{"secret":"0123456789abcdef0123456789abcdef","filename":"a","text":"x"}]}]}`,
		"two contents":       `{"repos":[{"forge":"github","project":"a/b","attachments":[{"url":"https://github.com/x","text":"x","base64":"eA=="}]}]}`,
		"padded project":     `{"repos":[{"forge":"github","project":"a/ b"}]}`,
		"negative pr number": `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":-1,"title":"x"}]}]}`,
	}
	for name, raw := range cases {
		if _, err := seedJSON(New(Options{}), raw); err == nil {
			t.Errorf("%s: seeded without error", name)
		}
	}
}

func TestSeedReplacesTheSameRepoAndRefusesAnIDCollision(t *testing.T) {
	e, fixture := seeded(t, Options{})
	if _, err := seedJSON(e, `{"repos":[{"forge":"github","project":"ACME/widgets","pulls":[{"number":9,"title":"Only"}]}]}`); err != nil {
		t.Fatal(err)
	}
	if res := handle(e, "gh", "pr", "view", "--repo", "acme/widgets", "7", "--json", "title"); res.ExitCode == 0 {
		t.Fatal("the replaced repo still answers #7")
	}
	if got := decode[map[string]string](t, handle(e, "gh", "pr", "view", "--repo", "acme/widgets", "9", "--json", "title")); got["title"] != "Only" {
		t.Fatalf("title = %q", got["title"])
	}
	collide := `{"repos":[{"forge":"gitlab","project":"other/repo","id":` + jsonInt(fixture.Repos[1].ID) + `}]}`
	if _, err := seedJSON(e, collide); err == nil {
		t.Fatal("a second GitLab project took an id already in use")
	}
}

func jsonInt(n int64) string {
	out, _ := json.Marshal(n)
	return string(out)
}

func TestUnknownInvocationsFailLoudlyWithTheFullArgv(t *testing.T) {
	e, _ := seeded(t, Options{})
	cases := [][]string{
		{"gh", "pr", "create", "--title", "t", "--body", "b"},
		{"gh", "pr", "view", "--repo", "acme/widgets", "7", "--json", "title,labels"},
		{"gh", "pr", "view", "--repo", "acme/widgets", "7", "--web"},
		{"gh", "api", "repos/acme/widgets/pulls/7/reviews", "-X", "POST", "--input", "-"},
		{"gh", "api", "graphql", "-f", "query=mutation { thread: resolveReviewThread(input: {threadId: \"X\"}) { thread { isResolved } } }"},
		{"gh", "api", "user", "--jq", ".login | ascii_downcase"},
		{"gh", "api", "user", "--paginate"},
		{"glab", "mr", "create", "--title", "t"},
		{"glab", "api", "projects/grp%2Fsub%2Ftool/merge_requests/3/discussions?per_page=50&page=1&sort=asc", "--include"},
		{"glab", "api", "projects/grp%2Fsub%2Ftool/merge_requests/3/approve", "-X", "POST", "-f", "sha=abc"},
		{"hub", "pr", "list"},
	}
	for _, argv := range cases {
		result := handle(e, argv[0], argv[1:]...)
		if result.ExitCode == 0 || len(result.Stdout) != 0 {
			t.Errorf("%q answered exit %d stdout %q", argv, result.ExitCode, result.Stdout)
			continue
		}
		if !strings.Contains(result.Stderr, "unhandled") || !strings.Contains(result.Stderr, formatArgv(argv[0], argv[1:])) {
			t.Errorf("%q stderr lacks the argv: %q", argv, result.Stderr)
		}
	}
	log := e.Invocations(0)
	if len(log.Invocations) != len(cases) {
		t.Fatalf("recorded %d, want %d", len(log.Invocations), len(cases))
	}
	for _, inv := range log.Invocations {
		if !inv.Unhandled || inv.Route != "" {
			t.Errorf("invocation %q recorded as handled (%q)", inv.Args, inv.Route)
		}
	}
}

func TestMissingResourcesAreForgeErrorsNotUnhandled(t *testing.T) {
	e, _ := seeded(t, Options{})
	cases := [][]string{
		{"gh", "pr", "view", "--repo", "acme/widgets", "99", "--json", "title"},
		{"gh", "pr", "view", "--repo", "acme/nothing", "7", "--json", "title"},
		{"gh", "api", "https://github.com/user-attachments/assets/missing", "-H", "Accept: */*", "--allow-escape-sequences"},
		{"glab", "api", "projects/grp%2Fsub%2Ftool/merge_requests/99"},
		{"glab", "api", "projects/grp%2Fsub%2Ftool/uploads/0123456789abcdef0123456789abcdef/other.svg"},
	}
	for _, argv := range cases {
		result := handle(e, argv[0], argv[1:]...)
		if result.ExitCode != 1 || strings.Contains(result.Stderr, "unhandled") {
			t.Errorf("%q: exit %d stderr %q, want a forge not-found error", argv, result.ExitCode, result.Stderr)
		}
	}
	for _, inv := range e.Invocations(0).Invocations {
		if inv.Unhandled || inv.Route == "" {
			t.Errorf("%q recorded as unhandled", inv.Args)
		}
	}
}

func TestInvocationsAreRecordedAndObserved(t *testing.T) {
	var mu sync.Mutex
	var observed []Invocation
	e, _ := seeded(t, Options{OnInvocation: func(inv Invocation) {
		mu.Lock()
		observed = append(observed, inv)
		mu.Unlock()
	}})
	e.Handle(control.ForgeCall{CLI: "gh", Args: []string{"api", "user", "--jq", ".login"}, Cwd: "/a", Stdin: []byte("in")})
	e.Handle(control.ForgeCall{CLI: "glab", Args: []string{"mr", "diff", "3", "-R", "grp/sub/tool"}, Cwd: "/b"})

	log := e.Invocations(0)
	if len(log.Invocations) != 2 || len(observed) != 2 {
		t.Fatalf("recorded %d, observed %d", len(log.Invocations), len(observed))
	}
	first := log.Invocations[0]
	if first.Seq != 1 || first.CLI != "gh" || strings.Join(first.Args, " ") != "api user --jq .login" ||
		first.Cwd != "/a" || first.Stdin != "in" || first.Route != "gh api user" || first.ExitCode != 0 || first.StdoutBytes != len("octo\n") {
		t.Fatalf("first = %+v", first)
	}
	if second := log.Invocations[1]; second.Route != "glab mr diff" || second.Cwd != "/b" {
		t.Fatalf("second = %+v", second)
	}
	if since := e.Invocations(1); len(since.Invocations) != 1 || since.Invocations[0].Seq != 2 {
		t.Fatalf("since 1 = %+v", since)
	}
	// The returned log is a copy.
	log.Invocations[0].Args[0] = "mutated"
	if e.Invocations(0).Invocations[0].Args[0] != "api" {
		t.Fatal("Invocations returned the engine's own slice")
	}
}

func TestInvocationLogIsBounded(t *testing.T) {
	e, _ := seeded(t, Options{})
	for range maxInvocations + 5 {
		handle(e, "gh", "api", "user", "--jq", ".login")
	}
	log := e.Invocations(0)
	if len(log.Invocations) != maxInvocations || log.Dropped != 5 || log.Invocations[0].Seq != 6 {
		t.Fatalf("len %d dropped %d first seq %d", len(log.Invocations), log.Dropped, log.Invocations[0].Seq)
	}
}

func TestResetClearsStateAndLogButNotIDs(t *testing.T) {
	e, fixture := seeded(t, Options{})
	handle(e, "gh", "api", "user", "--jq", ".login")
	e.Reset()
	if log := e.Invocations(0); len(log.Invocations) != 0 || log.Dropped != 0 {
		t.Fatalf("log after reset: %+v", log)
	}
	if res := handle(e, "gh", "pr", "view", "--repo", "acme/widgets", "7", "--json", "title"); res.ExitCode == 0 {
		t.Fatal("seeded repo survived reset")
	}
	if got := string(handle(e, "gh", "api", "user", "--jq", ".login").Stdout); got != defaultViewer+"\n" {
		t.Fatalf("viewer after reset = %q", got)
	}
	again, err := seedJSON(e, testFixture)
	if err != nil {
		t.Fatal(err)
	}
	if again.Repos[0].Pulls[0].Comments[0].ID == fixture.Repos[0].Pulls[0].Comments[0].ID {
		t.Fatal("a generated id repeated across a reset")
	}
	if e.Invocations(0).Invocations[0].Seq != 2 {
		t.Fatal("invocation sequence restarted after reset")
	}
}

func TestPullListInfersTheRepositoryFromTheCheckout(t *testing.T) {
	origins := map[string]string{
		"/gh":      "git@github.com:acme/widgets.git",
		"/gl":      "https://gitlab.com/grp/sub/tool.git",
		"/foreign": "https://example.com/acme/widgets.git",
	}
	e, _ := seeded(t, Options{Origin: func(cwd string) (string, error) {
		if origin, ok := origins[cwd]; ok {
			return origin, nil
		}
		return "", errors.New("not a repository")
	}})
	open := decode[[]map[string]any](t, e.Handle(control.ForgeCall{CLI: "gh", Cwd: "/gh",
		Args: []string{"pr", "list", "--head", "feature", "--state", "open", "--json", "url,number,title,state"}}))
	if len(open) != 1 || open[0]["url"] != "https://github.com/acme/widgets/pull/7" || open[0]["state"] != "OPEN" {
		t.Fatalf("gh open list = %v", open)
	}
	mrs := decode[[]map[string]any](t, e.Handle(control.ForgeCall{CLI: "glab", Cwd: "/gl",
		Args: []string{"api", "projects/:fullpath/merge_requests?state=opened&source_branch=feature&per_page=1&view=simple"}}))
	if len(mrs) != 1 || mrs[0]["web_url"] != "https://gitlab.com/grp/sub/tool/-/merge_requests/3" {
		t.Fatalf("glab open list = %v", mrs)
	}
	for _, cwd := range []string{"/foreign", "/nowhere", ""} {
		res := e.Handle(control.ForgeCall{CLI: "gh", Cwd: cwd, Args: []string{"pr", "list", "--head", "feature", "--state", "open", "--json", "url"}})
		if res.ExitCode == 0 {
			t.Errorf("cwd %q resolved a repository", cwd)
		}
	}
}

func TestParseRemote(t *testing.T) {
	cases := map[string][2]string{
		"https://github.com/acme/widgets.git":        {"github.com", "acme/widgets"},
		"https://user@gitlab.com/grp/sub/tool":       {"gitlab.com", "grp/sub/tool"},
		"ssh://git@gitlab.example.com:2222/g/r.git":  {"gitlab.example.com", "g/r"},
		"git@github.com:acme/widgets.git":            {"github.com", "acme/widgets"},
		"https://github.com/acme/widgets/":           {"github.com", "acme/widgets"},
		"https://token@github.com/acme/widgets.git/": {"github.com", "acme/widgets"},
	}
	for remote, want := range cases {
		host, project, ok := parseRemote(remote)
		if !ok || host != want[0] || project != want[1] {
			t.Errorf("parseRemote(%q) = %q %q %v", remote, host, project, ok)
		}
	}
	for _, bad := range []string{"", "/local/path", "https://github.com/solo"} {
		if _, _, ok := parseRemote(bad); ok {
			t.Errorf("parseRemote(%q) accepted", bad)
		}
	}
}

func TestParseFlags(t *testing.T) {
	defs := []flagDef{{long: "repo", short: "R", value: true}, {long: "include", short: "i"}, {long: "header", short: "H", value: true}}
	flags, positional, err := parseFlags([]string{"a", "-Rx/y", "--include", "-H", "h1", "--header=h2", "--", "-z"}, defs)
	if err != nil {
		t.Fatal(err)
	}
	if flags["repo"][0] != "x/y" || !(len(flags["include"]) == 1) || strings.Join(flags["header"], ",") != "h1,h2" ||
		strings.Join(positional, ",") != "a,-z" {
		t.Fatalf("flags %v positional %v", flags, positional)
	}
	for _, bad := range [][]string{{"--nope"}, {"-R"}, {"--include=yes"}} {
		if _, _, err := parseFlags(bad, defs); err == nil {
			t.Errorf("parseFlags(%q) accepted", bad)
		}
	}
}
