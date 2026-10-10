package forgefake

import (
	"encoding/json"
	"errors"
	"net/url"
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
		"github review name": `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","reviews":[{"author":"a","authorName":"A","state":"APPROVED"}]}]}]}`,
		"name without login": `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","comments":[{"authorName":"A","body":"b"}]}]}]}`,
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
		{"gh", "pr", "create", "--title", "t", "--body", "b", "--web"},
		{"gh", "pr", "view", "--repo", "acme/widgets", "7", "--json", "title,labels"},
		{"gh", "pr", "view", "--repo", "acme/widgets", "7", "--web"},
		{"gh", "api", "repos/acme/widgets/pulls/7/reviews", "-X", "POST", "--input", "-"},
		{"gh", "api", "graphql", "-f", "query=mutation { thread: resolveReviewThread(input: {threadId: \"X\"}) { thread { isResolved } } }"},
		{"gh", "api", "user", "--jq", ".login | ascii_downcase"},
		{"gh", "api", "user", "--paginate"},
		{"gh", "run", "view", "1", "--repo", "acme/widgets", "--json", "jobs"},
		{"gh", "api", "repos/acme/widgets/actions/runs/1/jobs?per_page=100&filter=all"},
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
	e.Handle(control.ForgeCall{CLI: "glab", Args: []string{"api", "projects/grp%2Fsub%2Ftool/merge_requests/3"}, Cwd: "/b"})

	log := e.Invocations(0)
	if len(log.Invocations) != 2 || len(observed) != 2 {
		t.Fatalf("recorded %d, observed %d", len(log.Invocations), len(observed))
	}
	first := log.Invocations[0]
	if first.Seq != 1 || first.CLI != "gh" || strings.Join(first.Args, " ") != "api user --jq .login" ||
		first.Cwd != "/a" || first.Stdin != "in" || first.Route != "gh api user" || first.ExitCode != 0 || first.StdoutBytes != len("octo\n") {
		t.Fatalf("first = %+v", first)
	}
	if second := log.Invocations[1]; second.Route != "glab api merge request" || second.Cwd != "/b" {
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

func TestCreateOpensAPullForTheCheckoutsBranch(t *testing.T) {
	origins := map[string]string{"/gh": "git@github.com:acme/widgets.git", "/gl": "https://gitlab.com/grp/sub/tool.git"}
	branch := "topic"
	e, _ := seeded(t, Options{
		Origin: func(cwd string) (string, error) { return origins[cwd], nil },
		Head: func(string) (CheckoutHead, error) {
			return CheckoutHead{Branch: branch, SHA: strings.Repeat("f", 40)}, nil
		},
	})
	gh := func(args ...string) control.ForgeResult {
		return e.Handle(control.ForgeCall{CLI: "gh", Cwd: "/gh", Args: args})
	}
	glab := func(args ...string) control.ForgeResult {
		return e.Handle(control.ForgeCall{CLI: "glab", Cwd: "/gl", Args: args})
	}

	created := gh("pr", "create", "--title", "Topic", "--body", "Why", "--draft")
	if created.ExitCode != 0 || string(created.Stdout) != "https://github.com/acme/widgets/pull/8\n" {
		t.Fatalf("gh pr create = %d %q %q", created.ExitCode, created.Stdout, created.Stderr)
	}
	listed := decode[[]map[string]any](t, gh("pr", "list", "--head", branch, "--state", "open", "--json", "number,title,body,isDraft,headRefOid,baseRefName"))
	if len(listed) != 1 || listed[0]["number"] != float64(8) || listed[0]["title"] != "Topic" || listed[0]["body"] != "Why" ||
		listed[0]["isDraft"] != true || listed[0]["headRefOid"] != strings.Repeat("f", 40) || listed[0]["baseRefName"] != "main" {
		t.Fatalf("created pull = %v", listed)
	}
	if again := gh("pr", "create", "--title", "Again", "--body", ""); again.ExitCode != 1 || !strings.Contains(again.Stderr, "already exists") {
		t.Fatalf("duplicate gh pr create = %d %q", again.ExitCode, again.Stderr)
	}
	if other := gh("pr", "create", "--title", "Elsewhere", "--body", "", "--base", "release"); other.ExitCode != 0 {
		t.Fatalf("gh pr create into another base = %d %q", other.ExitCode, other.Stderr)
	}
	if missing := gh("pr", "create", "--title", "No body"); missing.ExitCode != 1 || !strings.Contains(missing.Stderr, "--body") {
		t.Fatalf("gh pr create without --body = %d %q", missing.ExitCode, missing.Stderr)
	}

	mr := glab("mr", "create", "--title", "Topic", "--description", "Why", "--yes", "--no-editor", "--draft")
	if mr.ExitCode != 0 || string(mr.Stdout) != "https://gitlab.com/grp/sub/tool/-/merge_requests/4\n" {
		t.Fatalf("glab mr create = %d %q %q", mr.ExitCode, mr.Stdout, mr.Stderr)
	}
	shown := decode[map[string]any](t, glab("api", "projects/grp%2Fsub%2Ftool/merge_requests/4"))
	if shown["title"] != "Draft: Topic" || shown["draft"] != true || shown["description"] != "Why" || shown["source_branch"] != branch {
		t.Fatalf("created merge request = %v", shown)
	}
	if again := glab("mr", "create", "--title", "Again", "--description", "", "--yes", "--no-editor", "--target-branch", "release"); again.ExitCode != 1 ||
		!strings.Contains(again.Stderr, "Another open merge request already exists for this source branch: !4") {
		t.Fatalf("duplicate glab mr create = %d %q", again.ExitCode, again.Stderr)
	}

	branch = "main"
	if same := gh("pr", "create", "--title", "Main", "--body", ""); same.ExitCode != 1 {
		t.Fatalf("gh pr create from the base = %d %q", same.ExitCode, same.Stderr)
	}
	for _, args := range [][]string{
		{"mr", "create", "--title", "Prompted", "--description", ""},
		{"mr", "create", "--title", "Filled", "--description", "", "--yes", "--no-editor", "--fill"},
	} {
		inv := glab(args...)
		if inv.ExitCode != 1 || !strings.Contains(inv.Stderr, "unhandled") {
			t.Errorf("glab %v = %d %q, want unhandled", args, inv.ExitCode, inv.Stderr)
		}
	}
	if inv := gh("pr", "create", "--title", "Filled", "--body", "", "--fill"); !strings.Contains(inv.Stderr, "unhandled") {
		t.Errorf("gh pr create --fill = %q, want unhandled", inv.Stderr)
	}
}

func TestRepositoryIdentityEndpointHonorsHost(t *testing.T) {
	e, fixture := seeded(t, Options{})
	for _, r := range fixture.Repos {
		cli, endpoint := "gh", "repos/"+r.Project
		if r.Forge == "gitlab" {
			cli = "glab"
			endpoint = "projects/" + url.PathEscape(r.Project)
		}
		result := decode[struct {
			ID int64 `json:"id"`
		}](t, handle(e, cli, "api", "--hostname", r.Host, endpoint))
		if result.ID != r.ID {
			t.Fatal("wrong repository ID")
		}
		if got := handle(e, cli, "api", "--hostname", "other.example", endpoint); got.ExitCode == 0 {
			t.Fatalf("wrong host: %+v", got)
		}
	}
}

func TestRepositorySSHConfigIsIsolatedAndRejectsConnections(t *testing.T) {
	e, _ := seeded(t, Options{})
	answer := handle(e, "ssh", "-G", "-o", "CanonicalizeHostname=no", "-o", "PermitLocalCommand=no", "-l", "alice", "example.com")
	if answer.ExitCode != 0 || string(answer.Stdout) != "hostname example.com\nuser alice\n" {
		t.Fatalf("config: %+v", answer)
	}
	for _, args := range [][]string{{"example.com"}, {"-G", "-o", "ProxyCommand=anything", "example.com"}} {
		answer = handle(e, "ssh", args...)
		if answer.ExitCode == 0 || !strings.Contains(answer.Stderr, "unhandled") {
			t.Fatalf("connection: %+v", answer)
		}
	}
}

// The app's GraphQL text after whitespace collapse (internal/git
// githubReviewThreadsQuery and githubPRCommentsQuery).
const (
	appReviewThreadsQuery = `query { repository(owner: "acme", name: "widgets") { pullRequest(number: 7) { reviewThreads(first: 50) { pageInfo { hasNextPage endCursor } nodes { id isResolved isOutdated path line startLine diffSide startDiffSide subjectType comments(first: 50) { nodes { id databaseId author { login ... on User { name } } body createdAt replyTo { id databaseId } } } } } } } }`
	appPRCommentsQuery    = `query { repository(owner: "acme", name: "widgets") { pullRequest(number: 7) { comments(first: 50) { pageInfo { hasNextPage endCursor } nodes { id databaseId author { login ... on User { name } } body createdAt isMinimized } } } } }`
)

const namedFixture = `{"repos":[
  {"forge":"github","project":"acme/widgets","pulls":[{"number":7,"title":"t","author":"rmurphy","authorName":"Randy Murphy",
    "comments":[{"body":"by default"},{"author":"coderabbitai[bot]","body":"bot"}],
    "threads":[{"path":"w.go","line":1,"comments":[{"author":"bob","authorName":"Bob Smith","body":"nit"}]}]}]},
  {"forge":"gitlab","project":"grp/tool","pulls":[{"number":3,"title":"t","author":"dave","authorName":"Dave Jones",
    "comments":[{"body":"note"}],
    "reviews":[{"author":"erin","authorName":"Erin Lee","state":"APPROVED"}]}]}
]}`

type actorAnswer struct {
	Login    string  `json:"login"`
	Username string  `json:"username"`
	Name     *string `json:"name"`
}

func (a actorAnswer) String() string {
	if a.Name == nil {
		return a.Login + a.Username + "/-"
	}
	return a.Login + a.Username + "/" + *a.Name
}

func TestAuthorDisplayNamesAnswerTheAppsQueries(t *testing.T) {
	e := New(Options{})
	if _, err := seedJSON(e, namedFixture); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	type connection struct {
		Nodes []struct {
			Author   actorAnswer `json:"author"`
			Comments struct {
				Nodes []struct {
					Author actorAnswer `json:"author"`
				} `json:"nodes"`
			} `json:"comments"`
		} `json:"nodes"`
	}
	type graphQLAnswer struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads connection `json:"reviewThreads"`
					Comments      connection `json:"comments"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	threads := decode[graphQLAnswer](t, handle(e, "gh", "api", "graphql", "-f", "query="+appReviewThreadsQuery))
	nodes := threads.Data.Repository.PullRequest.ReviewThreads.Nodes
	if len(nodes) != 1 || len(nodes[0].Comments.Nodes) != 1 || nodes[0].Comments.Nodes[0].Author.String() != "bob/Bob Smith" {
		t.Fatalf("review thread authors = %+v", nodes)
	}
	comments := decode[graphQLAnswer](t, handle(e, "gh", "api", "graphql", "-f", "query="+appPRCommentsQuery))
	var got []string
	for _, node := range comments.Data.Repository.PullRequest.Comments.Nodes {
		got = append(got, node.Author.String())
	}
	// A defaulted comment carries the pull author's name; an unnamed one
	// answers as a bot, with no name key.
	if strings.Join(got, ",") != "rmurphy/Randy Murphy,coderabbitai[bot]/-" {
		t.Fatalf("conversation authors = %v", got)
	}

	// The query before the display-name selection is not one the app makes.
	stale := strings.ReplaceAll(appPRCommentsQuery, "author { login ... on User { name } }", "author { login }")
	if result := handle(e, "gh", "api", "graphql", "-f", "query="+stale); result.ExitCode == 0 || !strings.Contains(result.Stderr, "unhandled") {
		t.Fatalf("stale query answered: exit %d stderr %q", result.ExitCode, result.Stderr)
	}

	detail := decode[struct {
		Author actorAnswer `json:"author"`
	}](t, handle(e, "gh", "pr", "view", "--repo", "acme/widgets", "7", "--json", "author"))
	if detail.Author.String() != "rmurphy/Randy Murphy" {
		t.Fatalf("gh pr view author = %+v", detail.Author)
	}

	mr := decode[struct {
		Author actorAnswer `json:"author"`
	}](t, handle(e, "glab", "api", "projects/grp%2Ftool/merge_requests/3"))
	if mr.Author.String() != "dave/Dave Jones" {
		t.Fatalf("glab MR author = %+v", mr.Author)
	}
	approvals := decode[struct {
		ApprovedBy []struct {
			User actorAnswer `json:"user"`
		} `json:"approved_by"`
	}](t, handle(e, "glab", "api", "projects/grp%2Ftool/merge_requests/3/approvals"))
	if len(approvals.ApprovedBy) != 1 || approvals.ApprovedBy[0].User.String() != "erin/Erin Lee" {
		t.Fatalf("glab approvals = %+v", approvals)
	}
	discussions := decode[[]struct {
		Notes []struct {
			Author actorAnswer `json:"author"`
		} `json:"notes"`
	}](t, handle(e, "glab", "api", "projects/grp%2Ftool/merge_requests/3/discussions?per_page=50&page=1"))
	if len(discussions) != 1 || len(discussions[0].Notes) != 1 || discussions[0].Notes[0].Author.String() != "dave/Dave Jones" {
		t.Fatalf("glab note authors = %+v", discussions)
	}
}

func TestOfflineAnswersEveryForgeCallAsUnreachableUntilBack(t *testing.T) {
	e, _ := seeded(t, Options{})
	view := []string{"pr", "view", "--repo", "acme/widgets", "7", "--json", "title"}
	e.SetOffline(true)
	for _, argv := range [][]string{
		append([]string{"gh"}, view...),
		{"glab", "api", "projects/grp%2Fsub%2Ftool/merge_requests/3"},
	} {
		result := handle(e, argv[0], argv[1:]...)
		if result.ExitCode != 1 || len(result.Stdout) != 0 || strings.Contains(result.Stderr, "unhandled") {
			t.Errorf("%q offline: exit %d stdout %q stderr %q, want the CLI's connection failure", argv, result.ExitCode, result.Stdout, result.Stderr)
		}
	}
	if got := handle(e, "glab", "api", "projects/grp%2Fsub%2Ftool/merge_requests/3").Stderr; !strings.Contains(got, "lookup gitlab.com: i/o timeout") {
		t.Errorf("glab offline stderr = %q", got)
	}
	if got := handle(e, "gh", view...).Stderr; !strings.Contains(got, "error connecting to api.github.com") {
		t.Errorf("gh offline stderr = %q", got)
	}
	// ssh is the local client, not the forge.
	if answer := handle(e, "ssh", "-G", "-o", "CanonicalizeHostname=no", "-o", "PermitLocalCommand=no", "-l", "alice", "example.com"); answer.ExitCode != 0 {
		t.Errorf("ssh -G failed while the forge is offline: %s", answer.Stderr)
	}
	for _, inv := range e.Invocations(0).Invocations {
		if inv.CLI == "ssh" {
			continue
		}
		if inv.Unhandled || inv.Route != "offline" {
			t.Errorf("%q recorded as %q unhandled=%v, want route offline", inv.Args, inv.Route, inv.Unhandled)
		}
	}
	e.SetOffline(false)
	if result := handle(e, "gh", view...); result.ExitCode != 0 {
		t.Fatalf("the forge did not come back: exit %d %s", result.ExitCode, result.Stderr)
	}
	e.SetOffline(true)
	e.Reset()
	if _, err := seedJSON(e, testFixture); err != nil {
		t.Fatal(err)
	}
	if result := handle(e, "gh", view...); result.ExitCode != 0 {
		t.Fatalf("Reset left the forge offline: exit %d %s", result.ExitCode, result.Stderr)
	}
}

// TestGitHubJobLogsAnswer404UntilTheJobCompletes: the Actions log endpoint
// serves nothing for a running job, which is what makes the app wait for
// completion on GitHub while it follows a GitLab trace live.
func TestGitHubJobLogsAnswer404UntilTheJobCompletes(t *testing.T) {
	e := New(Options{})
	fixture, err := seedJSON(e, `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","ci":{"jobs":[{"id":500,"name":"j","status":"running","log":"partial"}]}}]}]}`)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	result := handle(e, "gh", "api", "repos/a/b/actions/jobs/500/logs")
	if result.ExitCode == 0 || !strings.Contains(result.Stderr, "404") {
		t.Fatalf("running job log: exit %d stdout %q stderr %q, want 404", result.ExitCode, result.Stdout, result.Stderr)
	}
	fixture.Repos[0].Pulls[0].CI.Jobs[0].Status = "success"
	if _, err := e.Seed(fixture); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	result = handle(e, "gh", "api", "repos/a/b/actions/jobs/500/logs")
	if result.ExitCode != 0 || string(result.Stdout) != "\xef\xbb\xbfpartial" {
		t.Fatalf("completed job log: exit %d stdout %q stderr %q", result.ExitCode, result.Stdout, result.Stderr)
	}
	// A completed job whose log the forge has not published yet.
	fixture.Repos[0].Pulls[0].CI.Jobs[0].LogWithheld = true
	if _, err := e.Seed(fixture); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	result = handle(e, "gh", "api", "repos/a/b/actions/jobs/500/logs")
	if result.ExitCode == 0 || !strings.Contains(result.Stderr, "(HTTP 404)") {
		t.Fatalf("withheld job log: exit %d stdout %q stderr %q, want 404", result.ExitCode, result.Stdout, result.Stderr)
	}
}

func TestGitLabTraceAnswers404WhileTheLogIsWithheld(t *testing.T) {
	e := New(Options{})
	fixture, err := seedJSON(e, `{"repos":[{"forge":"gitlab","project":"g/t","pulls":[{"number":1,"title":"x","ci":{"jobs":[{"id":600,"name":"j","status":"success","log":"done","logWithheld":true}]}}]}]}`)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	result := handle(e, "glab", "api", "projects/g%2Ft/jobs/600/trace")
	if result.ExitCode == 0 || !strings.Contains(result.Stderr, "(HTTP 404)") {
		t.Fatalf("withheld trace: exit %d stdout %q stderr %q, want 404", result.ExitCode, result.Stdout, result.Stderr)
	}
	fixture.Repos[0].Pulls[0].CI.Jobs[0].LogWithheld = false
	if _, err := e.Seed(fixture); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	result = handle(e, "glab", "api", "projects/g%2Ft/jobs/600/trace")
	if result.ExitCode != 0 || string(result.Stdout) != "done" {
		t.Fatalf("published trace: exit %d stdout %q stderr %q", result.ExitCode, result.Stdout, result.Stderr)
	}
}

// TestGitHubRunJobsAnswerTheRESTShape: the REST jobs list of a run, as the
// real endpoint spells it (snake_case, lowercase states, null for what is
// not there yet), paginated.
func TestGitHubRunJobsAnswerTheRESTShape(t *testing.T) {
	e := New(Options{})
	_, err := seedJSON(e, `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","ci":{"id":70,"name":"CI","jobs":[
{"id":501,"name":"build","status":"running","steps":[{"name":"Set up job","status":"success"},{"name":"Build","status":"running"}]},
{"id":502,"name":"lint","status":"failed","startedAt":"2026-01-01T00:00:00Z","completedAt":"2026-01-01T00:01:00Z"}]}}]}]}`)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	type step struct {
		Number     int     `json:"number"`
		Name       string  `json:"name"`
		Status     string  `json:"status"`
		Conclusion *string `json:"conclusion"`
	}
	type answer struct {
		TotalCount int `json:"total_count"`
		Jobs       []struct {
			ID          int64   `json:"id"`
			RunID       int64   `json:"run_id"`
			Name        string  `json:"name"`
			Status      string  `json:"status"`
			Conclusion  *string `json:"conclusion"`
			CompletedAt *string `json:"completed_at"`
			HTMLURL     string  `json:"html_url"`
			Steps       []step  `json:"steps"`
		} `json:"jobs"`
	}
	got := decode[answer](t, handle(e, "gh", "api", "repos/a/b/actions/runs/70/jobs?per_page=100"))
	if got.TotalCount != 2 || len(got.Jobs) != 2 {
		t.Fatalf("answer = %+v", got)
	}
	build, lint := got.Jobs[0], got.Jobs[1]
	if build.ID != 501 || build.RunID != 70 || build.Status != "in_progress" || build.Conclusion != nil || build.CompletedAt != nil ||
		build.HTMLURL != "https://github.com/a/b/actions/runs/70/job/501" {
		t.Fatalf("build = %+v", build)
	}
	if len(build.Steps) != 2 || build.Steps[0].Number != 1 || build.Steps[0].Status != "completed" || *build.Steps[0].Conclusion != "success" ||
		build.Steps[1].Status != "in_progress" || build.Steps[1].Conclusion != nil {
		t.Fatalf("build steps = %+v", build.Steps)
	}
	if lint.Status != "completed" || lint.Conclusion == nil || *lint.Conclusion != "failure" || lint.CompletedAt == nil {
		t.Fatalf("lint = %+v", lint)
	}

	second := decode[answer](t, handle(e, "gh", "api", "repos/a/b/actions/runs/70/jobs?per_page=1&page=2"))
	if second.TotalCount != 2 || len(second.Jobs) != 1 || second.Jobs[0].ID != 502 {
		t.Fatalf("page 2 = %+v", second)
	}
	if result := handle(e, "gh", "api", "repos/a/b/actions/runs/71/jobs?per_page=100"); result.ExitCode == 0 || !strings.Contains(result.Stderr, "(HTTP 404)") {
		t.Fatalf("unknown run: exit %d stderr %q, want 404", result.ExitCode, result.Stderr)
	}
}
