package git

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/forgeapi"
	"agent-overflow/internal/git/forgeschema"
	"agent-overflow/internal/harness/forgefake"
)

const factsToken = "facts-token"

// factsForge is a Core whose forge API is the fake forge, every GraphQL
// answer checked against the request's document and the forge's schema.
// While gate is set, GraphQL requests wait for it to close after telling
// arrived.
type factsForge struct {
	core    *Core
	engine  *forgefake.Engine
	mu      sync.Mutex
	gate    chan struct{}
	arrived chan string
	// seen counts GraphQL requests by operation, and failAt answers the
	// one of an operation at that count with a GraphQL error and no data.
	seen, failAt map[string]int
}

func newFactsForge(t testing.TB, fixture forgefake.Fixture) *factsForge {
	t.Helper()
	engine := forgefake.New(forgefake.Options{APIToken: factsToken})
	if _, err := engine.Seed(fixture); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	f := &factsForge{engine: engine, arrived: make(chan string, 64), seen: map[string]int{}, failAt: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forge := map[string]string{"/github/graphql": "github", "/gitlab/api/graphql": "gitlab"}[r.URL.Path]
		if forge == "" {
			engine.ServeHTTP(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var payload struct {
			Query         string `json:"query"`
			OperationName string `json:"operationName"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("a GraphQL request that is not JSON: %v", err)
		}
		f.mu.Lock()
		gate := f.gate
		f.seen[payload.OperationName]++
		fail := f.failAt[payload.OperationName] == f.seen[payload.OperationName]
		f.mu.Unlock()
		if fail {
			// GitLab's answer to a query that failed while it ran.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"errors":[{"message":"Internal server error"}]}`))
			return
		}
		if gate != nil {
			f.arrived <- payload.OperationName
			<-gate
		}
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, r)
		maps.Copy(w.Header(), rec.Header())
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		if rec.Code != http.StatusOK {
			return
		}
		problems, err := forgeschema.AnswerProblems(forge, payload.Query, rec.Body.Bytes())
		if err != nil {
			t.Errorf("GraphQL answer check: %v", err)
		}
		for _, p := range problems {
			t.Errorf("the fake's GraphQL answer: %s", p)
		}
	}))
	t.Cleanup(srv.Close)
	svc, err := forgeapi.New(forgeapi.Options{Version: "test", Isolated: &forgeapi.Isolated{BaseURL: srv.URL, Token: factsToken}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	f.core = NewCore(WithForgeAPI(svc))
	t.Cleanup(func() {
		for _, inv := range engine.Invocations(0).Invocations {
			if inv.Unhandled {
				t.Errorf("unhandled forge call %s %s: %s", inv.Method, inv.Path, inv.Detail)
			}
		}
	})
	return f
}

// hold makes GraphQL requests wait until the returned release runs.
func (f *factsForge) hold() (release func()) {
	gate := make(chan struct{})
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.gate = nil
			f.mu.Unlock()
			close(gate)
		})
	}
}

// requests are the facts reads the engine received since seq, by
// operation, with the keys' slots each used.
func (f *factsForge) requests(t *testing.T, since int) []factsRequest {
	t.Helper()
	var out []factsRequest
	for _, inv := range f.engine.Invocations(since).Invocations {
		if !strings.HasPrefix(inv.Operation, "ForgeFacts") && inv.Route != "rate limited" {
			continue
		}
		req := factsRequest{Operation: inv.Operation, Route: inv.Route}
		var vars map[string]any
		if len(inv.Variables) > 0 {
			if err := json.Unmarshal(inv.Variables, &vars); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; ; i++ {
			n := fmt.Sprint(i)
			if _, ok := vars["o"+n]; !ok {
				if _, ok := vars["p"+n]; !ok {
					break
				}
			}
			used := false
			for _, flag := range []string{"r", "u", "q", "g"} {
				if vars[flag+n] == true {
					used = true
				}
			}
			switch {
			case !used:
			case vars["o"+n] != nil && vars["r"+n] == true:
				req.Used = append(req.Used, fmt.Sprintf("%v/%v#%v", vars["o"+n], vars["n"+n], vars["x"+n]))
			case vars["o"+n] != nil:
				req.Used = append(req.Used, fmt.Sprint(vars["o"+n]))
			case vars["n"+n] != "":
				req.Used = append(req.Used, fmt.Sprintf("%v#%v", vars["p"+n], vars["n"+n]))
			default:
				req.Used = append(req.Used, fmt.Sprintf("%v~%v", vars["p"+n], vars["t"+n]))
			}
		}
		out = append(out, req)
	}
	return out
}

type factsRequest struct {
	Operation string
	Route     string
	Used      []string
}

func (f *factsForge) seq() int {
	log := f.engine.Invocations(0).Invocations
	if len(log) == 0 {
		return 0
	}
	return log[len(log)-1].Seq
}

var (
	githubFactsPR = PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "app", Number: 2}
	gitlabFactsPR = PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "app", Number: 5}
)

