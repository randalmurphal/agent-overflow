package forgefake

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

// seeded is an engine with testFixture whose HTTP mounts accept
// testAPIToken.
func seeded(t *testing.T, opts Options) (*Engine, Fixture) {
	t.Helper()
	opts.APIToken = testAPIToken
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
		"no repos":            `{"repos":[]}`,
		"bad forge":           `{"repos":[{"forge":"bitbucket","project":"a/b"}]}`,
		"github project":      `{"repos":[{"forge":"github","project":"a/b/c"}]}`,
		"duplicate repo":      `{"repos":[{"forge":"github","project":"a/b"},{"forge":"github","project":"A/B"}]}`,
		"duplicate pull":      `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x"},{"number":1,"title":"y"}]}]}`,
		"line thread":         `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","threads":[{"path":"f","comments":[{"body":"b"}]}]}]}]}`,
		"gitlab verdict":      `{"repos":[{"forge":"gitlab","project":"g/r","pulls":[{"number":1,"title":"x","reviews":[{"author":"a","state":"CHANGES_REQUESTED"}]}]}]}`,
		"name without login":  `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","comments":[{"authorName":"A","body":"b"}]}]}]}`,
		"job status":          `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","ci":{"jobs":[{"name":"j","status":"green"}]}}]}]}`,
		"step out of order":   `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","ci":{"jobs":[{"name":"j","status":"success","steps":[{"number":3,"name":"a","status":"success"},{"number":2,"name":"b","status":"success"}]}]}}]}]}`,
		"gitlab upload key":   `{"repos":[{"forge":"gitlab","project":"g/r","attachments":[{"secret":"nothex","filename":"a.png","text":"x"}]}]}`,
		"github upload key":   `{"repos":[{"forge":"github","project":"a/b","attachments":[{"secret":"0123456789abcdef0123456789abcdef","filename":"a","text":"x"}]}]}`,
		"two contents":        `{"repos":[{"forge":"github","project":"a/b","attachments":[{"url":"https://github.com/x","text":"x","base64":"eA=="}]}]}`,
		"padded project":      `{"repos":[{"forge":"github","project":"a/ b"}]}`,
		"negative pr number":  `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":-1,"title":"x"}]}]}`,
		"github permission":   `{"repos":[{"forge":"github","project":"a/b","merge":{"permission":"developer"}}]}`,
		"gitlab permission":   `{"repos":[{"forge":"gitlab","project":"g/r","merge":{"permission":"WRITE"}}]}`,
		"gitlab on github":    `{"repos":[{"forge":"github","project":"a/b","merge":{"mergeMethod":"ff"}}]}`,
		"github on gitlab":    `{"repos":[{"forge":"gitlab","project":"g/r","merge":{"autoMergeAllowed":true}}]}`,
		"no methods":          `{"repos":[{"forge":"github","project":"a/b","merge":{"methods":[]}}]}`,
		"default not allowed": `{"repos":[{"forge":"github","project":"a/b","merge":{"methods":["squash"],"defaultMethod":"merge"}}]}`,
		"merge method":        `{"repos":[{"forge":"gitlab","project":"g/r","merge":{"mergeMethod":"octopus"}}]}`,
		"squash option":       `{"repos":[{"forge":"gitlab","project":"g/r","merge":{"squashOption":"sometimes"}}]}`,
		"gitlab checks":       `{"repos":[{"forge":"gitlab","project":"g/r","pulls":[{"number":1,"title":"x","readiness":{"requiredChecks":["unit"]}}]}]}`,
		"github can approve":  `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","readiness":{"viewerCanApprove":true}}]}]}`,
		"negative approvals":  `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","readiness":{"requiredApprovals":-1}}]}]}`,
		"armed merged pull":   `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","state":"merged","readiness":{"autoMerge":{}}}]}]}`,
		"armed method":        `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","readiness":{"autoMerge":{"method":"ff"}}}]}]}`,
		"gitlab behind":       `{"repos":[{"forge":"gitlab","project":"g/r","pulls":[{"number":1,"title":"x","readiness":{"behind":true}}]}]}`,
		"merged by on open":   `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","mergedBy":"a"}]}]}`,
		"closed at on open":   `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","closedAt":"2026-01-01T00:00:00Z"}]}]}`,
		"event kind":          `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","events":[{"kind":"approved"}]}]}]}`,
		"gitlab force push":   `{"repos":[{"forge":"gitlab","project":"g/r","pulls":[{"number":1,"title":"x","events":[{"kind":"force_pushed"}]}]}]}`,
		"event time":          `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","events":[{"kind":"reopened","at":"yesterday"}]}]}]}`,
		"event without ref":   `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","events":[{"kind":"base_changed"}]}]}]}`,
		"gitlab event method": `{"repos":[{"forge":"gitlab","project":"g/r","pulls":[{"number":1,"title":"x","events":[{"kind":"auto_merge_set","method":"squash"}]}]}]}`,
	}
	for name, raw := range cases {
		if _, err := seedJSON(New(Options{}), raw); err == nil {
			t.Errorf("%s: seeded without error", name)
		}
	}
}