func githubFactsFixture() forgefake.Fixture {
	return forgefake.Fixture{
		Viewer: "alice",
		Accounts: []forgefake.Account{
			{Forge: "github", Login: "octo", Name: "Octo Cat"},
			{Forge: "github", Login: "acme", Name: "Acme Inc", Kind: "org"},
			{Forge: "github", Login: "nameless"},
		},
		Repos: []forgefake.Repo{
			{Forge: "github", Project: "acme/app",
				Pulls:  []forgefake.Pull{{Number: 2, Title: "Pull two", Author: "octo", HeadRef: "two", HeadSHA: strings.Repeat("2", 40)}, {Number: 4, Title: "Merged four", Author: "octo", HeadRef: "four", HeadSHA: strings.Repeat("4", 40), State: "merged"}},
				Issues: []forgefake.Issue{{Number: 1, Title: "Issue one"}, {Number: 3, Title: "  Closed three  ", State: "closed"}}},
			{Forge: "github", Project: "other/lib", Issues: []forgefake.Issue{{Number: 9, Title: "Lib nine"}}},
		},
	}
}

func gitlabFactsFixture() forgefake.Fixture {
	return forgefake.Fixture{
		Viewer: "alice",
		Accounts: []forgefake.Account{
			{Forge: "gitlab", Login: "alice", Name: "Alice A"},
			{Forge: "gitlab", Login: "grp", Name: "The Group", Kind: "group", Epics: []forgefake.Issue{{Number: 4, Title: "Epic four", State: "closed"}}},
		},
		Repos: []forgefake.Repo{{
			Forge: "gitlab", Project: "grp/app",
			Pulls:      []forgefake.Pull{{Number: 5, Title: "MR five", Author: "alice", HeadRef: "five", HeadSHA: strings.Repeat("5", 40)}},
			Issues:     []forgefake.Issue{{Number: 1, Title: "Issue one"}},
			Labels:     []forgefake.Label{{Name: "bug", Color: "#F00", TextColor: "#FFFFFF"}, {Name: "needs review", Color: "#00ff00"}},
			Milestones: []forgefake.Milestone{{Title: "v1", IID: 3}, {Title: "v2", IID: 8, Group: "grp"}},
			Snippets:   []forgefake.Snippet{{ID: 7, Title: "Snip seven"}},
		}},
	}
}

func resolveFacts(t testing.TB, core *Core, pr PRReference, keys ...string) map[string]ForgeFact {
	t.Helper()
	got, err := core.ResolveFacts(t.Context(), t.TempDir(), pr, PRRevision{}, keys)
	if err != nil {
		t.Fatalf("ResolveFacts: %v", err)
	}
	if len(got.Facts) != len(keys) {
		t.Fatalf("ResolveFacts answered %d facts for %d keys", len(got.Facts), len(keys))
	}
	out := map[string]ForgeFact{}
	for i, fact := range got.Facts {
		if fact.Key != keys[i] {
			t.Fatalf("fact %d is %q, want %q (keys' order)", i, fact.Key, keys[i])
		}
		out[fact.Key] = fact
	}
	return out
}

func expectFacts(t *testing.T, got map[string]ForgeFact, want ...ForgeFact) {
	t.Helper()
	for _, w := range want {
		if g := got[w.Key]; !reflect.DeepEqual(g, w) {
			t.Errorf("%s = %+v\nwant %+v", w.Key, g, w)
		}
	}
}

// Every GitHub kind answers found, absent or unavailable in one request,
// a NOT_FOUND at one alias leaving the others' answers whole.
func TestGitHubForgeFactsAnswerEachKeyInOneRequest(t *testing.T) {
	t.Parallel()
	f := newFactsForge(t, githubFactsFixture())
	keys := []string{
		"ref:acme/app#1", "ref:acme/app#2", "ref:acme/app#3", "ref:acme/app#4",
		"ref:acme/app#99", "ref:nobody/none#1", "ref:other/lib#9",
		"user:octo", "user:acme", "user:nameless", "user:ghost",
		"user:-bad-", "ref:acme/app#0", "label:acme/app~bug", "wiki:acme/app", "ref:acme/app#1",
	}
	got := resolveFacts(t, f.core, githubFactsPR, keys...)
	expectFacts(t, got,
		ForgeFact{Key: "ref:acme/app#1", Status: FactFound, Kind: "issue", Title: "Issue one", State: "open", URL: "https://github.com/acme/app/issues/1"},
		ForgeFact{Key: "ref:acme/app#2", Status: FactFound, Kind: "pull", Title: "Pull two", State: "open", URL: "https://github.com/acme/app/pull/2"},
		ForgeFact{Key: "ref:acme/app#3", Status: FactFound, Kind: "issue", Title: "Closed three", State: "closed", URL: "https://github.com/acme/app/issues/3"},
		ForgeFact{Key: "ref:acme/app#4", Status: FactFound, Kind: "pull", Title: "Merged four", State: "merged", URL: "https://github.com/acme/app/pull/4"},
		ForgeFact{Key: "ref:acme/app#99", Status: FactAbsent},
		ForgeFact{Key: "ref:nobody/none#1", Status: FactAbsent},
		ForgeFact{Key: "ref:other/lib#9", Status: FactFound, Kind: "issue", Title: "Lib nine", State: "open", URL: "https://github.com/other/lib/issues/9"},
		ForgeFact{Key: "user:octo", Status: FactFound, Kind: "user", Title: "Octo Cat", URL: "https://github.com/octo"},
		ForgeFact{Key: "user:acme", Status: FactFound, Kind: "org", Title: "Acme Inc", URL: "https://github.com/acme"},
		ForgeFact{Key: "user:nameless", Status: FactFound, Kind: "user", URL: "https://github.com/nameless"},
		ForgeFact{Key: "user:ghost", Status: FactAbsent},
		// Not a login GitHub can hold, not a number it can, a kind it has
		// not, a kind nobody has: no slot.
		ForgeFact{Key: "user:-bad-", Status: FactAbsent},
		ForgeFact{Key: "ref:acme/app#0", Status: FactUnavailable},
		ForgeFact{Key: "label:acme/app~bug", Status: FactUnavailable},
		ForgeFact{Key: "wiki:acme/app", Status: FactUnavailable},
	)
	reqs := f.requests(t, 0)
	if len(reqs) != 1 || reqs[0].Operation != "ForgeFacts32" || len(reqs[0].Used) != 11 {
		t.Fatalf("requests = %+v, want one ForgeFacts32 with the 11 distinct readable keys", reqs)
	}
}

// Every GitLab kind answers, a user: key falling back to a group, label
// colors normalized, URLs canonical.
func TestGitLabForgeFactsAnswerEveryKind(t *testing.T) {
	t.Parallel()
	f := newFactsForge(t, gitlabFactsFixture())
	got := resolveFacts(t, f.core, gitlabFactsPR,
		"ref:grp/app#1", "ref:grp/app#2", "mr:grp/app!5", "mr:grp/app!6",
		"label:grp/app~bug", "label:grp/app~needs review", "label:grp/app~nope",
		"milestone:grp/app%v1", "milestone:grp/app%v2", "milestone:grp/app%v9",
		"snippet:grp/app$7", "snippet:grp/app$8",
		"epic:grp&4", "epic:grp&5", "epic:nogroup&1",
		"user:alice", "user:grp", "user:nobody",
		"ref:gone/app#1",
	)
	expectFacts(t, got,
		ForgeFact{Key: "ref:grp/app#1", Status: FactFound, Kind: "issue", Title: "Issue one", State: "open", URL: "https://gitlab.com/grp/app/-/issues/1"},
		ForgeFact{Key: "ref:grp/app#2", Status: FactAbsent},
		ForgeFact{Key: "mr:grp/app!5", Status: FactFound, Title: "MR five", State: "open", URL: "https://gitlab.com/grp/app/-/merge_requests/5"},
		ForgeFact{Key: "mr:grp/app!6", Status: FactAbsent},
		ForgeFact{Key: "label:grp/app~bug", Status: FactFound, Title: "bug", Color: "#ff0000", TextColor: "#ffffff", URL: "https://gitlab.com/grp/app/-/issues?label_name=bug"},
		ForgeFact{Key: "label:grp/app~needs review", Status: FactFound, Title: "needs review", Color: "#00ff00", TextColor: "#1f1e24", URL: "https://gitlab.com/grp/app/-/issues?label_name=needs+review"},
		ForgeFact{Key: "label:grp/app~nope", Status: FactAbsent},
		ForgeFact{Key: "milestone:grp/app%v1", Status: FactFound, Title: "v1", URL: "https://gitlab.com/grp/app/-/milestones/3"},
		ForgeFact{Key: "milestone:grp/app%v2", Status: FactFound, Title: "v2", URL: "https://gitlab.com/groups/grp/-/milestones/8"},
		ForgeFact{Key: "milestone:grp/app%v9", Status: FactAbsent},
		ForgeFact{Key: "snippet:grp/app$7", Status: FactFound, Title: "Snip seven", URL: "https://gitlab.com/grp/app/-/snippets/7"},
		ForgeFact{Key: "snippet:grp/app$8", Status: FactAbsent},
		ForgeFact{Key: "epic:grp&4", Status: FactFound, Title: "Epic four", State: "closed", URL: "https://gitlab.com/groups/grp/-/epics/4"},
		ForgeFact{Key: "epic:grp&5", Status: FactAbsent},
		ForgeFact{Key: "epic:nogroup&1", Status: FactAbsent},
		ForgeFact{Key: "user:alice", Status: FactFound, Kind: "user", Title: "Alice A", URL: "https://gitlab.com/alice"},
		ForgeFact{Key: "user:grp", Status: FactFound, Kind: "group", Title: "The Group", URL: "https://gitlab.com/groups/grp"},
		ForgeFact{Key: "user:nobody", Status: FactAbsent},
		ForgeFact{Key: "ref:gone/app#1", Status: FactAbsent},
	)
	reqs := f.requests(t, 0)
	if len(reqs) != 1 || reqs[0].Operation != "ForgeFacts32" || len(reqs[0].Used) != 19 {
		t.Fatalf("requests = %+v, want one ForgeFacts32 with every key", reqs)
	}
}

// labelsFixture is a GitLab project grp/app with n labels whose titles
// are title(i), in reverse order of title.
func labelsFixture(n int, title func(i int) string) forgefake.Fixture {
	labels := make([]forgefake.Label, n)
	for i := range labels {
		labels[i] = forgefake.Label{Name: title(n - 1 - i), Color: "#ABC"}
	}
	return forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{{Forge: "gitlab", Project: "grp/app", Labels: labels}}}
}

func labelTitleOf(i int) string { return fmt.Sprintf("label %04d", i) }

// labelListRequests counts the label list pages read since seq.
func (f *factsForge) labelListRequests(since int) int {
	n := 0
	for _, inv := range f.engine.Invocations(since).Invocations {
		if inv.Operation == "ForgeFactsLabels" {
			n++
		}
	}
	return n
}