func TestSeedReplacesTheSameRepoAndRefusesAnIDCollision(t *testing.T) {
	e, fixture := seeded(t, Options{})
	srv := testServer(t, e)
	if _, err := seedJSON(e, `{"repos":[{"forge":"github","project":"ACME/widgets","pulls":[{"number":9,"title":"Only"}]}]}`); err != nil {
		t.Fatal(err)
	}
	if got := prTick(t, srv, "github.com", "acme", "widgets", 7, true, false, false); !strings.Contains(got.body, `"NOT_FOUND"`) {
		t.Fatalf("the replaced repo still answers #7: %s", got.body)
	}
	if got := prTick(t, srv, "github.com", "acme", "widgets", 9, true, false, false); !strings.Contains(got.body, `"title":"Only"`) {
		t.Fatalf("#9 = %s", got.body)
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
		// GitHub reads are HTTP requests; the gh CLI answers only pr create.
		{"gh", "pr", "view", "--repo", "acme/widgets", "7", "--json", "title"},
		{"gh", "pr", "list", "--head", "feature", "--state", "open", "--json", "url"},
		{"gh", "api", "user", "--jq", ".login"},
		{"gh", "api", "--hostname", "github.com", "repos/acme/widgets"},
		{"gh", "run", "view", "1", "--repo", "acme/widgets", "--json", "jobs"},
		{"glab", "mr", "create", "--title", "t"},
		{"glab", "api", "--hostname", "gitlab.com", "projects/grp%2Fsub%2Ftool/merge_requests/3/discussions?per_page=50&page=1&sort=asc", "--include"},
		{"glab", "api", "--hostname", "gitlab.com", "projects/grp%2Fsub%2Ftool/merge_requests/3/approve", "-X", "POST", "-f", "sha=abc"},
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
	srv := testServer(t, e)
	for _, path := range []string{
		"projects/grp%2Fsub%2Ftool/merge_requests/99",
		"projects/grp%2Fsub%2Ftool/uploads/0123456789abcdef0123456789abcdef/other.svg",
	} {
		if got := glabGET(t, srv, "gitlab.com", path); got.status != 404 || !strings.Contains(got.body, "Not Found") {
			t.Errorf("%s = %+v, want a forge not-found error", path, got)
		}
	}
	// GitHub's GraphQL answers a missing object as 200 with NOT_FOUND
	// beside null data; REST answers 404.
	if got := prTick(t, srv, "github.com", "acme", "widgets", 99, true, true, true); got.status != 200 ||
		!strings.Contains(got.body, `"NOT_FOUND"`) || !strings.Contains(got.body, `"pullRequest":null`) || !strings.Contains(got.body, `"path":["repository","pullRequest"]`) {
		t.Errorf("missing pull = %+v", got)
	}
	if got := prTick(t, srv, "github.com", "acme", "nothing", 7, true, true, true); got.status != 200 || !strings.Contains(got.body, `"repository":null`) {
		t.Errorf("missing repository = %+v", got)
	}
	if got := httpRequest(t, srv, "GET", "127.0.0.1", "/github/absolute/github.com/user-attachments/assets/missing", "", nil); got.status != 404 {
		t.Errorf("missing attachment = %+v", got)
	}
	for _, inv := range e.Invocations(0).Invocations {
		if inv.Unhandled || inv.Route == "" {
			t.Errorf("%+v recorded as unhandled", inv)
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
	srv := testServer(t, e)
	ssh := e.Handle(control.ForgeCall{CLI: "ssh", Args: []string{"-G", "example.com"}, Cwd: "/a", Stdin: []byte("in")})
	glabGET(t, srv, "gitlab.com", "projects/grp%2Fsub%2Ftool/merge_requests/3")

	log := e.Invocations(0)
	if len(log.Invocations) != 2 || len(observed) != 2 {
		t.Fatalf("recorded %d, observed %d", len(log.Invocations), len(observed))
	}
	first := log.Invocations[0]
	if first.Seq != 1 || first.Via != ViaCLI || first.CLI != "ssh" || strings.Join(first.Args, " ") != "-G example.com" ||
		first.Cwd != "/a" || first.Stdin != "in" || first.Route != "ssh" || first.ExitCode != 0 || first.StdoutBytes != len(ssh.Stdout) {
		t.Fatalf("first = %+v", first)
	}
	if second := log.Invocations[1]; second.Via != ViaHTTP || second.Route != "glab api merge request" || second.Host != "gitlab.com" {
		t.Fatalf("second = %+v", second)
	}
	if since := e.Invocations(1); len(since.Invocations) != 1 || since.Invocations[0].Seq != 2 {
		t.Fatalf("since 1 = %+v", since)
	}
	// The returned log is a copy.
	log.Invocations[0].Args[0] = "mutated"
	if e.Invocations(0).Invocations[0].Args[0] != "-G" {
		t.Fatal("Invocations returned the engine's own slice")
	}
}

func TestInvocationLogIsBounded(t *testing.T) {
	e, _ := seeded(t, Options{})
	for range maxInvocations + 5 {
		handle(e, "ssh", "-G", "example.com")
	}
	log := e.Invocations(0)
	if len(log.Invocations) != maxInvocations || log.Dropped != 5 || log.Invocations[0].Seq != 6 {
		t.Fatalf("len %d dropped %d first seq %d", len(log.Invocations), log.Dropped, log.Invocations[0].Seq)
	}
}

func TestResetClearsStateAndLogButNotIDs(t *testing.T) {
	e, fixture := seeded(t, Options{})
	srv := testServer(t, e)
	glabGET(t, srv, "gitlab.com", "projects/grp%2Fsub%2Ftool/merge_requests/3")
	e.Reset()
	if log := e.Invocations(0); len(log.Invocations) != 0 || log.Dropped != 0 {
		t.Fatalf("log after reset: %+v", log)
	}
	got := prTick(t, srv, "github.com", "acme", "widgets", 7, true, false, false)
	if !strings.Contains(got.body, `"NOT_FOUND"`) {
		t.Fatal("seeded repo survived reset")
	}
	if !strings.Contains(got.body, `"viewer":{"login":"`+defaultViewer+`"}`) {
		t.Fatalf("viewer after reset: %s", got.body)
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
	if flags["repo"][0] != "x/y" || len(flags["include"]) != 1 || strings.Join(flags["header"], ",") != "h1,h2" ||
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
	srv := testServer(t, e)
	open := graphQL(t, srv, "github.com", "OpenPRsByHead", map[string]any{"owner": "acme", "name": "widgets", "head": branch})
	if !strings.Contains(open.body, `"number":8,"state":"OPEN","title":"Topic","url":"https://github.com/acme/widgets/pull/8"}]`) ||
		!strings.Contains(open.body, `"nodes":[{"id":"PR_ao`) {
		t.Fatalf("open pulls after create = %s", open.body)
	}
	var created8 struct {
		Data struct {
			Repository struct {
				PullRequest map[string]any `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	decodeBody(t, prTick(t, srv, "github.com", "acme", "widgets", 8, true, false, false), &created8)
	if pull := created8.Data.Repository.PullRequest; pull["body"] != "Why" || pull["isDraft"] != true || pull["headRefOid"] != strings.Repeat("f", 40) || pull["baseRefName"] != "main" {
		t.Fatalf("created pull = %v", pull)
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
	var shown map[string]any
	decodeBody(t, glabGET(t, srv, "gitlab.com", "projects/grp%2Fsub%2Ftool/merge_requests/4"), &shown)
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
	srv := testServer(t, e)
	for _, r := range fixture.Repos {
		var got httpAnswer
		var wrong httpAnswer
		if r.Forge == "github" {
			got = httpRequest(t, srv, "GET", r.Host, "/github/rest/repos/"+r.Project, "", nil)
			wrong = httpRequest(t, srv, "GET", "other.example", "/github/rest/repos/"+r.Project, "", nil)
		} else {
			endpoint := "projects/" + url.PathEscape(r.Project)
			got = glabGET(t, srv, r.Host, endpoint)
			wrong = glabGET(t, srv, "other.example", endpoint)
		}
		var identity struct {
			ID int64 `json:"id"`
		}
		decodeBody(t, got, &identity)
		if identity.ID != r.ID {
			t.Fatalf("%s identity = %s", r.Forge, got.body)
		}
		if wrong.status != 404 {
			t.Fatalf("%s identity on another host = %+v", r.Forge, wrong)
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

const namedFixture = `{"repos":[
  {"forge":"github","project":"acme/widgets","pulls":[{"number":7,"title":"t","author":"rmurphy","authorName":"Randy Murphy",
    "comments":[{"body":"by default"},{"author":"coderabbitai[bot]","body":"bot"}],
    "threads":[{"path":"w.go","line":1,"comments":[{"author":"bob","authorName":"Bob Smith","body":"nit"}]}],
    "reviews":[{"author":"carol","authorName":"Carol Diaz","state":"APPROVED"},{"author":"copilot-pull-request-reviewer","state":"COMMENTED"}]}]},
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
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, namedFixture); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	srv := testServer(t, e)

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
	var tick struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					Author        actorAnswer `json:"author"`
					LatestReviews connection  `json:"latestReviews"`
					ReviewThreads connection  `json:"reviewThreads"`
					Comments      connection  `json:"comments"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	decodeBody(t, prTick(t, srv, "github.com", "acme", "widgets", 7, true, true, false), &tick)
	pull := tick.Data.Repository.PullRequest
	if pull.Author.String() != "rmurphy/Randy Murphy" {
		t.Fatalf("PR author = %+v", pull.Author)
	}
	if nodes := pull.ReviewThreads.Nodes; len(nodes) != 1 || len(nodes[0].Comments.Nodes) != 1 || nodes[0].Comments.Nodes[0].Author.String() != "bob/Bob Smith" {
		t.Fatalf("review thread authors = %+v", nodes)
	}
	var got []string
	for _, node := range pull.Comments.Nodes {
		got = append(got, node.Author.String())
	}
	// A defaulted comment carries the pull author's name; an unnamed one
	// answers as a bot, with no name key.
	if strings.Join(got, ",") != "rmurphy/Randy Murphy,coderabbitai[bot]/-" {
		t.Fatalf("conversation authors = %v", got)
	}
	got = nil
	for _, node := range pull.LatestReviews.Nodes {
		got = append(got, node.Author.String())
	}
	if strings.Join(got, ",") != "carol/Carol Diaz,copilot-pull-request-reviewer/-" {
		t.Fatalf("latest review authors = %v", got)
	}

	var mr struct {
		Author actorAnswer `json:"author"`
	}
	decodeBody(t, glabGET(t, srv, "gitlab.com", "projects/grp%2Ftool/merge_requests/3"), &mr)
	if mr.Author.String() != "dave/Dave Jones" {
		t.Fatalf("GitLab MR author = %+v", mr.Author)
	}
	var approvals struct {
		ApprovedBy []struct {
			User actorAnswer `json:"user"`
		} `json:"approved_by"`
	}
	decodeBody(t, glabGET(t, srv, "gitlab.com", "projects/grp%2Ftool/merge_requests/3/approvals"), &approvals)
	if len(approvals.ApprovedBy) != 1 || approvals.ApprovedBy[0].User.String() != "erin/Erin Lee" {
		t.Fatalf("GitLab approvals = %+v", approvals)
	}
	var discussions []struct {
		Notes []struct {
			Author actorAnswer `json:"author"`
		} `json:"notes"`
	}
	decodeBody(t, glabGET(t, srv, "gitlab.com", "projects/grp%2Ftool/merge_requests/3/discussions?per_page=100"), &discussions)
	if len(discussions) != 1 || len(discussions[0].Notes) != 1 || discussions[0].Notes[0].Author.String() != "dave/Dave Jones" {
		t.Fatalf("GitLab note authors = %+v", discussions)
	}
}
func TestOfflineAnswersEveryForgeCallAsUnreachableUntilBack(t *testing.T) {
	e, _ := seeded(t, Options{})
	srv := testServer(t, e)
	mrCreate := []string{"mr", "create", "--title", "t", "--description", "d", "--yes", "--no-editor"}
	create := []string{"pr", "create", "--title", "t", "--body", "b"}
	e.SetOffline(true)
	for _, argv := range [][]string{append([]string{"gh"}, create...), append([]string{"glab"}, mrCreate...)} {
		result := handle(e, argv[0], argv[1:]...)
		if result.ExitCode != 1 || len(result.Stdout) != 0 || strings.Contains(result.Stderr, "unhandled") {
			t.Errorf("%q offline: exit %d stdout %q stderr %q, want the CLI's connection failure", argv, result.ExitCode, result.Stdout, result.Stderr)
		}
	}
	if got := handle(e, "glab", mrCreate...).Stderr; !strings.Contains(got, "lookup gitlab.com: i/o timeout") {
		t.Errorf("glab offline stderr = %q", got)
	}
	if got := handle(e, "gh", create...).Stderr; !strings.Contains(got, "error connecting to api.github.com") {
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
	mr := "projects/grp%2Fsub%2Ftool/merge_requests/3"
	e.SetOffline(false)
	if got := glabGET(t, srv, "gitlab.com", mr); got.status != 200 {
		t.Fatalf("the forge did not come back: %+v", got)
	}
	e.SetOffline(true)
	e.Reset()
	if _, err := seedJSON(e, testFixture); err != nil {
		t.Fatal(err)
	}
	if got := glabGET(t, srv, "gitlab.com", mr); got.status != 200 {
		t.Fatalf("Reset left the forge offline: %+v", got)
	}
}

// TestGitHubJobLogsServeARunningJobOnceItsBlobExists: the Actions log
// endpoint answers 404 for a running job until its log blob exists
// (logWithheld), then serves the log so far, with an ETag that moves as it
// grows, and the whole log once the job completed.
func TestGitHubJobLogsServeARunningJobOnceItsBlobExists(t *testing.T) {
	e := New(Options{APIToken: testAPIToken})
	fixture, err := seedJSON(e, `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","ci":{"jobs":[
{"id":500,"name":"j","status":"running","log":"partial","logWithheld":true},
{"id":501,"name":"queued","status":"pending"}]}}]}]}`)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	srv := testServer(t, e)
	logs := func(job string, header http.Header) httpAnswer {
		return httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/a/b/actions/jobs/"+job+"/logs", "", header)
	}
	if got := logs("500", nil); got.status != 404 || lastInvocation(t, e).Unhandled {
		t.Fatalf("running job log before its blob exists = %+v, want a handled 404", got)
	}
	if got := logs("501", nil); got.status != 404 || lastInvocation(t, e).Unhandled {
		t.Fatalf("queued job log = %+v, want a handled 404", got)
	}
	fixture.Repos[0].Pulls[0].CI.Jobs[0].LogWithheld = false
	if _, err := e.Seed(fixture); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	partial := logs("500", nil)
	if partial.status != 200 || partial.body != "\xef\xbb\xbfpartial" || partial.header.Get("ETag") == "" {
		t.Fatalf("running job log = %+v", partial)
	}
	conditional := http.Header{"Authorization": {"Bearer " + testAPIToken}, "If-None-Match": {partial.header.Get("ETag")}}
	if got := logs("500", conditional); got.status != 304 || got.body != "" {
		t.Fatalf("unchanged running log revalidated = %+v, want 304", got)
	}
	fixture.Repos[0].Pulls[0].CI.Jobs[0].Log = "partial\nmore"
	if _, err := e.Seed(fixture); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	grown := logs("500", conditional)
	if grown.status != 200 || grown.body != "\xef\xbb\xbfpartial\nmore" || grown.header.Get("ETag") == partial.header.Get("ETag") {
		t.Fatalf("grown running log = %+v, want the new text under a new ETag", grown)
	}
	fixture.Repos[0].Pulls[0].CI.Jobs[0].Status = "success"
	if _, err := e.Seed(fixture); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if got := logs("500", nil); got.status != 200 || got.body != "\xef\xbb\xbfpartial\nmore" {
		t.Fatalf("completed job log = %+v", got)
	}
	// A completed job whose log the forge has not published yet.
	fixture.Repos[0].Pulls[0].CI.Jobs[0].LogWithheld = true
	if _, err := e.Seed(fixture); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if got := logs("500", nil); got.status != 404 || lastInvocation(t, e).Unhandled {
		t.Fatalf("withheld job log = %+v, want a handled 404", got)
	}
}
func TestGitLabTraceAnswers404WhileTheLogIsWithheld(t *testing.T) {
	e := New(Options{APIToken: testAPIToken})
	fixture, err := seedJSON(e, `{"repos":[{"forge":"gitlab","project":"g/t","pulls":[{"number":1,"title":"x","ci":{"jobs":[{"id":600,"name":"j","status":"success","log":"done","logWithheld":true}]}}]}]}`)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	srv := testServer(t, e)
	if got := glabGET(t, srv, "gitlab.com", "projects/g%2Ft/jobs/600/trace"); got.status != 404 || lastInvocation(t, e).Unhandled {
		t.Fatalf("withheld trace = %+v, want a handled 404", got)
	}
	fixture.Repos[0].Pulls[0].CI.Jobs[0].LogWithheld = false
	if _, err := e.Seed(fixture); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if got := glabGET(t, srv, "gitlab.com", "projects/g%2Ft/jobs/600/trace"); got.status != 200 || got.body != "done" || !strings.HasPrefix(got.header.Get("Content-Type"), "text/plain") {
		t.Fatalf("published trace = %+v", got)
	}
}

// TestGitHubRunJobsAnswerTheRESTShape: the REST jobs list of a run, as the
// real endpoint spells it (snake_case, lowercase states, null for what is
// not there yet), paginated with a Link to the next page.
func TestGitHubRunJobsAnswerTheRESTShape(t *testing.T) {
	e := New(Options{APIToken: testAPIToken})
	_, err := seedJSON(e, `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","ci":{"id":70,"name":"CI","jobs":[
{"id":501,"name":"build","status":"running","steps":[{"name":"Set up job","status":"success","startedAt":"2026-01-01T00:00:00Z","completedAt":"2026-01-01T00:00:01Z"},{"number":4,"name":"Build","status":"running","startedAt":"2026-01-01T00:00:01Z"},{"name":"Post Build","status":"pending"}]},
{"id":502,"name":"lint","status":"failed","startedAt":"2026-01-01T00:00:00Z","completedAt":"2026-01-01T00:01:00Z"}]}}]}]}`)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	srv := testServer(t, e)
	type step struct {
		Number      int     `json:"number"`
		Name        string  `json:"name"`
		Status      string  `json:"status"`
		Conclusion  *string `json:"conclusion"`
		StartedAt   *string `json:"started_at"`
		CompletedAt *string `json:"completed_at"`
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
	jobs := func(query string) (httpAnswer, answer) {
		resp := httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/a/b/actions/runs/70/jobs"+query, "", nil)
		var got answer
		decodeBody(t, resp, &got)
		return resp, got
	}
	resp, got := jobs("?per_page=100")
	if got.TotalCount != 2 || len(got.Jobs) != 2 || resp.header.Get("Link") != "" {
		t.Fatalf("answer = %+v, Link %q", got, resp.header.Get("Link"))
	}
	build, lint := got.Jobs[0], got.Jobs[1]
	if build.ID != 501 || build.RunID != 70 || build.Status != "in_progress" || build.Conclusion != nil || build.CompletedAt != nil ||
		build.HTMLURL != "https://github.com/a/b/actions/runs/70/job/501" {
		t.Fatalf("build = %+v", build)
	}
	if len(build.Steps) != 3 || build.Steps[0].Number != 1 || build.Steps[0].Status != "completed" || *build.Steps[0].Conclusion != "success" ||
		build.Steps[1].Status != "in_progress" || build.Steps[1].Conclusion != nil {
		t.Fatalf("build steps = %+v", build.Steps)
	}
	// Steps number on from a given number, and an unset time is null.
	if set, run, post := build.Steps[0], build.Steps[1], build.Steps[2]; set.StartedAt == nil || *set.StartedAt != "2026-01-01T00:00:00Z" ||
		set.CompletedAt == nil || *set.CompletedAt != "2026-01-01T00:00:01Z" || run.Number != 4 || run.StartedAt == nil ||
		*run.StartedAt != "2026-01-01T00:00:01Z" || run.CompletedAt != nil || post.Number != 5 || post.StartedAt != nil || post.CompletedAt != nil {
		t.Fatalf("build step numbers and times = %+v", build.Steps)
	}
	if lint.Status != "completed" || lint.Conclusion == nil || *lint.Conclusion != "failure" || lint.CompletedAt == nil {
		t.Fatalf("lint = %+v", lint)
	}

	first, page1 := jobs("?per_page=1")
	if want := `<` + srv.URL + `/github/rest/repos/a/b/actions/runs/70/jobs?page=2&per_page=1>; rel="next"`; first.header.Get("Link") != want || len(page1.Jobs) != 1 {
		t.Fatalf("page 1 Link = %q, want %q", first.header.Get("Link"), want)
	}
	last, second := jobs("?per_page=1&page=2")
	if second.TotalCount != 2 || len(second.Jobs) != 1 || second.Jobs[0].ID != 502 || last.header.Get("Link") != "" {
		t.Fatalf("page 2 = %+v, Link %q", second, last.header.Get("Link"))
	}
	if unknown := httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/a/b/actions/runs/71/jobs?per_page=100", "", nil); unknown.status != 404 {
		t.Fatalf("unknown run = %+v, want 404", unknown)
	}
	if filtered := httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/a/b/actions/runs/70/jobs?per_page=100&filter=all", "", nil); filtered.status != 404 || !lastInvocation(t, e).Unhandled {
		t.Fatalf("an unimplemented query parameter = %+v, want unhandled", filtered)
	}
}
func TestPRScopedCallsAnswerOnlyOnTheSeededHost(t *testing.T) {
	e := New(Options{APIToken: testAPIToken})
	_, err := seedJSON(e, `{"repos":[
{"forge":"gitlab","host":"gitlab.example.com:8443","project":"grp/tool",
 "attachments":[{"secret":"0123456789abcdef0123456789abcdef","filename":"d.svg","contentType":"image/svg+xml","text":"<svg/>"}],
 "pulls":[{"number":3,"title":"F","ci":{"id":80,"jobs":[{"id":601,"name":"j","status":"success","log":"ok"}]}}]}]}`)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	srv := testServer(t, e)
	for name, path := range map[string]string{
		"merge request": "projects/grp%2Ftool/merge_requests/3",
		"approvals":     "projects/grp%2Ftool/merge_requests/3/approvals",
		"discussions":   "projects/grp%2Ftool/merge_requests/3/discussions?per_page=100",
		"pipeline jobs": "projects/grp%2Ftool/pipelines/80/jobs?per_page=100",
		"job trace":     "projects/grp%2Ftool/jobs/601/trace",
		"upload":        "projects/grp%2Ftool/uploads/0123456789abcdef0123456789abcdef/d.svg",
	} {
		if got := glabGET(t, srv, "gitlab.example.com:8443", path); got.status != 200 {
			t.Errorf("%s on the seeded host = %+v", name, got)
		}
		// The Host header is compared as given: the bare hostname is
		// another host.
		for _, host := range []string{"gitlab.example.com", "gitlab.com"} {
			if got := glabGET(t, srv, host, path); got.status != 404 || lastInvocation(t, e).Unhandled || !strings.Contains(got.body, "Not Found") {
				t.Errorf("%s on %s = %+v, want the forge's not-found", name, host, got)
			}
		}
	}
}

// testAPIToken is the fixed token the tests' HTTP mounts accept.
const testAPIToken = "fake-token-1"

// testServer serves e over HTTP.
func testServer(t *testing.T, e *Engine) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return srv
}

// graphQLDocument is a document that declares op as the fake's table
// does; the fake reads the declaration, not the selections.
func graphQLDocument(t *testing.T, op string) string {
	t.Helper()
	for _, candidate := range githubGraphQL {
		if candidate.name != op {
			continue
		}
		var params []string
		for _, name := range append(slices.Clone(candidate.required), candidate.optional...) {
			params = append(params, "$"+name+": String")
		}
		return candidate.kind + " " + op + "(" + strings.Join(params, ", ") + ") { __typename }"
	}
	t.Fatalf("no GraphQL operation %s", op)
	return ""
}

// graphQL sends one operation to the GitHub GraphQL mount for host.
func graphQL(t *testing.T, srv *httptest.Server, host, op string, vars map[string]any) httpAnswer {
	t.Helper()
	return httpRequest(t, srv, "POST", host, "/github/graphql", mustJSON(t, map[string]any{
		"query": graphQLDocument(t, op), "operationName": op, "variables": vars,
	}), nil)
}

func prTick(t *testing.T, srv *httptest.Server, host, owner, name string, number int, detail, threads, checks bool) httpAnswer {
	t.Helper()
	return graphQL(t, srv, host, "PRTick", map[string]any{
		"owner": owner, "name": name, "number": number,
		"wantDetail": detail, "wantThreads": threads, "wantEvents": false, "wantChecks": checks,
	})
}

func decodeBody(t *testing.T, answer httpAnswer, out any) {
	t.Helper()
	if answer.status != 200 {
		t.Fatalf("status %d: %s", answer.status, answer.body)
	}
	if err := json.Unmarshal([]byte(answer.body), out); err != nil {
		t.Fatalf("decode %q: %v", answer.body, err)
	}
}

type httpAnswer struct {
	status int
	header http.Header
	body   string
}

func httpRequest(t *testing.T, srv *httptest.Server, method, host, path string, body string, header http.Header) httpAnswer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if header == nil {
		header = http.Header{"Authorization": {"Bearer " + testAPIToken}}
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return httpAnswer{status: resp.StatusCode, header: resp.Header, body: string(out)}
}

// glabGET sends a GitLab REST GET for path, relative to /api/v4/, to
// host, with the token as a personal token.
func glabGET(t *testing.T, srv *httptest.Server, host, path string) httpAnswer {
	t.Helper()
	return httpRequest(t, srv, "GET", host, "/gitlab/api/v4/"+path, "", http.Header{"Private-Token": {testAPIToken}})
}

func lastInvocation(t *testing.T, e *Engine) Invocation {
	t.Helper()
	log := e.Invocations(0).Invocations
	if len(log) == 0 {
		t.Fatal("nothing recorded")
	}
	return log[len(log)-1]
}

func TestHTTPMountsAnswerFromTheRouteTables(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, testFixture); err != nil {
		t.Fatal(err)
	}
	if _, err := seedJSON(e, `{"repos":[{"forge":"github","host":"ghe.example:8443","project":"acme/tools","pulls":[{"number":2,"title":"T","ci":{"id":70,"jobs":[{"id":501,"name":"j","status":"success","log":"done"}]}}]}]}`); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(e)
	defer srv.Close()

	mr := httpRequest(t, srv, "GET", "gitlab.com", "/gitlab/api/v4/projects/grp%2Fsub%2Ftool/merge_requests/3", "", http.Header{"Private-Token": {testAPIToken}})
	if mr.status != 200 || !strings.Contains(mr.body, `"iid":3`) || mr.header.Get("Content-Type") != "application/json" {
		t.Fatalf("merge request = %+v", mr)
	}
	inv := lastInvocation(t, e)
	if inv.Via != ViaHTTP || inv.Forge != "gitlab" || inv.Host != "gitlab.com" || inv.Method != "GET" || inv.Path != "projects/grp%2Fsub%2Ftool/merge_requests/3" ||
		inv.Status != 200 || inv.Route != "glab api merge request" || inv.Unhandled {
		t.Fatalf("invocation = %+v", inv)
	}

	discussions := httpRequest(t, srv, "GET", "gitlab.com", "/gitlab/api/v4/projects/grp%2Fsub%2Ftool/merge_requests/3/discussions?per_page=1&page=1", "", nil)
	if discussions.status != 200 || discussions.header.Get("X-Page") != "1" || strings.HasPrefix(discussions.body, "HTTP/") {
		t.Fatalf("discussions = %+v, want headers as headers", discussions)
	}

	list := httpRequest(t, srv, "GET", "gitlab.com", "/gitlab/api/v4/projects/grp%2Fsub%2Ftool/merge_requests?state=opened&per_page=5", "", nil)
	if list.status != 200 || !strings.Contains(list.body, `"iid":3`) {
		t.Fatalf("merge request list = %+v", list)
	}

	tick := prTick(t, srv, "github.com", "acme", "widgets", 7, false, true, false)
	if tick.status != 200 || !strings.Contains(tick.body, `"reviewThreads"`) {
		t.Fatalf("PRTick = %+v", tick)
	}
	if got := lastInvocation(t, e); got.Path != "graphql" || got.Route != "gh graphql PRTick" || got.Operation != "PRTick" ||
		string(got.Variables) != `{"name":"widgets","number":7,"owner":"acme","wantChecks":false,"wantDetail":false,"wantEvents":false,"wantThreads":true}` {
		t.Fatalf("PRTick invocation = %+v (variables %s)", got, got.Variables)
	}
	missing := prTick(t, srv, "ghe.example:8443", "acme", "widgets", 7, true, true, true)
	if missing.status != 200 || !strings.Contains(missing.body, `"NOT_FOUND"`) || lastInvocation(t, e).Unhandled {
		t.Fatalf("PRTick for a repository the host lacks = %+v, want a 200 NOT_FOUND envelope", missing)
	}
	if onHost := prTick(t, srv, "ghe.example:8443", "acme", "tools", 2, true, false, true); !strings.Contains(onHost.body, `"number":2`) || strings.Contains(onHost.body, "errors") {
		t.Fatalf("PRTick on the seeded host = %+v", onHost)
	}
	if bare := prTick(t, srv, "ghe.example", "acme", "tools", 2, true, false, true); !strings.Contains(bare.body, `"NOT_FOUND"`) {
		t.Fatalf("PRTick without the port = %+v, want NOT_FOUND", bare)
	}

	// The Host header is compared as given, port included.
	logs := httpRequest(t, srv, "GET", "ghe.example:8443", "/github/rest/repos/acme/tools/actions/jobs/501/logs", "", nil)
	if got := lastInvocation(t, e); logs.status != 200 || !strings.HasSuffix(logs.body, "done") || got.Host != "ghe.example:8443" {
		t.Fatalf("logs on the seeded host = %+v, invocation %+v", logs, got)
	}
	if other := httpRequest(t, srv, "GET", "ghe.example", "/github/rest/repos/acme/tools/actions/jobs/501/logs", "", nil); other.status != 404 || lastInvocation(t, e).Unhandled {
		t.Fatalf("logs without the port = %+v, want the forge's 404", other)
	}

	attachment := httpRequest(t, srv, "GET", "127.0.0.1", "/github/absolute/github.com/user-attachments/assets/abc", "", nil)
	if attachment.status != 200 || attachment.body != "\x89PNG\r\n" {
		t.Fatalf("attachment = %+v", attachment)
	}
	if got := lastInvocation(t, e); got.Path != "https://github.com/user-attachments/assets/abc" || got.Route != "gh api attachment" || got.Host != "github.com" {
		t.Fatalf("attachment invocation = %+v", got)
	}
	// A signed query is matched against the fixture URL, as gh requests the
	// URL as given, but never recorded.
	signed := httpRequest(t, srv, "GET", "127.0.0.1", "/github/absolute/github.com/user-attachments/assets/abc?sig=signed", "", nil)
	if got := lastInvocation(t, e); signed.status != 404 || got.Unhandled || got.Path != "https://github.com/user-attachments/assets/abc" {
		t.Fatalf("signed attachment = %+v, invocation %+v", signed, got)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestHTTPRefusals(t *testing.T) {
	t.Parallel()
	e, _ := seeded(t, Options{})
	srv := httptest.NewServer(e)
	defer srv.Close()
	before := len(e.Invocations(0).Invocations)
	if got := httpRequest(t, srv, "GET", "github.com", "/", "", http.Header{}); got.status != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", got.status)
	}
	if after := len(e.Invocations(0).Invocations); after != before {
		t.Errorf("a request with no token at all (a dev-server probe) was recorded")
	}
	for name, header := range map[string]http.Header{
		"empty bearer":        {"Authorization": {"Bearer "}},
		"wrong bearer":        {"Authorization": {"Bearer fake-token-2"}},
		"wrong private token": {"Private-Token": {"fake-token-2"}},
	} {
		got := httpRequest(t, srv, "GET", "github.com", "/github/rest/user", "", header)
		if got.status != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, got.status)
		}
		if inv := lastInvocation(t, e); !inv.Unhandled || inv.Status != 401 {
			t.Errorf("%s: invocation %+v, want an unhandled 401", name, inv)
		}
	}
	if (New(Options{})).authorized(&http.Request{Header: http.Header{"Authorization": {"Bearer "}}}) {
		t.Error("an engine without a token accepted an empty bearer")
	}

	unknown := httpRequest(t, srv, "DELETE", "github.com", "/github/rest/repos/acme/widgets", "", nil)
	inv := lastInvocation(t, e)
	if unknown.status != 404 || !inv.Unhandled || inv.Status != 404 ||
		!strings.Contains(inv.Detail, "DELETE repos/acme/widgets") || !strings.Contains(unknown.body, inv.Detail) {
		t.Fatalf("unknown route = %+v, invocation %+v", unknown, inv)
	}
	for _, path := range []string{"/elsewhere", "/github/rest/", "/gitlab/api/v3/projects/1"} {
		if got := httpRequest(t, srv, "GET", "github.com", path, "", nil); got.status != 404 || !lastInvocation(t, e).Unhandled {
			t.Errorf("%s = %d, want an unhandled 404", path, got.status)
		}
	}
	// GitLab's review writes have no handler: a request for one is
	// unhandled, not answered as if it changed something. So is a write
	// whose body carries a parameter its handler does not implement.
	if got := httpRequest(t, srv, "POST", "gitlab.com", "/gitlab/api/v4/projects/grp%2Fsub%2Ftool/merge_requests/3/approve", `{"sha":"x","approval_password":"p"}`, nil); got.status != 404 || !lastInvocation(t, e).Unhandled {
		t.Errorf("approve with an unimplemented parameter = %d, want an unhandled 404", got.status)
	}
	for _, write := range [][2]string{
		{"POST", "projects/grp%2Fsub%2Ftool/merge_requests/3/draft_notes"},
		{"POST", "projects/grp%2Fsub%2Ftool/merge_requests/3/discussions/abc/notes"},
		{"PUT", "projects/grp%2Fsub%2Ftool/merge_requests/3/discussions/abc"},
	} {
		if got := httpRequest(t, srv, write[0], "gitlab.com", "/gitlab/api/v4/"+write[1], `{}`, nil); got.status != 404 || !lastInvocation(t, e).Unhandled {
			t.Errorf("%s %s = %d, want an unhandled 404", write[0], write[1], got.status)
		}
	}
	// Range and If-None-Match only on the routes that list them.
	for name, want := range map[string]string{"Range": "range", "If-None-Match": "if-none-match"} {
		header := http.Header{"Authorization": {"Bearer " + testAPIToken}, name: {`"x"`}}
		if got := httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/widgets", "", header); got.status != 404 || !strings.Contains(lastInvocation(t, e).Detail, want) {
			t.Errorf("%s: status %d, want unhandled", name, got.status)
		}
	}
}

// A conditional route answers its 200s with an ETag and a request naming
// the current one with a bodiless 304 under the same route; a changed
// answer has a new ETag.
func TestConditionalRoutesAnswer304ForTheirCurrentETag(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	fixture, err := seedJSON(e, `{"repos":[
		{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","ci":{"id":70,"jobs":[{"id":500,"name":"j","status":"success","log":"done"}]}}]},
		{"forge":"gitlab","project":"g/t","pulls":[{"number":1,"title":"x","ci":{"jobs":[{"id":600,"name":"j","status":"running","log":"step 1"}]}}]}]}`)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	srv := testServer(t, e)
	get := func(forge, path, etag string) httpAnswer {
		header := http.Header{"Authorization": {"Bearer " + testAPIToken}}
		host, prefix := "github.com", "/github/rest/"
		if forge == "gitlab" {
			host, prefix = "gitlab.com", "/gitlab/api/v4/"
		}
		if etag != "" {
			header.Set("If-None-Match", etag)
		}
		return httpRequest(t, srv, "GET", host, prefix+path, "", header)
	}
	for _, tc := range []struct{ forge, path, route string }{
		{"github", "repos/a/b/actions/jobs/500/logs", "gh api job logs"},
		{"github", "repos/a/b/actions/runs/70/jobs?per_page=100", "gh api run jobs"},
		{"gitlab", "projects/g%2Ft/merge_requests/1", "glab api merge request"},
		{"gitlab", "projects/g%2Ft/merge_requests?state=opened&source_branch=feat&per_page=100", "glab api merge request list"},
		{"gitlab", "projects/g%2Ft/jobs/600/trace", "glab api job trace"},
	} {
		first := get(tc.forge, tc.path, "")
		etag := first.header.Get("ETag")
		if first.status != 200 || etag == "" {
			t.Fatalf("%s: first answer %d with ETag %q", tc.route, first.status, etag)
		}
		again := get(tc.forge, tc.path, etag)
		inv := lastInvocation(t, e)
		if again.status != http.StatusNotModified || again.body != "" || again.header.Get("ETag") != etag || inv.Unhandled || inv.Route != tc.route || inv.Status != 304 {
			t.Fatalf("%s: revalidation = %+v, invocation %+v", tc.route, again, inv)
		}
		if stale := get(tc.forge, tc.path, `"stale"`); stale.status != 200 || stale.header.Get("ETag") != etag {
			t.Fatalf("%s: stale ETag answered %+v", tc.route, stale)
		}
	}
	// The trace grows: the old ETag is a full answer with a new one.
	trace := "projects/g%2Ft/jobs/600/trace"
	etag := get("gitlab", trace, "").header.Get("ETag")
	fixture.Repos[1].Pulls[0].CI.Jobs[0].Log = "step 1\nstep 2"
	if _, err := e.Seed(fixture); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if grown := get("gitlab", trace, etag); grown.status != 200 || grown.body != "step 1\nstep 2" || grown.header.Get("ETag") == etag {
		t.Fatalf("grown trace = %+v", grown)
	}
	// A not-found answer carries no ETag.
	if missing := get("gitlab", "projects/g%2Ft/jobs/601/trace", ""); missing.status != 404 || missing.header.Get("ETag") != "" {
		t.Fatalf("missing trace = %+v", missing)
	}
}

// An exhausted pool refuses as its forge does, recorded under route
// "rate limited"; a low one answers with its quota headers; other pools
// are untouched; the limit lifts at its reset and on Reset.
func TestRateLimitedPoolsAnswerAsTheForge(t *testing.T) {
	t.Parallel()
	e, _ := seeded(t, Options{APIToken: testAPIToken})
	srv := testServer(t, e)
	reset := time.Now().Add(time.Hour).Unix()
	for _, bad := range []RateLimit{
		{Forge: "svn", Pool: "core", Reset: reset},
		{Forge: "github", Pool: "search", Reset: reset},
		{Forge: "gitlab", Pool: "core", Reset: reset},
		{Forge: "github", Pool: "core", Remaining: -1, Reset: reset},
		{Forge: "github", Pool: "core", Reset: time.Now().Add(-time.Second).Unix()},
	} {
		if err := e.SetRateLimit(bad); err == nil {
			t.Errorf("SetRateLimit(%+v) accepted", bad)
		}
	}
	resetText := strconv.FormatInt(reset, 10)

	if err := e.SetRateLimit(RateLimit{Forge: "github", Pool: "core", Remaining: 0, Reset: reset}); err != nil {
		t.Fatal(err)
	}
	refused := httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/widgets", "", nil)
	inv := lastInvocation(t, e)
	if refused.status != 403 || refused.header.Get("X-RateLimit-Remaining") != "0" || refused.header.Get("X-RateLimit-Reset") != resetText ||
		refused.header.Get("X-RateLimit-Resource") != "core" || refused.header.Get("X-RateLimit-Limit") != "5000" ||
		!strings.Contains(refused.body, "rate limit") || inv.Route != "rate limited" || inv.Unhandled || inv.Status != 403 {
		t.Fatalf("exhausted core = %+v, invocation %+v", refused, inv)
	}
	if got := prTick(t, srv, "github.com", "acme", "widgets", 7, true, false, false); got.status != 200 || strings.Contains(got.body, "RATE_LIMITED") {
		t.Fatalf("graphql under a core limit = %+v", got)
	}

	if err := e.SetRateLimit(RateLimit{Forge: "github", Pool: "graphql", Remaining: 0, Reset: reset}); err != nil {
		t.Fatal(err)
	}
	if got := prTick(t, srv, "github.com", "acme", "widgets", 7, true, false, false); got.status != 200 || !strings.Contains(got.body, `"type":"RATE_LIMITED"`) ||
		got.header.Get("X-RateLimit-Remaining") != "0" || got.header.Get("X-RateLimit-Resource") != "graphql" || lastInvocation(t, e).Route != "rate limited" {
		t.Fatalf("exhausted graphql = %+v", got)
	}

	if err := e.SetRateLimit(RateLimit{Forge: "gitlab", Pool: "throttle_authenticated_api", Remaining: 0, Reset: reset}); err != nil {
		t.Fatal(err)
	}
	limited := glabGET(t, srv, "gitlab.com", "projects/grp%2Fsub%2Ftool/merge_requests/3")
	retry, _ := strconv.Atoi(limited.header.Get("Retry-After"))
	if limited.status != 429 || limited.header.Get("RateLimit-Remaining") != "0" || limited.header.Get("RateLimit-Reset") != resetText ||
		retry < 3590 || retry > 3600 || lastInvocation(t, e).Route != "rate limited" {
		t.Fatalf("exhausted gitlab = %+v", limited)
	}

	// A low pool answers, reporting what is left.
	if err := e.SetRateLimit(RateLimit{Forge: "github", Pool: "core", Remaining: 100, Reset: reset}); err != nil {
		t.Fatal(err)
	}
	low := httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/widgets", "", nil)
	if low.status != 200 || low.header.Get("X-RateLimit-Remaining") != "100" || low.header.Get("X-RateLimit-Limit") != "5000" || lastInvocation(t, e).Route != "gh repository identity" {
		t.Fatalf("low core = %+v", low)
	}

	// The limit lifts in the second it resets.
	e.mu.Lock()
	_, stillLimited := e.rateLimitLocked(&call{cli: "gh"}, time.Unix(reset, 0).Add(-time.Millisecond))
	_, afterReset := e.rateLimitLocked(&call{cli: "gh"}, time.Unix(reset, 0))
	_, gone := e.rateLimits[rateLimitKey{"github", "core"}]
	e.mu.Unlock()
	if !stillLimited || afterReset || gone {
		t.Fatalf("core before reset %t, at reset %t, kept %t", stillLimited, afterReset, gone)
	}

	e.Reset()
	if _, err := seedJSON(e, testFixture); err != nil {
		t.Fatal(err)
	}
	if got := glabGET(t, srv, "gitlab.com", "projects/grp%2Fsub%2Ftool/merge_requests/3"); got.status != 200 || got.header.Get("RateLimit-Remaining") != "" {
		t.Fatalf("gitlab after Reset = %+v", got)
	}
}

func TestInvocationJSONIsADiscriminatedUnion(t *testing.T) {
	t.Parallel()
	e, _ := seeded(t, Options{})
	srv := testServer(t, e)
	handle(e, "ssh", "-G", "example.com")
	httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/widgets", "", nil)
	prTick(t, srv, "github.com", "acme", "widgets", 7, true, false, false)
	log := e.Invocations(0).Invocations
	records := make([]map[string]any, 3)
	for i, inv := range log[len(log)-3:] {
		raw, err := json.Marshal(inv)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &records[i]); err != nil {
			t.Fatal(err)
		}
	}
	cli, viaHTTP, graphQL := records[0], records[1], records[2]
	for _, key := range []string{"via", "cli", "args", "cwd", "exitCode"} {
		if _, ok := cli[key]; !ok {
			t.Errorf("cli record lacks %s: %v", key, cli)
		}
	}
	for _, key := range []string{"method", "path", "status", "forge", "host"} {
		if _, ok := cli[key]; ok {
			t.Errorf("cli record carries %s", key)
		}
		if _, ok := viaHTTP[key]; !ok {
			t.Errorf("http record lacks %s: %v", key, viaHTTP)
		}
	}
	for _, key := range []string{"cli", "args", "cwd", "exitCode", "stdin", "stderr", "operation", "variables"} {
		if _, ok := viaHTTP[key]; ok {
			t.Errorf("http record carries %s", key)
		}
	}
	if cli["via"] != "cli" || viaHTTP["via"] != "http" || viaHTTP["path"] != "repos/acme/widgets" || viaHTTP["host"] != "github.com" || viaHTTP["status"] != float64(200) {
		t.Fatalf("records = %v / %v", cli, viaHTTP)
	}
	vars, _ := graphQL["variables"].(map[string]any)
	if graphQL["operation"] != "PRTick" || graphQL["path"] != "graphql" || vars["wantDetail"] != true || vars["number"] != float64(7) {
		t.Fatalf("graphql record = %v", graphQL)
	}
}

func TestHTTPOfflineDropsTheConnectionWithoutAReply(t *testing.T) {
	t.Parallel()
	e, _ := seeded(t, Options{})
	srv := httptest.NewServer(e)
	defer srv.Close()
	e.SetOffline(true)
	req, _ := http.NewRequestWithContext(t.Context(), "GET", srv.URL+"/github/rest/user", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIToken)
	resp, err := srv.Client().Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("an offline forge replied %d", resp.StatusCode)
	}
	if inv := lastInvocation(t, e); inv.Route != "offline" || inv.Via != ViaHTTP {
		t.Fatalf("invocation = %+v", inv)
	}
}

// PRTick answers the parts its variables include and nothing else:
// number always, the viewer and detail with $wantDetail, the head
// commit's rollup with $wantChecks, threads and comments with
// $wantThreads.
func TestPRTickHonorsItsIncludeVariables(t *testing.T) {
	t.Parallel()
	e, _ := seeded(t, Options{})
	srv := testServer(t, e)
	for _, parts := range [][3]bool{{false, false, false}, {true, false, false}, {false, true, false}, {false, false, true}, {true, true, true}} {
		detail, threads, checks := parts[0], parts[1], parts[2]
		var got struct {
			Data struct {
				Viewer     map[string]any `json:"viewer"`
				Repository struct {
					PullRequest map[string]any `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		decodeBody(t, prTick(t, srv, "github.com", "acme", "widgets", 7, detail, threads, checks), &got)
		pull := got.Data.Repository.PullRequest
		_, hasTitle := pull["title"]
		_, hasCommits := pull["commits"]
		_, hasThreads := pull["reviewThreads"]
		_, hasComments := pull["comments"]
		if pull["number"] != float64(7) || (got.Data.Viewer != nil) != detail || hasTitle != detail ||
			hasCommits != checks || hasThreads != threads || hasComments != threads {
			t.Errorf("parts %v answered %v (viewer %v)", parts, pull, got.Data.Viewer)
		}
	}
	var checks struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					Commits struct {
						Nodes []struct {
							Commit struct {
								ID                string `json:"id"`
								StatusCheckRollup struct {
									Contexts struct {
										Nodes []map[string]any `json:"nodes"`
									} `json:"contexts"`
								} `json:"statusCheckRollup"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	decodeBody(t, prTick(t, srv, "github.com", "acme", "widgets", 7, false, false, true), &checks)
	commits := checks.Data.Repository.PullRequest.Commits.Nodes
	if len(commits) != 1 || commits[0].Commit.ID == "" || len(commits[0].Commit.StatusCheckRollup.Contexts.Nodes) != 1 {
		t.Fatalf("rollup = %+v", commits)
	}
	run := commits[0].Commit.StatusCheckRollup.Contexts.Nodes[0]
	suite, _ := run["checkSuite"].(map[string]any)
	workflowRun, _ := suite["workflowRun"].(map[string]any)
	workflow, _ := workflowRun["workflow"].(map[string]any)
	if run["__typename"] != "CheckRun" || run["conclusion"] != "SUCCESS" || workflow["name"] != "CI" || run["databaseId"] == nil {
		t.Fatalf("check run = %v", run)
	}
}

// Every connection pages from the cursor its last page handed out: the
// review threads, the conversation comments, one thread's comments and
// the rollup's contexts.
func TestGraphQLPagesEveryConnection(t *testing.T) {
	t.Parallel()
	thread := func(i int) string {
		return fmt.Sprintf(`{"path":"w.go","line":%d,"comments":[{"body":"t%d"}]}`, i+1, i)
	}
	threads := []string{`{"id":"PRRT_long","path":"w.go","line":1,"comments":[` + strings.Repeat(`{"body":"c"},`, 129) + `{"body":"last"}]}`}
	for i := range 129 {
		threads = append(threads, thread(i))
	}
	jobs := make([]string, 0, 120)
	for i := range 120 {
		jobs = append(jobs, fmt.Sprintf(`{"name":"j%d","status":"success"}`, i))
	}
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x",
"comments":[`+strings.TrimSuffix(strings.Repeat(`{"body":"k"},`, 150), ",")+`],
"threads":[`+strings.Join(threads, ",")+`],
"ci":{"jobs":[`+strings.Join(jobs, ",")+`]}}]}]}`); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	type page struct {
		PageInfo struct {
			HasNextPage bool    `json:"hasNextPage"`
			EndCursor   *string `json:"endCursor"`
		} `json:"pageInfo"`
		Nodes []map[string]any `json:"nodes"`
	}
	var tick struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads page `json:"reviewThreads"`
					Comments      page `json:"comments"`
					Commits       struct {
						Nodes []struct {
							Commit struct {
								ID                string `json:"id"`
								StatusCheckRollup struct {
									Contexts page `json:"contexts"`
								} `json:"statusCheckRollup"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	decodeBody(t, prTick(t, srv, "github.com", "a", "b", 1, false, true, true), &tick)
	pull := tick.Data.Repository.PullRequest
	first := pull.ReviewThreads
	long, _ := first.Nodes[0]["comments"].(map[string]any)
	longInfo, _ := long["pageInfo"].(map[string]any)
	if len(first.Nodes) != 100 || !first.PageInfo.HasNextPage || len(pull.Comments.Nodes) != 100 || !pull.Comments.PageInfo.HasNextPage ||
		longInfo["hasNextPage"] != true || len(pull.Commits.Nodes[0].Commit.StatusCheckRollup.Contexts.Nodes) != 100 {
		t.Fatalf("first pages: threads %d comments %d long thread %v", len(first.Nodes), len(pull.Comments.Nodes), longInfo)
	}

	rest := func(op string, vars map[string]any, path ...string) page {
		t.Helper()
		var out map[string]any
		decodeBody(t, graphQL(t, srv, "github.com", op, vars), &out)
		node := out["data"]
		for _, key := range path {
			node = node.(map[string]any)[key]
		}
		raw, _ := json.Marshal(node)
		var p page
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pullVars := func(after *string) map[string]any {
		return map[string]any{"owner": "a", "name": "b", "number": 1, "after": *after}
	}
	if p := rest("PRThreadsPage", pullVars(first.PageInfo.EndCursor), "repository", "pullRequest", "reviewThreads"); len(p.Nodes) != 30 || p.PageInfo.HasNextPage {
		t.Errorf("second threads page = %d nodes, more %v", len(p.Nodes), p.PageInfo.HasNextPage)
	}
	if p := rest("PRCommentsPage", pullVars(pull.Comments.PageInfo.EndCursor), "repository", "pullRequest", "comments"); len(p.Nodes) != 50 || p.PageInfo.HasNextPage {
		t.Errorf("second comments page = %d nodes", len(p.Nodes))
	}
	p := rest("ThreadCommentsPage", map[string]any{"threadID": "PRRT_long", "after": longInfo["endCursor"]}, "node", "comments")
	if len(p.Nodes) != 30 || p.Nodes[29]["body"] != "last" || p.PageInfo.HasNextPage {
		t.Errorf("second thread comments page = %d nodes", len(p.Nodes))
	}
	commit := pull.Commits.Nodes[0].Commit
	p = rest("RollupContextsPage", map[string]any{"commitID": commit.ID, "number": 1, "after": *commit.StatusCheckRollup.Contexts.PageInfo.EndCursor}, "node", "statusCheckRollup", "contexts")
	if len(p.Nodes) != 20 || p.Nodes[19]["name"] != "j119" {
		t.Errorf("second contexts page = %d nodes", len(p.Nodes))
	}
	for _, id := range []map[string]any{{"threadID": "PRRT_none", "after": "cursor:0"}} {
		if got := graphQL(t, srv, "github.com", "ThreadCommentsPage", id); !strings.Contains(got.body, `"node":null`) || !strings.Contains(got.body, "NOT_FOUND") {
			t.Errorf("unknown thread = %s", got.body)
		}
	}
	if got := graphQL(t, srv, "github.com", "RollupContextsPage", map[string]any{"commitID": "C_ao1_1", "number": 1, "after": "cursor:0"}); !strings.Contains(got.body, "NOT_FOUND") {
		t.Errorf("unknown commit = %s", got.body)
	}
	for _, inv := range e.Invocations(0).Invocations {
		if inv.Unhandled {
			t.Errorf("unhandled %+v", inv)
		}
	}
}

// A request whose operation the fake does not implement as asked is
// unhandled: an unknown operation, a document declaring other variables
// or another kind, a variable the document does not declare, a missing
// or mistyped one, a foreign cursor, a GET.
func TestGraphQLRefusesADriftedOperation(t *testing.T) {
	t.Parallel()
	e, _ := seeded(t, Options{})
	srv := testServer(t, e)
	tick := map[string]any{"owner": "acme", "name": "widgets", "number": 7, "wantDetail": true, "wantThreads": false, "wantEvents": false, "wantChecks": false}
	send := func(query, op string, vars map[string]any) httpAnswer {
		return httpRequest(t, srv, "POST", "github.com", "/github/graphql", mustJSON(t, map[string]any{"query": query, "operationName": op, "variables": vars}), nil)
	}
	doc := graphQLDocument(t, "PRTick")
	cases := map[string]httpAnswer{
		"unknown operation":   send(`query Viewer { viewer { login } }`, "Viewer", nil),
		"another kind":        send(strings.Replace(doc, "query", "mutation", 1), "PRTick", tick),
		"another name":        send(strings.Replace(doc, "PRTick", "PRRead", 1), "PRTick", tick),
		"undeclared variable": send(doc, "PRTick", map[string]any{"owner": "acme", "name": "widgets", "number": 7, "wantDetail": true, "wantThreads": false, "wantEvents": false, "wantChecks": false, "wantCI": true}),
		"missing variable":    send(doc, "PRTick", map[string]any{"owner": "acme", "name": "widgets", "number": 7, "wantDetail": true, "wantThreads": false}),
		"declared extra":      send(strings.Replace(doc, "(", "($wantCI: Boolean!, ", 1), "PRTick", tick),
		"mistyped variable":   send(doc, "PRTick", map[string]any{"owner": "acme", "name": "widgets", "number": "7", "wantDetail": true, "wantThreads": false, "wantEvents": false, "wantChecks": false}),
		"foreign cursor":      graphQL(t, srv, "github.com", "PRThreadsPage", map[string]any{"owner": "acme", "name": "widgets", "number": 7, "after": "Y3Vyc29y"}),
		"no operation name":   send(doc, "", tick),
		"GET":                 httpRequest(t, srv, "GET", "github.com", "/github/graphql", "", nil),
	}
	for name, got := range cases {
		if got.status != 404 || !strings.Contains(got.body, "unhandled") {
			t.Errorf("%s = %+v, want an unhandled 404", name, got)
		}
	}
	if got := len(e.Invocations(0).Invocations); got != len(cases) {
		t.Fatalf("recorded %d invocations, want %d", got, len(cases))
	}
}

// SetThreadResolved changes the thread's state, which the next read
// shows, and answers only the alias $resolved selects.
func TestSetThreadResolvedChangesTheThread(t *testing.T) {
	t.Parallel()
	e, fixture := seeded(t, Options{})
	srv := testServer(t, e)
	id := fixture.Repos[0].Pulls[0].Threads[0].ID
	resolved := func() bool {
		var got struct {
			Data struct {
				Repository struct {
					PullRequest struct {
						ReviewThreads struct {
							Nodes []struct {
								IsResolved bool `json:"isResolved"`
							} `json:"nodes"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		decodeBody(t, prTick(t, srv, "github.com", "acme", "widgets", 7, false, true, false), &got)
		return got.Data.Repository.PullRequest.ReviewThreads.Nodes[0].IsResolved
	}
	if resolved() {
		t.Fatal("the seeded thread starts resolved")
	}
	if got := graphQL(t, srv, "github.com", "SetThreadResolved", map[string]any{"threadID": id, "resolved": true}); got.body != `{"data":{"resolve":{"thread":{"isResolved":true}}}}`+"\n" {
		t.Fatalf("resolve = %q", got.body)
	}
	if !resolved() {
		t.Fatal("the resolve did not change the thread")
	}
	if got := graphQL(t, srv, "github.com", "SetThreadResolved", map[string]any{"threadID": id, "resolved": false}); got.body != `{"data":{"unresolve":{"thread":{"isResolved":false}}}}`+"\n" {
		t.Fatalf("unresolve = %q", got.body)
	}
	if resolved() {
		t.Fatal("the unresolve did not change the thread")
	}
	if got := graphQL(t, srv, "ghe.example", "SetThreadResolved", map[string]any{"threadID": id, "resolved": true}); !strings.Contains(got.body, "NOT_FOUND") || resolved() {
		t.Fatalf("a thread on another host = %q", got.body)
	}
}

// PRStates answers pulls by node id with their state and merge time; an id
// that does not resolve on the request's host is null beside a NOT_FOUND
// error. A default node id survives a reseed and a new engine.
func TestGitHubPRStates(t *testing.T) {
	t.Parallel()
	const fixture = `{"repos":[{"forge":"github","project":"a/b","pulls":[
{"number":1,"title":"open"},
{"number":2,"title":"closed","state":"closed"},
{"number":3,"title":"merged","state":"merged","mergedAt":"2026-10-13T12:00:00Z"}]}]}`
	e := New(Options{APIToken: testAPIToken})
	seededFixture, err := seedJSON(e, fixture)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int]string{}
	for _, p := range seededFixture.Repos[0].Pulls {
		ids[p.Number] = p.NodeID
	}
	again := New(Options{APIToken: testAPIToken})
	reseeded, err := seedJSON(again, fixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range reseeded.Repos[0].Pulls {
		if p.NodeID == "" || p.NodeID != ids[p.Number] {
			t.Fatalf("pull #%d node id %q in a new engine, want %q", p.Number, p.NodeID, ids[p.Number])
		}
	}
	srv := testServer(t, e)
	answer := graphQL(t, srv, "github.com", "PRStates", map[string]any{"ids": []string{ids[3], ids[2], ids[1], "PR_missing"}})
	var got struct {
		Data struct {
			Nodes []map[string]any `json:"nodes"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	decodeBody(t, answer, &got)
	nodes := got.Data.Nodes
	if len(nodes) != 4 || nodes[3] != nil {
		t.Fatalf("nodes = %+v", nodes)
	}
	if nodes[0]["state"] != "MERGED" || nodes[0]["mergedAt"] != "2026-10-13T12:00:00Z" || nodes[0]["number"] != float64(3) {
		t.Fatalf("merged node = %+v", nodes[0])
	}
	if nodes[1]["state"] != "CLOSED" || nodes[1]["mergedAt"] != nil || nodes[2]["state"] != "OPEN" {
		t.Fatalf("closed and open nodes = %+v %+v", nodes[1], nodes[2])
	}
	if len(got.Errors) != 1 || got.Errors[0]["type"] != "NOT_FOUND" {
		t.Fatalf("errors = %+v, want one NOT_FOUND", got.Errors)
	}
	if inv := lastInvocation(t, e); inv.Route != "gh graphql PRStates" || inv.Unhandled {
		t.Fatalf("invocation = %+v", inv)
	}
	other := graphQL(t, srv, "ghe.example", "PRStates", map[string]any{"ids": []string{ids[1]}})
	if !strings.Contains(other.body, `"nodes":[null]`) || !strings.Contains(other.body, "NOT_FOUND") {
		t.Fatalf("a pull on another host = %s", other.body)
	}
	if empty := graphQL(t, srv, "github.com", "PRStates", map[string]any{"ids": []string{}}); empty.status != 404 {
		t.Fatalf("no ids = %+v, want unhandled", empty)
	}
	if _, err := seedJSON(New(Options{}), `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","mergedAt":"2026-10-13T12:00:00Z"}]}]}`); err == nil {
		t.Fatal("an open pull with mergedAt was accepted")
	}
	if _, err := seedJSON(New(Options{}), `{"repos":[{"forge":"gitlab","project":"g/r","pulls":[{"number":1,"title":"x","nodeId":"PR_x"}]}]}`); err == nil {
		t.Fatal("a GitLab merge request with a node id was accepted")
	}
}

// The merge request list answers named iids in any state with merged_at,
// and refuses iids beside a filter it does not implement.
func TestGitLabMergeRequestsByIID(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, `{"repos":[{"forge":"gitlab","project":"g/r","pulls":[
{"number":1,"title":"open"},
{"number":2,"title":"closed","state":"closed"},
{"number":3,"title":"merged","state":"merged","mergedAt":"2026-10-13T12:00:00Z"}]}]}`); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	list := httpRequest(t, srv, "GET", "gitlab.com", "/gitlab/api/v4/projects/g%2Fr/merge_requests?iids%5B%5D=3&iids%5B%5D=2&iids%5B%5D=9&per_page=100&state=all", "", nil)
	var got []map[string]any
	decodeBody(t, list, &got)
	if len(got) != 2 || got[0]["iid"] != float64(3) || got[0]["state"] != "merged" || got[0]["merged_at"] != "2026-10-13T12:00:00Z" ||
		got[1]["iid"] != float64(2) || got[1]["state"] != "closed" || got[1]["merged_at"] != nil {
		t.Fatalf("merge requests by iid = %+v", got)
	}
	if inv := lastInvocation(t, e); inv.Route != "glab api merge request list" || inv.Unhandled {
		t.Fatalf("invocation = %+v", inv)
	}
	if opened := httpRequest(t, srv, "GET", "gitlab.com", "/gitlab/api/v4/projects/g%2Fr/merge_requests?iids%5B%5D=1&state=opened", "", nil); opened.status != 404 {
		t.Fatalf("iids with state=opened = %+v, want unhandled", opened)
	}
}

// source_branch matches a fork's merge request from a branch of the same
// name, which answers the fork's own source_project_id; the simple view is
// not implemented.
func TestGitLabOpenMergeRequestsIncludeForks(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, `{"repos":[{"forge":"gitlab","project":"g/r","id":77,"pulls":[
{"number":1,"title":"own","headRef":"topic"},
{"number":2,"title":"fork","headRef":"topic","headRepo":"stranger/r"}]}]}`); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	list := httpRequest(t, srv, "GET", "gitlab.com", "/gitlab/api/v4/projects/g%2Fr/merge_requests?per_page=100&source_branch=topic&state=opened", "", nil)
	var got []map[string]any
	decodeBody(t, list, &got)
	if len(got) != 2 || got[0]["iid"] != float64(2) || got[0]["project_id"] != float64(77) || got[0]["source_project_id"] == float64(77) ||
		got[1]["iid"] != float64(1) || got[1]["project_id"] != float64(77) || got[1]["source_project_id"] != float64(77) {
		t.Fatalf("open merge requests = %+v", got)
	}
	if simple := httpRequest(t, srv, "GET", "gitlab.com", "/gitlab/api/v4/projects/g%2Fr/merge_requests?source_branch=topic&state=opened&view=simple", "", nil); simple.status != 404 {
		t.Fatalf("view=simple = %+v, want unhandled", simple)
	}
}

// OpenPRsByHead lists a head's open pulls; MergedPRs pages merged ones
// by first and after.
func TestGitHubPullLists(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, `{"repos":[{"forge":"github","project":"a/b","pulls":[
{"number":1,"title":"open","headRef":"topic","nodeId":"PR_one"},
{"number":2,"title":"closed","headRef":"topic","state":"closed"},
{"number":3,"title":"m1","headRef":"h1","state":"merged","headSha":"`+strings.Repeat("1", 40)+`"},
{"number":4,"title":"m2","headRef":"h2","state":"merged"},
{"number":5,"title":"m3","headRef":"h3","state":"merged"},
{"number":6,"title":"fork","headRef":"topic","headRepo":"stranger/b","nodeId":"PR_fork"}]}]}`); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	open := graphQL(t, srv, "github.com", "OpenPRsByHead", map[string]any{"owner": "a", "name": "b", "head": "topic"})
	if !strings.Contains(open.body, `"nodes":[{"id":"PR_one","isCrossRepository":false,"number":1,"state":"OPEN","title":"open","url":"https://github.com/a/b/pull/1"},`+
		`{"id":"PR_fork","isCrossRepository":true,"number":6,"state":"OPEN","title":"fork","url":"https://github.com/a/b/pull/6"}]`) ||
		!strings.Contains(open.body, `"pageInfo":{"endCursor":"cursor:2","hasNextPage":false}`) {
		t.Fatalf("open = %s", open.body)
	}
	if _, err := seedJSON(e, `{"repos":[{"forge":"github","project":"a/c","pulls":[{"number":1,"title":"self","headRepo":"a/c"}]}]}`); err == nil {
		t.Fatal("a pull whose headRepo is its own repository was seeded")
	}
	if none := graphQL(t, srv, "github.com", "OpenPRsByHead", map[string]any{"owner": "a", "name": "b", "head": "other"}); !strings.Contains(none.body, `"nodes":[]`) {
		t.Fatalf("no open pull = %s", none.body)
	}
	type merged struct {
		Data struct {
			Repository struct {
				PullRequests struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []map[string]any `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		} `json:"data"`
	}
	var page1, page2 merged
	decodeBody(t, graphQL(t, srv, "github.com", "MergedPRs", map[string]any{"owner": "a", "name": "b", "first": 2}), &page1)
	got := page1.Data.Repository.PullRequests
	if len(got.Nodes) != 2 || !got.PageInfo.HasNextPage || got.Nodes[0]["headRefName"] != "h1" || got.Nodes[0]["headRefOid"] != strings.Repeat("1", 40) {
		t.Fatalf("merged page 1 = %+v", got)
	}
	decodeBody(t, graphQL(t, srv, "github.com", "MergedPRs", map[string]any{"owner": "a", "name": "b", "first": 2, "after": got.PageInfo.EndCursor}), &page2)
	if rest := page2.Data.Repository.PullRequests; len(rest.Nodes) != 1 || rest.PageInfo.HasNextPage || rest.Nodes[0]["headRefName"] != "h3" {
		t.Fatalf("merged page 2 = %+v", rest)
	}
	if over := graphQL(t, srv, "github.com", "MergedPRs", map[string]any{"owner": "a", "name": "b", "first": 101}); over.status != 404 {
		t.Fatalf("first over 100 = %+v, want unhandled", over)
	}
}

// mergeFixture is a GitHub repository and a GitLab project whose pulls
// the merge routes act on; the viewer is octo, neither pull's author.
const mergeFixture = `{
  "viewer": "octo",
  "repos": [
    {"forge": "github", "project": "acme/merge", "merge": {"autoMergeAllowed": true},
     "pulls": [
       {"number": 1, "title": "Plain", "headRef": "plain"},
       {"number": 2, "title": "Guarded", "headRef": "guarded", "headSha": "2222222222222222222222222222222222222222",
        "readiness": {"requiredApprovals": 1, "requiredChecks": ["unit", "e2e"], "resolveThreads": true},
        "threads": [{"path": "a.go", "line": 1, "comments": [{"body": "fix"}]}],
        "ci": {"jobs": [{"name": "unit", "status": "running"}, {"name": "lint", "status": "failed"}]}},
       {"number": 3, "title": "Merged", "state": "merged", "headRef": "done"}
     ]},
    {"forge": "gitlab", "project": "grp/merge", "id": 7700, "merge": {"pipelineMustSucceed": true},
     "pulls": [
       {"number": 1, "title": "Plain", "headRef": "plain"},
       {"number": 2, "title": "Guarded", "headRef": "guarded", "headSha": "3333333333333333333333333333333333333333",
        "readiness": {"requiredApprovals": 1}, "ci": {"jobs": [{"name": "unit", "status": "running"}]}},
       {"number": 3, "title": "draft: Drafty", "headRef": "drafty"}
     ]}
  ]
}`

func mergeEngine(t *testing.T) (*Engine, *httptest.Server) {
	t.Helper()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, mergeFixture); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	return e, testServer(t, e)
}

func ghTick(t *testing.T, srv *httptest.Server, number int) map[string]any {
	t.Helper()
	var out struct {
		Data struct {
			Repository map[string]any `json:"repository"`
		} `json:"data"`
	}
	decodeBody(t, graphQL(t, srv, "github.com", "PRTick", map[string]any{
		"owner": "acme", "name": "merge", "number": number, "wantDetail": true, "wantThreads": false, "wantEvents": true, "wantChecks": true,
	}), &out)
	pull := out.Data.Repository["pullRequest"].(map[string]any)
	pull["repository"] = out.Data.Repository
	return pull
}

func glabJSON(t *testing.T, srv *httptest.Server, path string) map[string]any {
	t.Helper()
	var out map[string]any
	decodeBody(t, glabGET(t, srv, "gitlab.com", path), &out)
	return out
}

func glabSend(t *testing.T, srv *httptest.Server, method, path, body string) httpAnswer {
	t.Helper()
	return httpRequest(t, srv, method, "gitlab.com", "/gitlab/api/v4/"+path, body, http.Header{"Private-Token": {testAPIToken}})
}

// An open pull with nothing required reads as ready on both forges, as on
// an unprotected branch; the requirements a seed declares block it the
// way each forge reports them.
func TestMergeReadinessDefaultsAndRequirements(t *testing.T) {
	t.Parallel()
	_, srv := mergeEngine(t)
	plain := ghTick(t, srv, 1)
	repo := plain["repository"].(map[string]any)
	if plain["mergeStateStatus"] != "CLEAN" || plain["reviewDecision"] != nil || plain["autoMergeRequest"] != nil ||
		plain["viewerCanEnableAutoMerge"] != true || plain["merged"] != false || plain["mergedAt"] != nil || plain["closedAt"] != nil ||
		plain["baseRef"].(map[string]any)["refUpdateRule"] != nil || repo["viewerPermission"] != "WRITE" || repo["viewerDefaultMergeMethod"] != "MERGE" ||
		repo["mergeCommitAllowed"] != true || repo["squashMergeAllowed"] != true || repo["rebaseMergeAllowed"] != true || repo["autoMergeAllowed"] != true {
		t.Fatalf("plain pull = %v", plain)
	}
	if items := plain["timelineItems"].(map[string]any); len(items["nodes"].([]any)) != 0 || items["pageInfo"].(map[string]any)["hasPreviousPage"] != false {
		t.Fatalf("plain timeline = %v", items)
	}
	guarded := ghTick(t, srv, 2)
	rule := guarded["baseRef"].(map[string]any)["refUpdateRule"].(map[string]any)
	if guarded["mergeStateStatus"] != "BLOCKED" || guarded["reviewDecision"] != "REVIEW_REQUIRED" ||
		rule["requiredApprovingReviewCount"] != float64(1) || rule["requiresConversationResolution"] != true {
		t.Fatalf("guarded pull = %v", guarded)
	}
	contexts := guarded["commits"].(map[string]any)["nodes"].([]any)[0].(map[string]any)["commit"].(map[string]any)["statusCheckRollup"].(map[string]any)["contexts"].(map[string]any)["nodes"].([]any)
	if contexts[0].(map[string]any)["isRequired"] != true || contexts[1].(map[string]any)["isRequired"] != false {
		t.Fatalf("contexts = %v", contexts)
	}
	merged := ghTick(t, srv, 3)
	nodes := merged["timelineItems"].(map[string]any)["nodes"].([]any)
	if merged["mergeStateStatus"] != "UNKNOWN" || merged["merged"] != true || merged["mergedBy"].(map[string]any)["login"] != "ao-author" ||
		len(nodes) != 2 || nodes[0].(map[string]any)["__typename"] != "MergedEvent" || nodes[1].(map[string]any)["__typename"] != "ClosedEvent" {
		t.Fatalf("merged pull = %v", merged)
	}

	if mr := glabJSON(t, srv, "projects/grp%2Fmerge/merge_requests/1"); mr["detailed_merge_status"] != "ci_must_pass" {
		// A project that requires a pipeline blocks a merge request without one.
		t.Fatalf("plain merge request = %v", mr)
	}
	mr := glabJSON(t, srv, "projects/grp%2Fmerge/merge_requests/2")
	if mr["detailed_merge_status"] != "ci_still_running" || mr["user"].(map[string]any)["can_merge"] != true || mr["merge_when_pipeline_succeeds"] != false ||
		mr["merged_by"].(map[string]any)["username"] != nil || mr["project_id"] != float64(7700) || mr["source_project_id"] != float64(7700) || mr["blocking_discussions_resolved"] != true {
		t.Fatalf("guarded merge request = %v", mr)
	}
	approvals := glabJSON(t, srv, "projects/grp%2Fmerge/merge_requests/2/approvals")
	if approvals["approvals_required"] != float64(1) || approvals["approvals_left"] != float64(1) || approvals["user_can_approve"] != true || approvals["user_has_approved"] != false {
		t.Fatalf("approvals = %v", approvals)
	}
	if mr := glabJSON(t, srv, "projects/grp%2Fmerge/merge_requests/3"); mr["draft"] != true || mr["detailed_merge_status"] != "draft_status" {
		t.Fatalf("a draft title = %v", mr)
	}
	project := glabJSON(t, srv, "projects/grp%2Fmerge")
	if project["merge_method"] != "merge" || project["squash_option"] != "default_off" || project["only_allow_merge_if_pipeline_succeeds"] != true ||
		project["permissions"].(map[string]any)["project_access"].(map[string]any)["access_level"] != float64(30) {
		t.Fatalf("project = %v", project)
	}
	// GitLab's project read is conditional, as GitLab's is.
	first := glabGET(t, srv, "gitlab.com", "projects/grp%2Fmerge")
	again := httpRequest(t, srv, "GET", "gitlab.com", "/gitlab/api/v4/projects/grp%2Fmerge", "", http.Header{"Private-Token": {testAPIToken}, "If-None-Match": {first.header.Get("ETag")}})
	if first.header.Get("ETag") == "" || again.status != http.StatusNotModified {
		t.Fatalf("project revalidation = %d (ETag %q)", again.status, first.header.Get("ETag"))
	}
}

// GitHub's merge mutations transition the pull and the next PRTick shows
// it, each with its timeline item; a wrong expectedHeadOid answers
// GitHub's own refusal and changes nothing.
func TestGitHubMergeMutationsTransitionThePull(t *testing.T) {
	t.Parallel()
	e, srv := mergeEngine(t)
	plain := ghTick(t, srv, 1)
	id, head := plain["id"], plain["headRefOid"]
	if id != nil {
		t.Fatalf("PRTick selects no id: %v", id)
	}
	var target struct {
		Data struct {
			Repository struct {
				DeleteBranchOnMerge bool `json:"deleteBranchOnMerge"`
				PullRequest         struct {
					ID          string `json:"id"`
					HeadRefName string `json:"headRefName"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	decodeBody(t, graphQL(t, srv, "github.com", "PRMergeTarget", map[string]any{"owner": "acme", "name": "merge", "number": 1}), &target)
	node := target.Data.Repository.PullRequest.ID
	if node == "" || target.Data.Repository.PullRequest.HeadRefName != "plain" {
		t.Fatalf("PRMergeTarget = %+v", target)
	}

	stale := graphQL(t, srv, "github.com", "MergePR", map[string]any{"pullRequestID": node, "method": "MERGE", "expectedHeadOid": strings.Repeat("f", 40)})
	if !strings.Contains(stale.body, "Head branch was modified. Review and try the merge again.") || !strings.Contains(stale.body, `"mergePullRequest":null`) {
		t.Fatalf("a stale merge = %s", stale.body)
	}
	if again := ghTick(t, srv, 1); again["merged"] != false || len(again["timelineItems"].(map[string]any)["nodes"].([]any)) != 0 {
		t.Fatalf("a stale merge changed the pull: %v", again)
	}
	// A clean pull cannot be armed; auto-merge needs something to wait for.
	if got := graphQL(t, srv, "github.com", "EnableAutoMerge", map[string]any{"pullRequestID": node, "method": "SQUASH", "expectedHeadOid": head}); !strings.Contains(got.body, "clean status") {
		t.Fatalf("auto-merge of a clean pull = %s", got.body)
	}
	merged := graphQL(t, srv, "github.com", "MergePR", map[string]any{"pullRequestID": node, "method": "REBASE", "expectedHeadOid": head})
	if !strings.Contains(merged.body, `"merged":true`) {
		t.Fatalf("merge = %s", merged.body)
	}
	after := ghTick(t, srv, 1)
	nodes := after["timelineItems"].(map[string]any)["nodes"].([]any)
	if after["state"] != "MERGED" || after["mergedBy"].(map[string]any)["login"] != "octo" || after["mergedAt"] == nil || after["closedAt"] == nil ||
		len(nodes) != 2 || nodes[0].(map[string]any)["__typename"] != "MergedEvent" || nodes[1].(map[string]any)["__typename"] != "ClosedEvent" {
		t.Fatalf("after merge = %v", after)
	}
	if got := graphQL(t, srv, "github.com", "MergePR", map[string]any{"pullRequestID": node, "method": "MERGE", "expectedHeadOid": head}); !strings.Contains(got.body, "already merged") {
		t.Fatalf("a second merge = %s", got.body)
	}
	// The branch delete: 204 once, then GitHub's 422.
	del := func() httpAnswer {
		return httpRequest(t, srv, "DELETE", "github.com", "/github/rest/repos/acme/merge/git/refs/heads/plain", "", nil)
	}
	if got := del(); got.status != http.StatusNoContent {
		t.Fatalf("delete = %+v", got)
	}
	if got := del(); got.status != http.StatusUnprocessableEntity || !strings.Contains(got.body, "Reference does not exist") || lastInvocation(t, e).Route != "gh api delete branch" {
		t.Fatalf("a second delete = %+v", got)
	}

	// The guarded pull: approve over REST, dismiss over GraphQL, arm and
	// disarm auto-merge, each read back.
	guarded := func() map[string]any { return ghTick(t, srv, 2) }
	review := httpRequest(t, srv, "POST", "github.com", "/github/rest/repos/acme/merge/pulls/2/reviews", `{"event":"APPROVE","commit_id":"2222222222222222222222222222222222222222"}`, nil)
	if review.status != 200 || !strings.Contains(review.body, `"state":"APPROVED"`) {
		t.Fatalf("approve = %+v", review)
	}
	pull := guarded()
	opinions := pull["latestOpinionatedReviews"].(map[string]any)["nodes"].([]any)
	if pull["reviewDecision"] != "APPROVED" || len(opinions) != 1 || opinions[0].(map[string]any)["viewerDidAuthor"] != true || opinions[0].(map[string]any)["authorCanPushToRepository"] != true {
		t.Fatalf("after approve = %v", pull)
	}
	if nodes := pull["timelineItems"].(map[string]any)["nodes"].([]any); len(nodes) != 1 || nodes[0].(map[string]any)["state"] != "APPROVED" {
		t.Fatalf("after approve: timeline %v", nodes)
	}
	reviewID := opinions[0].(map[string]any)["id"].(string)
	if got := graphQL(t, srv, "github.com", "ViewerApproval", map[string]any{"owner": "acme", "name": "merge", "number": 2}); !strings.Contains(got.body, reviewID) {
		t.Fatalf("ViewerApproval = %s", got.body)
	}
	dismissed := graphQL(t, srv, "github.com", "DismissReview", map[string]any{"reviewID": reviewID, "message": "withdrawn"})
	if !strings.Contains(dismissed.body, `"state":"DISMISSED"`) {
		t.Fatalf("dismiss = %s", dismissed.body)
	}
	pull = guarded()
	nodes = pull["timelineItems"].(map[string]any)["nodes"].([]any)
	if pull["reviewDecision"] != "REVIEW_REQUIRED" || len(pull["latestOpinionatedReviews"].(map[string]any)["nodes"].([]any)) != 0 ||
		len(nodes) != 2 || nodes[0].(map[string]any)["state"] != "DISMISSED" || nodes[1].(map[string]any)["dismissalMessage"] != "withdrawn" {
		t.Fatalf("after dismiss = %v", pull)
	}
	decodeBody(t, graphQL(t, srv, "github.com", "PRMergeTarget", map[string]any{"owner": "acme", "name": "merge", "number": 2}), &target)
	node = target.Data.Repository.PullRequest.ID
	if got := graphQL(t, srv, "github.com", "MergePR", map[string]any{"pullRequestID": node, "method": "MERGE", "expectedHeadOid": "2222222222222222222222222222222222222222"}); !strings.Contains(got.body, "not mergeable") {
		t.Fatalf("merge of a blocked pull = %s", got.body)
	}
	armed := graphQL(t, srv, "github.com", "EnableAutoMerge", map[string]any{"pullRequestID": node, "method": "SQUASH", "expectedHeadOid": "2222222222222222222222222222222222222222"})
	if !strings.Contains(armed.body, `"enabledAt"`) {
		t.Fatalf("auto-merge = %s", armed.body)
	}
	pull = guarded()
	request := pull["autoMergeRequest"].(map[string]any)
	if request["mergeMethod"] != "SQUASH" || request["enabledBy"].(map[string]any)["login"] != "octo" || pull["viewerCanEnableAutoMerge"] != false || pull["viewerCanDisableAutoMerge"] != true {
		t.Fatalf("armed = %v", pull)
	}
	if nodes := pull["timelineItems"].(map[string]any)["nodes"].([]any); nodes[len(nodes)-1].(map[string]any)["__typename"] != "AutoSquashEnabledEvent" {
		t.Fatalf("armed timeline = %v", nodes)
	}
	if got := graphQL(t, srv, "github.com", "DisableAutoMerge", map[string]any{"pullRequestID": node}); !strings.Contains(got.body, `"autoMergeRequest":null`) {
		t.Fatalf("disarm = %s", got.body)
	}
	pull = guarded()
	if nodes := pull["timelineItems"].(map[string]any)["nodes"].([]any); pull["autoMergeRequest"] != nil || nodes[len(nodes)-1].(map[string]any)["__typename"] != "AutoMergeDisabledEvent" {
		t.Fatalf("disarmed = %v", pull)
	}
	if got := graphQL(t, srv, "github.com", "DisableAutoMerge", map[string]any{"pullRequestID": node}); !strings.Contains(got.body, "not enabled") {
		t.Fatalf("a second disarm = %s", got.body)
	}
	// Review line comments are not implemented; an unknown pull is not found.
	if got := httpRequest(t, srv, "POST", "github.com", "/github/rest/repos/acme/merge/pulls/2/reviews", `{"event":"COMMENT","body":"x","comments":[{"path":"a.go","line":1,"body":"y"}]}`, nil); got.status != 404 || !lastInvocation(t, e).Unhandled {
		t.Fatalf("a review with comments = %+v", got)
	}
	if got := graphQL(t, srv, "github.com", "MarkPRReady", map[string]any{"pullRequestID": "PR_nope"}); !strings.Contains(got.body, "NOT_FOUND") || lastInvocation(t, e).Unhandled {
		t.Fatalf("an unknown pull = %s", got.body)
	}
	if got := httpRequest(t, srv, "GET", "github.com", "/github/rest/user", "", nil); !strings.Contains(got.body, `"login":"octo"`) {
		t.Fatalf("user = %+v", got)
	}
}

// The timeline is read newest page first and backwards with before; the
// thread states page forwards.
func TestGitHubTimelineAndThreadStatesPage(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	events := make([]map[string]any, 150)
	for i := range events {
		events[i] = map[string]any{"kind": "review_requested", "ref": fmt.Sprintf("r%d", i), "at": fmt.Sprintf("2026-01-01T%02d:%02d:00Z", i/60, i%60)}
	}
	threads := make([]map[string]any, 120)
	for i := range threads {
		threads[i] = map[string]any{"path": "a.go", "line": 1, "resolved": i != 110, "comments": []map[string]any{{"body": "x"}}}
	}
	fixture := mustJSON(t, map[string]any{"repos": []map[string]any{{"forge": "github", "project": "a/b", "pulls": []map[string]any{{"number": 1, "title": "x", "events": events, "threads": threads}}}}})
	if _, err := seedJSON(e, fixture); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	var tick struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					TimelineItems struct {
						PageInfo struct {
							HasPreviousPage bool   `json:"hasPreviousPage"`
							StartCursor     string `json:"startCursor"`
						} `json:"pageInfo"`
						Nodes []map[string]any `json:"nodes"`
					} `json:"timelineItems"`
					ThreadStates struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []map[string]any `json:"nodes"`
					} `json:"threadStates"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	decodeBody(t, graphQL(t, srv, "github.com", "PRTick", map[string]any{"owner": "a", "name": "b", "number": 1, "wantDetail": true, "wantThreads": false, "wantEvents": true, "wantChecks": false}), &tick)
	items := tick.Data.Repository.PullRequest.TimelineItems
	if len(items.Nodes) != 100 || !items.PageInfo.HasPreviousPage || items.Nodes[99]["requestedReviewer"].(map[string]any)["login"] != "r149" {
		t.Fatalf("newest timeline page = %d nodes, previous %v", len(items.Nodes), items.PageInfo.HasPreviousPage)
	}
	var page struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					TimelineItems struct {
						PageInfo struct {
							HasPreviousPage bool `json:"hasPreviousPage"`
						} `json:"pageInfo"`
						Nodes []map[string]any `json:"nodes"`
					} `json:"timelineItems"`
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool `json:"hasNextPage"`
						} `json:"pageInfo"`
						Nodes []map[string]any `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	decodeBody(t, graphQL(t, srv, "github.com", "PRTimelinePage", map[string]any{"owner": "a", "name": "b", "number": 1, "before": items.PageInfo.StartCursor}), &page)
	older := page.Data.Repository.PullRequest.TimelineItems
	if len(older.Nodes) != 50 || older.PageInfo.HasPreviousPage || older.Nodes[0]["requestedReviewer"].(map[string]any)["login"] != "r0" {
		t.Fatalf("older timeline page = %d nodes", len(older.Nodes))
	}
	states := tick.Data.Repository.PullRequest.ThreadStates
	if len(states.Nodes) != 100 || !states.PageInfo.HasNextPage {
		t.Fatalf("thread states = %d nodes", len(states.Nodes))
	}
	decodeBody(t, graphQL(t, srv, "github.com", "PRThreadStatesPage", map[string]any{"owner": "a", "name": "b", "number": 1, "after": states.PageInfo.EndCursor}), &page)
	if rest := page.Data.Repository.PullRequest.ReviewThreads; len(rest.Nodes) != 20 || rest.PageInfo.HasNextPage || rest.Nodes[10]["isResolved"] != false {
		t.Fatalf("second thread states page = %+v", rest)
	}
}

// GitLab's merge routes transition the merge request and the next read
// shows it; a wrong sha answers 409 and changes nothing.
func TestGitLabMergeRoutesTransitionTheMergeRequest(t *testing.T) {
	t.Parallel()
	e, srv := mergeEngine(t)
	const mrPath = "projects/grp%2Fmerge/merge_requests/2"
	const head = "3333333333333333333333333333333333333333"
	if got := glabSend(t, srv, "POST", mrPath+"/approve", `{"sha":"`+strings.Repeat("0", 40)+`"}`); got.status != http.StatusConflict || !strings.Contains(got.body, "SHA does not match HEAD") {
		t.Fatalf("a stale approve = %+v", got)
	}
	if approvals := glabJSON(t, srv, mrPath+"/approvals"); approvals["user_has_approved"] != false {
		t.Fatalf("a stale approve approved: %v", approvals)
	}
	if got := glabSend(t, srv, "POST", mrPath+"/approve", `{"sha":"`+head+`"}`); got.status != http.StatusCreated || !strings.Contains(got.body, `"user_has_approved":true`) {
		t.Fatalf("approve = %+v", got)
	}
	if approvals := glabJSON(t, srv, mrPath+"/approvals"); approvals["approvals_left"] != float64(0) || approvals["user_can_approve"] != false || approvals["approved"] != true {
		t.Fatalf("after approve: %v", approvals)
	}
	if got := glabSend(t, srv, "POST", mrPath+"/approve", `{"sha":"`+head+`"}`); got.status != http.StatusUnauthorized {
		t.Fatalf("a second approve = %+v", got)
	}
	notes := func() []string {
		var discussions []struct {
			Notes []struct {
				Body   string `json:"body"`
				System bool   `json:"system"`
			} `json:"notes"`
		}
		decodeBody(t, glabGET(t, srv, "gitlab.com", mrPath+"/discussions?per_page=100"), &discussions)
		var out []string
		for _, d := range discussions {
			for _, n := range d.Notes {
				if n.System {
					out = append(out, n.Body)
				}
			}
		}
		return out
	}
	if got := notes(); !slices.Equal(got, []string{"approved this merge request"}) {
		t.Fatalf("after approve: notes %q", got)
	}
	if got := glabSend(t, srv, "POST", mrPath+"/unapprove", ""); got.status != http.StatusCreated || !strings.Contains(got.body, `"user_has_approved":false`) {
		t.Fatalf("unapprove = %+v", got)
	}
	if got := glabSend(t, srv, "POST", mrPath+"/unapprove", ""); got.status != http.StatusNotFound {
		t.Fatalf("a second unapprove = %+v", got)
	}
	// Blocked: a merge is refused, auto-merge arms.
	if got := glabSend(t, srv, "PUT", mrPath+"/merge", `{"sha":"`+head+`","squash":false,"should_remove_source_branch":true}`); got.status != http.StatusMethodNotAllowed {
		t.Fatalf("a merge while blocked = %+v", got)
	}
	if got := glabSend(t, srv, "PUT", mrPath+"/merge", `{"sha":"`+strings.Repeat("0", 40)+`","auto_merge":true}`); got.status != http.StatusConflict {
		t.Fatalf("a stale auto-merge = %+v", got)
	}
	armed := glabSend(t, srv, "PUT", mrPath+"/merge", `{"sha":"`+head+`","squash":true,"should_remove_source_branch":true,"auto_merge":true,"merge_when_pipeline_succeeds":true}`)
	if armed.status != 200 || !strings.Contains(armed.body, `"merge_when_pipeline_succeeds":true`) || !strings.Contains(armed.body, `"squash":true`) {
		t.Fatalf("auto-merge = %+v", armed)
	}
	if mr := glabJSON(t, srv, mrPath); mr["merge_user"].(map[string]any)["username"] != "octo" || mr["force_remove_source_branch"] != true {
		t.Fatalf("armed merge request = %v", mr)
	}
	if got := glabSend(t, srv, "POST", mrPath+"/cancel_auto_merge", ""); got.status != http.StatusCreated || !strings.Contains(got.body, `"merge_when_pipeline_succeeds":false`) {
		t.Fatalf("cancel = %+v", got)
	}
	if got := glabSend(t, srv, "POST", mrPath+"/cancel_auto_merge", ""); got.status != http.StatusConflict {
		t.Fatalf("a second cancel = %+v", got)
	}
	if got := notes(); !slices.Equal(got, []string{"approved this merge request", "unapproved this merge request", "enabled an automatic merge when all merge checks for " + head + " pass", "canceled the automatic merge"}) {
		t.Fatalf("notes = %q", got)
	}
	// The pipeline finishes and someone approves (a reseed): mergeable,
	// then merged.
	ready := strings.Replace(mergeFixture, `"ci": {"jobs": [{"name": "unit", "status": "running"}]}}`, `"ci": {"jobs": [{"name": "unit", "status": "success"}]}, "reviews": [{"author": "bob", "state": "APPROVED"}]}`, 1)
	if ready == mergeFixture {
		t.Fatal("the ready fixture did not change")
	}
	if _, err := seedJSON(e, ready); err != nil {
		t.Fatal(err)
	}
	if mr := glabJSON(t, srv, mrPath); mr["detailed_merge_status"] != "mergeable" {
		t.Fatalf("ready merge request = %v", mr)
	}
	merged := glabSend(t, srv, "PUT", mrPath+"/merge", `{"sha":"`+head+`","squash":true,"should_remove_source_branch":true}`)
	if merged.status != 200 || !strings.Contains(merged.body, `"state":"merged"`) {
		t.Fatalf("merge = %+v", merged)
	}
	mr := glabJSON(t, srv, mrPath)
	if mr["merged_at"] == nil || mr["merged_by"].(map[string]any)["username"] != "octo" || mr["merge_commit_sha"] == nil || mr["squash_commit_sha"] == nil || mr["detailed_merge_status"] != "not_open" {
		t.Fatalf("merged merge request = %v", mr)
	}
	if got := glabSend(t, srv, "PUT", mrPath+"/merge", `{"sha":"`+head+`"}`); got.status != http.StatusMethodNotAllowed {
		t.Fatalf("a second merge = %+v", got)
	}
	// A rebase moves the head; a title update clears the draft prefix.
	if got := glabSend(t, srv, "PUT", "projects/grp%2Fmerge/merge_requests/1/rebase", ""); got.status != http.StatusAccepted || !strings.Contains(got.body, `"rebase_in_progress":true`) {
		t.Fatalf("rebase = %+v", got)
	}
	if mr := glabJSON(t, srv, "projects/grp%2Fmerge/merge_requests/1"); mr["sha"] == "" || mr["sha"] == glabJSON(t, srv, "projects/grp%2Fmerge/merge_requests/2")["sha"] {
		t.Fatalf("rebased merge request = %v", mr)
	}
	const draftPath = "projects/grp%2Fmerge/merge_requests/3"
	if got := glabSend(t, srv, "PUT", draftPath, `{"title":"Drafty"}`); got.status != 200 || !strings.Contains(got.body, `"draft":false`) {
		t.Fatalf("title update = %+v", got)
	}
	if got := glabSend(t, srv, "PUT", draftPath, `{"title":"Drafty","labels":"x"}`); got.status != 404 || !lastInvocation(t, e).Unhandled {
		t.Fatalf("an update with an unimplemented field = %+v", got)
	}
	if got := glabGET(t, srv, "gitlab.com", "user"); !strings.Contains(got.body, `"username":"octo"`) {
		t.Fatalf("user = %+v", got)
	}
}

// The refusal knob answers one route as told, recorded under the route,
// without running its handler, for its count or until cleared; it
// refuses what it cannot be, and Reset clears it.
func TestRefusalKnob(t *testing.T) {
	t.Parallel()
	e, srv := mergeEngine(t)
	const approve = "projects/grp%2Fmerge/merge_requests/2/approve"
	body := `{"sha":"3333333333333333333333333333333333333333"}`
	for name, bad := range map[string]Refusal{
		"unknown route":      {Route: "glab api approvals please", Status: 403},
		"unknown operation":  {Route: "gh graphql Nope", Status: 403},
		"a CLI command":      {Route: "gh pr create", Status: 403},
		"status":             {Route: "glab api approve", Status: 500},
		"200 on REST":        {Route: "glab api approve", Status: 200, Body: json.RawMessage(`{"errors":["x"]}`)},
		"200 without errors": {Route: "gh graphql MergePR", Status: 200, Body: json.RawMessage(`{"data":{}}`)},
		"content type":       {Route: "glab api approve", Status: 403, Header: map[string]string{"content-type": "text/plain"}},
		"negative times":     {Route: "glab api approve", Status: 403, Times: -1},
	} {
		if err := e.SetRefusal(bad); err == nil {
			t.Errorf("%s: accepted %+v", name, bad)
		}
	}
	scope := `{"error":"insufficient_scope","error_description":"The request requires higher privileges than provided by the access token.","scope":"api"}`
	if err := e.SetRefusal(Refusal{Route: "glab api approve", Status: 403, Body: json.RawMessage(scope), Times: 1}); err != nil {
		t.Fatal(err)
	}
	got := glabSend(t, srv, "POST", approve, body)
	inv := lastInvocation(t, e)
	if got.status != 403 || got.body != scope || inv.Route != "glab api approve" || inv.Unhandled || inv.Status != 403 {
		t.Fatalf("refused = %+v, invocation %+v", got, inv)
	}
	if approvals := glabJSON(t, srv, "projects/grp%2Fmerge/merge_requests/2/approvals"); approvals["user_has_approved"] != false {
		t.Fatal("a refused approve approved")
	}
	if got := glabSend(t, srv, "POST", approve, body); got.status != http.StatusCreated {
		t.Fatalf("after a one-time refusal = %+v", got)
	}
	// Until cleared, with headers, on a GraphQL operation too.
	if err := e.SetRefusal(Refusal{Route: "gh api pull reviews", Status: 403, Header: map[string]string{"X-Accepted-OAuth-Scopes": "repo"}, Body: json.RawMessage(`{"message":"Resource not accessible by integration"}`)}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got := httpRequest(t, srv, "POST", "github.com", "/github/rest/repos/acme/merge/pulls/2/reviews", `{"event":"APPROVE"}`, nil)
		if got.status != 403 || got.header.Get("X-Accepted-OAuth-Scopes") != "repo" {
			t.Fatalf("sticky refusal = %+v", got)
		}
	}
	if err := e.SetRefusal(Refusal{Route: "gh api pull reviews"}); err != nil {
		t.Fatal(err)
	}
	if got := httpRequest(t, srv, "POST", "github.com", "/github/rest/repos/acme/merge/pulls/2/reviews", `{"event":"APPROVE"}`, nil); got.status != 200 {
		t.Fatalf("after clearing = %+v", got)
	}
	headMoved := `{"data":{"mergePullRequest":null},"errors":[{"type":"UNPROCESSABLE","message":"Head branch was modified. Review and try the merge again."}]}`
	if err := e.SetRefusal(Refusal{Route: "gh graphql MergePR", Status: 200, Body: json.RawMessage(headMoved)}); err != nil {
		t.Fatal(err)
	}
	if got := graphQL(t, srv, "github.com", "MergePR", map[string]any{"pullRequestID": "PR_x", "method": "MERGE", "expectedHeadOid": "abc1234"}); got.status != 200 || got.body != headMoved || lastInvocation(t, e).Route != "gh graphql MergePR" {
		t.Fatalf("GraphQL refusal = %+v", got)
	}
	e.Reset()
	if _, err := seedJSON(e, mergeFixture); err != nil {
		t.Fatal(err)
	}
	if got := graphQL(t, srv, "github.com", "MergePR", map[string]any{"pullRequestID": "PR_x", "method": "MERGE", "expectedHeadOid": "abc1234"}); strings.Contains(got.body, "Head branch was modified") {
		t.Fatalf("Reset kept a refusal: %s", got.body)
	}
}