// A labels: key answers the project's whole label list, read 100 a page
// by cursor in the forge's order, complete when the last page says so;
// past ten pages it answers the first 1,000, not complete. It is held in
// the facts cache at a size that counts its labels, and a held list sends
// nothing.
func TestGitLabLabelListPagesToItsCap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		labels, pages, held int
		complete            bool
	}{
		{0, 1, 0, true},
		{250, 3, 250, true},
		{1000, 10, 1000, true},
		{1050, 10, 1000, false},
	} {
		t.Run(fmt.Sprint(tc.labels), func(t *testing.T) {
			t.Parallel()
			f := newFactsForge(t, labelsFixture(tc.labels, labelTitleOf))
			got := resolveFacts(t, f.core, gitlabFactsPR, "labels:grp/app")["labels:grp/app"]
			if got.Status != FactFound || len(got.Labels) != tc.held || got.Complete != tc.complete {
				t.Fatalf("list = %s with %d labels, complete %v; want found with %d, complete %v", got.Status, len(got.Labels), got.Complete, tc.held, tc.complete)
			}
			for i, label := range got.Labels {
				if want := (ForgeLabel{Title: labelTitleOf(i), Color: "#aabbcc", TextColor: "#1f1e24"}); label != want {
					t.Fatalf("label %d = %+v, want %+v", i, label, want)
				}
			}
			if n := f.labelListRequests(0); n != tc.pages {
				t.Fatalf("%d pages read, want %d", n, tc.pages)
			}
			entries, bytes := f.core.forges["gitlab"].(*gitlabForge).facts.held()
			if want := factBytes(f.cacheKey(t, "labels:grp/app"), got); entries != 1 || bytes != want {
				t.Fatalf("held %d entries, %d bytes; want 1 entry of %d bytes", entries, bytes, want)
			}
			if minimum := int64(tc.held * 64); bytes < minimum {
				t.Fatalf("a list of %d labels counts %d bytes, under %d", tc.held, bytes, minimum)
			}
			since := f.seq()
			resolveFacts(t, f.core, gitlabFactsPR, "labels:grp/app")
			if n := f.labelListRequests(since); n != 0 {
				t.Fatalf("a held list read %d pages again", n)
			}
		})
	}
}

// cacheKey is the facts cache key of key for gitlabFactsPR under the
// fake's login.
func (f *factsForge) cacheKey(t *testing.T, key string) string {
	t.Helper()
	client, err := f.core.gitlabAPI(gitlabFactsPR.Host)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := client.CredentialFingerprint(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return gitlabFactsPR.Forge + "\x00" + gitlabFactsPR.Host + "\x00" + fp + "\x00" + key
}

// A list stops at maxLabelListBytes, not complete, so the worst list a
// cache entry holds is bounded however long the titles; and the entry
// fits the forge facts cache.
func TestGitLabLabelListBoundsItsBytes(t *testing.T) {
	t.Parallel()
	long := func(i int) string { return fmt.Sprintf("%04d", i) + strings.Repeat("é", 250) }
	f := newFactsForge(t, labelsFixture(1000, long))
	got := resolveFacts(t, f.core, gitlabFactsPR, "labels:grp/app")["labels:grp/app"]
	var bytes int64
	for _, label := range got.Labels {
		bytes += labelBytes(label)
	}
	if got.Status != FactFound || got.Complete || len(got.Labels) == 0 || bytes > maxLabelListBytes || bytes+labelBytes(got.Labels[0]) <= maxLabelListBytes {
		t.Fatalf("a list of long titles = %d labels of %d bytes, complete %v; want it to stop within %d", len(got.Labels), bytes, got.Complete, maxLabelListBytes)
	}
	if entries, _ := f.core.forges["gitlab"].(*gitlabForge).facts.held(); entries != 1 {
		t.Fatalf("the largest list is not held (%d entries)", entries)
	}
}

// A project GitLab does not have is absent and held as absent; a page that
// fails makes the whole list unavailable, nothing of it held, and the next
// call reads it again from the first page.
func TestGitLabLabelListAbsentAndFailedPage(t *testing.T) {
	t.Parallel()
	f := newFactsForge(t, labelsFixture(250, labelTitleOf))
	got := resolveFacts(t, f.core, gitlabFactsPR, "labels:grp/gone")
	expectFacts(t, got, ForgeFact{Key: "labels:grp/gone", Status: FactAbsent})
	since := f.seq()
	resolveFacts(t, f.core, gitlabFactsPR, "labels:grp/gone")
	if n := f.labelListRequests(since); n != 0 {
		t.Fatalf("an absent list was read again (%d pages) within its TTL", n)
	}

	f.mu.Lock()
	f.failAt["ForgeFactsLabels"] = f.seen["ForgeFactsLabels"] + 2
	f.mu.Unlock()
	got = resolveFacts(t, f.core, gitlabFactsPR, "labels:grp/app", "ref:grp/app#1")
	expectFacts(t, got, ForgeFact{Key: "labels:grp/app", Status: FactUnavailable}, ForgeFact{Key: "ref:grp/app#1", Status: FactAbsent})
	forge := f.core.forges["gitlab"].(*gitlabForge)
	if _, ok := forge.facts.get(f.cacheKey(t, "labels:grp/app"), f.core.nowFn()); ok {
		t.Fatal("a list whose page failed is held")
	}
	since = f.seq()
	got = resolveFacts(t, f.core, gitlabFactsPR, "labels:grp/app")
	if n := f.labelListRequests(since); n != 3 || len(got["labels:grp/app"].Labels) != 250 {
		t.Fatalf("after a failed page: %d pages, %d labels; want the whole list read again", n, len(got["labels:grp/app"].Labels))
	}
}

// Concurrent calls naming one list share one read of it; a list key on
// GitHub, or with no project, is unavailable without a request.
func TestGitLabLabelListSharedAndRefused(t *testing.T) {
	t.Parallel()
	f := newFactsForge(t, labelsFixture(150, labelTitleOf))
	joined := make(chan string, 8)
	f.core.forges["gitlab"].(*gitlabForge).facts.onJoin = func(key string) { joined <- key }
	release := f.hold()
	defer release()
	answers := make(chan map[string]ForgeFact, 2)
	go func() { answers <- resolveFacts(t, f.core, gitlabFactsPR, "labels:grp/app") }()
	<-f.arrived
	go func() { answers <- resolveFacts(t, f.core, gitlabFactsPR, "labels:grp/app") }()
	<-joined
	release()
	for range 2 {
		if got := (<-answers)["labels:grp/app"]; len(got.Labels) != 150 || !got.Complete {
			t.Fatalf("a shared list = %d labels, complete %v", len(got.Labels), got.Complete)
		}
	}
	if n := f.labelListRequests(0); n != 2 {
		t.Fatalf("two concurrent calls read %d pages, want the list's 2 once", n)
	}

	g := newFactsForge(t, githubFactsFixture())
	expectFacts(t, resolveFacts(t, g.core, githubFactsPR, "labels:acme/app", "labels:"),
		ForgeFact{Key: "labels:acme/app", Status: FactUnavailable}, ForgeFact{Key: "labels:", Status: FactUnavailable})
	expectFacts(t, resolveFacts(t, f.core, gitlabFactsPR, "labels:", "labels:-bad"),
		ForgeFact{Key: "labels:", Status: FactUnavailable}, ForgeFact{Key: "labels:-bad", Status: FactAbsent})
	if n := len(g.engine.Invocations(0).Invocations); n != 0 {
		t.Fatalf("refused list keys made %d requests", n)
	}
}

// A call fully cached sends nothing; a fact the forge could not answer is
// not held; an absent fact expires before a found one; the largest
// document's slots are filled before a second request.
func TestForgeFactsCacheByStatus(t *testing.T) {
	t.Parallel()
	f := newFactsForge(t, githubFactsFixture())
	now := time.Unix(1_800_000_000, 0)
	var clock sync.Mutex
	f.core.nowFn = func() time.Time { clock.Lock(); defer clock.Unlock(); return now }
	advance := func(d time.Duration) { clock.Lock(); now = now.Add(d); clock.Unlock() }

	keys := []string{"ref:acme/app#1", "ref:acme/app#99", "user:octo"}
	resolveFacts(t, f.core, githubFactsPR, keys...)
	since := f.seq()
	got := resolveFacts(t, f.core, githubFactsPR, keys...)
	if reqs := f.requests(t, since); len(reqs) != 0 {
		t.Fatalf("a fully cached call sent %+v", reqs)
	}
	expectFacts(t, got, ForgeFact{Key: "ref:acme/app#99", Status: FactAbsent})

	advance(forgeFactAbsentTTL + time.Second)
	since = f.seq()
	resolveFacts(t, f.core, githubFactsPR, keys...)
	if reqs := f.requests(t, since); len(reqs) != 1 || !slices.Equal(reqs[0].Used, []string{"acme/app#99"}) {
		t.Fatalf("after the absent TTL: %+v, want one request for the absent key", reqs)
	}
	advance(forgeFactFoundTTL)
	since = f.seq()
	resolveFacts(t, f.core, githubFactsPR, keys...)
	if reqs := f.requests(t, since); len(reqs) != 1 || len(reqs[0].Used) != 3 {
		t.Fatalf("after the found TTL: %+v, want one request for every key", reqs)
	}

	// The forge refuses the read: every key unavailable, nothing held. A
	// 502 is GitHub stopping a slow query, read again once in halves and
	// no further.
	if err := f.engine.SetRefusal(forgefake.Refusal{Route: "gh graphql ForgeFacts8", Status: http.StatusBadGateway}); err != nil {
		t.Fatal(err)
	}
	since = f.seq()
	got = resolveFacts(t, f.core, githubFactsPR, "ref:acme/app#3", "user:acme", "user:octo", "user:nameless")
	expectFacts(t, got, ForgeFact{Key: "ref:acme/app#3", Status: FactUnavailable}, ForgeFact{Key: "user:acme", Status: FactUnavailable})
	if reqs := f.requests(t, since); len(reqs) != 3 {
		t.Fatalf("a refusing forge was sent %d requests for one read, want 3 (the read and its halves): %+v", len(reqs), reqs)
	}
	if err := f.engine.SetRefusal(forgefake.Refusal{Route: "gh graphql ForgeFacts8"}); err != nil {
		t.Fatal(err)
	}
	since = f.seq()
	got = resolveFacts(t, f.core, githubFactsPR, "ref:acme/app#3", "user:acme")
	if reqs := f.requests(t, since); len(reqs) != 1 || got["ref:acme/app#3"].Status != FactFound {
		t.Fatalf("after a refused read: %+v %+v, want the keys read again", reqs, got)
	}

	// 129 keys: the largest document full, the rest in the smallest.
	var many []string
	for i := range 129 {
		many = append(many, fmt.Sprintf("ref:acme/app#%d", 1000+i))
	}
	since = f.seq()
	resolveFacts(t, f.core, githubFactsPR, many...)
	reqs := f.requests(t, since)
	if len(reqs) != 2 || reqs[0].Operation != "ForgeFacts128" || len(reqs[0].Used) != 128 || reqs[1].Operation != "ForgeFacts8" || len(reqs[1].Used) != 1 {
		t.Fatalf("129 keys: %+v", reqs)
	}
}

// Concurrent calls share the reads of the keys they share: two identical
// calls one request, overlapping calls one slot per shared key.
func TestForgeFactsConcurrentCallsShareReads(t *testing.T) {
	t.Parallel()
	t.Run("identical", func(t *testing.T) {
		t.Parallel()
		f := newFactsForge(t, githubFactsFixture())
		joined := make(chan string, 8)
		f.core.forges["github"].(*githubForge).facts.onJoin = func(key string) { joined <- key }
		release := f.hold()
		defer release()
		keys := []string{"ref:acme/app#1", "user:octo"}
		answers := make(chan map[string]ForgeFact, 2)
		go func() { answers <- resolveFacts(t, f.core, githubFactsPR, keys...) }()
		<-f.arrived
		go func() { answers <- resolveFacts(t, f.core, githubFactsPR, keys...) }()
		<-joined
		<-joined
		release()
		for range 2 {
			expectFacts(t, <-answers, ForgeFact{Key: "ref:acme/app#1", Status: FactFound, Kind: "issue", Title: "Issue one", State: "open", URL: "https://github.com/acme/app/issues/1"})
		}
		if reqs := f.requests(t, 0); len(reqs) != 1 {
			t.Fatalf("two identical concurrent calls sent %+v", reqs)
		}
	})
	t.Run("overlapping", func(t *testing.T) {
		t.Parallel()
		f := newFactsForge(t, gitlabFactsFixture())
		release := f.hold()
		defer release()
		answers := make(chan map[string]ForgeFact, 2)
		go func() { answers <- resolveFacts(t, f.core, gitlabFactsPR, "ref:grp/app#1", "mr:grp/app!5") }()
		<-f.arrived
		// The second call claims the shared key before it sends its own.
		go func() { answers <- resolveFacts(t, f.core, gitlabFactsPR, "mr:grp/app!5", "user:alice") }()
		<-f.arrived
		release()
		for range 2 {
			if got := <-answers; got["mr:grp/app!5"].Status != FactFound {
				t.Fatalf("shared key = %+v", got["mr:grp/app!5"])
			}
		}
		reqs := f.requests(t, 0)
		var used []string
		for _, r := range reqs {
			used = append(used, r.Used...)
		}
		slices.Sort(used)
		if want := []string{"alice~alice", "grp/app#1", "grp/app#5"}; len(reqs) != 2 || !slices.Equal(used, want) {
			t.Fatalf("overlapping calls sent %+v (slots %q), want the shared key once", reqs, used)
		}
	})
}

// Under the rate limit's reserve a facts read is background work: the
// gate refuses it without a request and its keys answer unavailable; a
// rate limited answer is unavailable and held by nobody.
func TestForgeFactsRateGate(t *testing.T) {
	t.Parallel()
	f := newFactsForge(t, githubFactsFixture())
	reset := time.Now().Add(time.Hour).Unix()
	if err := f.engine.SetRateLimit(forgefake.RateLimit{Forge: "github", Pool: "graphql", Remaining: 499, Reset: reset}); err != nil {
		t.Fatal(err)
	}
	got := resolveFacts(t, f.core, githubFactsPR, "ref:acme/app#1")
	expectFacts(t, got, ForgeFact{Key: "ref:acme/app#1", Status: FactFound, Kind: "issue", Title: "Issue one", State: "open", URL: "https://github.com/acme/app/issues/1"})
	since := f.seq()
	got = resolveFacts(t, f.core, githubFactsPR, "ref:acme/app#2", "user:octo")
	expectFacts(t, got, ForgeFact{Key: "ref:acme/app#2", Status: FactUnavailable}, ForgeFact{Key: "user:octo", Status: FactUnavailable})
	if n := len(f.engine.Invocations(since).Invocations); n != 0 {
		t.Fatalf("the gate's refusal made %d requests", n)
	}

	g := newFactsForge(t, gitlabFactsFixture())
	if err := g.engine.SetRateLimit(forgefake.RateLimit{Forge: "gitlab", Pool: "throttle_authenticated_api", Remaining: 0, Reset: reset}); err != nil {
		t.Fatal(err)
	}
	got = resolveFacts(t, g.core, gitlabFactsPR, "ref:grp/app#1")
	expectFacts(t, got, ForgeFact{Key: "ref:grp/app#1", Status: FactUnavailable})
	if entries, _ := g.core.forges["gitlab"].(*gitlabForge).facts.held(); entries != 0 {
		t.Fatalf("a rate limited read held %d facts", entries)
	}
}

// A problem at one alias other than NOT_FOUND makes that key unavailable
// and leaves the others' answers; a GitLab complexity refusal is read
// again in halves.
func TestForgeFactsPerAliasProblems(t *testing.T) {
	t.Parallel()
	f := newFactsForge(t, githubFactsFixture())
	body := `{"data":{"r0":null,"r1":null,"u2":{"__typename":"User","login":"octo","url":"https://github.com/octo","name":"Octo Cat"}},` +
		`"errors":[{"type":"FORBIDDEN","path":["r0"],"message":"Resource not accessible by integration"},{"type":"NOT_FOUND","path":["r1"],"message":"Could not resolve"}]}`
	if err := f.engine.SetRefusal(forgefake.Refusal{Route: "gh graphql ForgeFacts8", Status: http.StatusOK, Body: json.RawMessage(body), Times: 1}); err != nil {
		t.Fatal(err)
	}
	got := resolveFacts(t, f.core, githubFactsPR, "ref:acme/app#1", "ref:acme/app#99", "user:octo")
	expectFacts(t, got,
		ForgeFact{Key: "ref:acme/app#1", Status: FactUnavailable},
		ForgeFact{Key: "ref:acme/app#99", Status: FactAbsent},
		ForgeFact{Key: "user:octo", Status: FactFound, Kind: "user", Title: "Octo Cat", URL: "https://github.com/octo"},
	)

	g := newFactsForge(t, gitlabFactsFixture())
	if err := g.engine.SetRefusal(forgefake.Refusal{Route: "glab graphql ForgeFacts16", Status: http.StatusOK,
		Body: json.RawMessage(`{"errors":[{"message":"Query has complexity of 260, which exceeds max complexity of 250"}]}`), Times: 1}); err != nil {
		t.Fatal(err)
	}
	keys := []string{"ref:grp/app#1", "mr:grp/app!5", "label:grp/app~bug", "user:alice", "epic:grp&4", "snippet:grp/app$7"}
	got = resolveFacts(t, g.core, gitlabFactsPR, keys...)
	for _, key := range keys {
		if got[key].Status != FactFound {
			t.Errorf("%s = %+v after the halves", key, got[key])
		}
	}
	var ops []string
	for _, r := range g.requests(t, 0) {
		ops = append(ops, fmt.Sprintf("%s:%d", r.Operation, len(r.Used)))
	}
	if want := []string{"ForgeFacts16:6", "ForgeFacts4:3", "ForgeFacts4:3"}; !slices.Equal(ops, want) {
		t.Fatalf("requests = %q, want %q", ops, want)
	}
}

// Keys of at most four projects other than the PR's are read; a fifth's
// answer unavailable without a slot.
func TestForgeFactsBoundOtherProjects(t *testing.T) {
	t.Parallel()
	f := newFactsForge(t, githubFactsFixture())
	keys := []string{"ref:acme/app#1", "ref:p1/r#1", "ref:p2/r#1", "ref:P1/r#2", "ref:p3/r#1", "ref:p4/r#1", "ref:p5/r#1", "user:octo"}
	got := resolveFacts(t, f.core, githubFactsPR, keys...)
	expectFacts(t, got, ForgeFact{Key: "ref:p5/r#1", Status: FactUnavailable}, ForgeFact{Key: "ref:p4/r#1", Status: FactAbsent})
	if reqs := f.requests(t, 0); len(reqs) != 1 || len(reqs[0].Used) != 7 {
		t.Fatalf("requests = %+v, want the fifth project's key left out", reqs)
	}
}

// ResolveFacts refuses arguments it cannot run with and answers a
// repeated key once, in every position it is named.
func TestResolveFactsArguments(t *testing.T) {
	t.Parallel()
	core := NewCore()
	cwd := t.TempDir()
	if _, err := core.ResolveFacts(t.Context(), cwd, githubFactsPR, PRRevision{}, make([]string, MaxFactKeys+1)); err == nil {
		t.Fatal("ResolveFacts took more than MaxFactKeys keys")
	}
	if _, err := core.ResolveFacts(t.Context(), cwd, PRReference{Forge: "github"}, PRRevision{}, nil); err == nil {
		t.Fatal("ResolveFacts took an invalid PR")
	}
	if _, err := core.ResolveFacts(t.Context(), cwd, githubFactsPR, PRRevision{HeadSHA: "abc"}, nil); err == nil {
		t.Fatal("ResolveFacts took an abbreviated head")
	}
	got, err := core.ResolveFacts(t.Context(), cwd, githubFactsPR, PRRevision{}, []string{"x:1", "", "x:1", "commit:zz"})
	if err != nil {
		t.Fatal(err)
	}
	want := []ForgeFact{{Key: "x:1", Status: FactUnavailable}, {Key: "", Status: FactUnavailable}, {Key: "x:1", Status: FactUnavailable}, {Key: "commit:zz", Status: FactUnavailable}}
	if !reflect.DeepEqual(got.Facts, want) {
		t.Fatalf("facts = %+v", got.Facts)
	}
}

// A fact cache holds at most its entries and bytes, however many keys
// pass through it, and clear empties it.
func TestFactCacheBounds(t *testing.T) {
	t.Parallel()
	at := time.Unix(1_800_000_000, 0)
	for _, tc := range []struct {
		name    string
		cache   *factCache[ForgeFact]
		entries int
		bytes   int64
		fact    func(i int) ForgeFact
	}{
		{"forge", newForgeFactCache(), forgeFactCacheEntries, forgeFactCacheBytes, func(i int) ForgeFact {
			return ForgeFact{Status: FactFound, Title: strings.Repeat("t", 255), URL: fmt.Sprintf("https://github.com/acme/app/issues/%d", i)}
		}},
		{"local lines", newFactCache(localFactCacheEntries, localFactCacheBytes, factBytes), localFactCacheEntries, localFactCacheBytes, func(i int) ForgeFact {
			return ForgeFact{Status: FactFound, Text: strings.Repeat("x", 16<<10), OID: strings.Repeat("a", 40), Path: "a/b.go"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for i := range 10_000 {
				key := fmt.Sprintf("forge\x00github.com\x00fp\x00ref:acme/app#%d", i)
				_, _, _, own := tc.cache.claim(key, at)
				tc.cache.settle(key, own, tc.fact(i), true, true, at.Add(time.Hour))
			}
			entries, bytes := tc.cache.held()
			if entries > tc.entries || bytes > tc.bytes || entries == 0 {
				t.Fatalf("held %d entries, %d bytes; bounds %d, %d", entries, bytes, tc.entries, tc.bytes)
			}
			if len(tc.cache.flights) != 0 {
				t.Fatalf("%d flights left", len(tc.cache.flights))
			}
			// The most recent key is held, the first evicted.
			if _, ok := tc.cache.get("forge\x00github.com\x00fp\x00ref:acme/app#9999", at); !ok {
				t.Fatal("the newest entry was evicted")
			}
			if _, ok := tc.cache.get("forge\x00github.com\x00fp\x00ref:acme/app#0", at); ok {
				t.Fatal("the oldest entry was kept past the bound")
			}
			tc.cache.clear()
			if entries, bytes := tc.cache.held(); entries != 0 || bytes != 0 {
				t.Fatalf("clear left %d entries, %d bytes", entries, bytes)
			}
		})
	}
	t.Run("blob images", func(t *testing.T) {
		t.Parallel()
		_, images := newLocalFactCaches()
		for i := range 10_000 {
			oid := fmt.Sprintf("%040x", i)
			images.put(oid, BlobImage{OID: oid, Size: 1 << 20, MIME: "image/png", Width: 10, Height: 10}, time.Time{})
		}
		if entries, bytes := images.held(); entries > blobImageCacheEntries || bytes > blobImageCacheBytes {
			t.Fatalf("held %d entries, %d bytes; bounds %d, %d", entries, bytes, blobImageCacheEntries, blobImageCacheBytes)
		}
	})
	t.Run("oversized entry", func(t *testing.T) {
		t.Parallel()
		cache := newFactCache(16, 64<<10, factBytes)
		cache.put("k", ForgeFact{Text: strings.Repeat("x", 2<<10)}, time.Time{})
		if entries, _ := cache.held(); entries != 0 {
			t.Fatal("an entry over maxBytes/64 was held")
		}
	})
}

// A forge adapter's resetCaches (a login change) clears its facts.
func TestForgeResetClearsFacts(t *testing.T) {
	t.Parallel()
	f := newFactsForge(t, githubFactsFixture())
	resolveFacts(t, f.core, githubFactsPR, "ref:acme/app#1")
	forge := f.core.forges["github"].(*githubForge)
	if entries, _ := forge.facts.held(); entries != 1 {
		t.Fatalf("held %d facts", entries)
	}
	f.core.ResetForgeCaches()
	if entries, _ := forge.facts.held(); entries != 0 {
		t.Fatalf("ResetForgeCaches left %d facts", entries)
	}
	since := f.seq()
	resolveFacts(t, f.core, githubFactsPR, "ref:acme/app#1")
	if reqs := f.requests(t, since); len(reqs) != 1 {
		t.Fatalf("after the reset: %+v, want the key read again", reqs)
	}

	g := newFactsForge(t, gitlabFactsFixture())
	resolveFacts(t, g.core, gitlabFactsPR, "ref:grp/app#1")
	g.core.ResetForgeCaches()
	if entries, _ := g.core.forges["gitlab"].(*gitlabForge).facts.held(); entries != 0 {
		t.Fatalf("GitLab: ResetForgeCaches left %d facts", entries)
	}
}

// 256 cached keys answer without a request and within a fixed number of
// allocations per key.
func TestForgeFactsCachedAllocations(t *testing.T) {
	f := newFactsForge(t, githubFactsFixture())
	keys := cachedFactKeys()
	resolveFacts(t, f.core, githubFactsPR, keys...)
	since := f.seq()
	cwd := t.TempDir()
	allocs := testing.AllocsPerRun(20, func() {
		if _, err := f.core.ResolveFacts(t.Context(), cwd, githubFactsPR, PRRevision{}, keys); err != nil {
			t.Fatal(err)
		}
	})
	if reqs := f.requests(t, since); len(reqs) != 0 {
		t.Fatalf("cached calls sent %+v", reqs)
	}
	t.Logf("ResolveFacts, 256 cached keys: %.0f allocations", allocs)
	if allocs > 8*float64(len(keys)) {
		t.Fatalf("ResolveFacts allocated %.0f times for %d cached keys", allocs, len(keys))
	}
}

func BenchmarkResolveFactsCached(b *testing.B) {
	f := newFactsForge(b, githubFactsFixture())
	keys := cachedFactKeys()
	resolveFacts(b, f.core, githubFactsPR, keys...)
	cwd := b.TempDir()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := f.core.ResolveFacts(b.Context(), cwd, githubFactsPR, PRRevision{}, keys); err != nil {
			b.Fatal(err)
		}
	}
}

func cachedFactKeys() []string {
	keys := make([]string, 0, MaxFactKeys)
	for i := range MaxFactKeys / 2 {
		keys = append(keys, fmt.Sprintf("ref:acme/app#%d", 1000+i))
	}
	for i := range MaxFactKeys / 2 {
		keys = append(keys, fmt.Sprintf("user:someone%d", i))
	}
	return keys
}
