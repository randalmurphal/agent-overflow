package git

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// gitlabManageReadCore is a Core over a GitLab answering one merge
// request (7, viewer may merge) with a discussion, the project and the
// viewer, and every management and state events request through manage.
func gitlabManageReadCore(t *testing.T, manage func(call forgeAPICall) (forgeAPIAnswer, bool)) (*Core, *forgeAPICalls) {
	t.Helper()
	mr := gitlabTestProject + "/merge_requests/7"
	return newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if reply, ok := manage(call); ok {
			return reply
		}
		if reply, ok := gitlabManageExtras(call); ok {
			return reply
		}
		switch call.Path {
		case mr:
			return forgeAPIAnswer{Body: strings.Replace(gitlabTestMR(7, 0), `{"iid":`, `{"user":{"can_merge":true},"iid":`, 1)}
		case mr + "/approvals":
			return forgeAPIAnswer{Body: `{"approved_by":[]}`}
		case mr + "/discussions?per_page=100":
			return forgeAPIAnswer{Body: "[" + gitlabDiscussion("d1", "head2") + "]"}
		case gitlabTestProject:
			return forgeAPIAnswer{Body: gitlabTestProjectSettings}
		case "user":
			return forgeAPIAnswer{Body: `{"username":"viewer"}`}
		}
		return forgeUnexpected(t, call)
	})
}

// A management read that fails, at the schema probe or at the read
// itself, leaves the rest of the PR read standing: the detail says the
// controls are not shown, offers none and lists nobody.
func TestGitLabManageReadFailureHidesOnlyTheControls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, op string
		answer   forgeAPIAnswer
	}{
		{"schema probe fails", "PRManageSchema", forgeAPIAnswer{Status: http.StatusBadGateway, Body: `{"message":"bad gateway"}`}},
		{"read refused", "PRManage", forgeAPIAnswer{Body: `{"data":{"project":null},"errors":[{"message":"The resource that you are attempting to access does not exist or you don't have permission to perform this action"}]}`}},
		{"read answers no merge request", "PRManage", forgeAPIAnswer{Body: `{"data":{"project":{"mergeRequest":null}}}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, _ := gitlabManageReadCore(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
				if call.Op == tc.op {
					return tc.answer, true
				}
				return forgeAPIAnswer{}, false
			})
			read, err := core.ReadPR(t.Context(), gitlabTestRef(7), allParts, nil, nil)
			if err != nil {
				t.Fatalf("ReadPR = %v, want the PR without its controls", err)
			}
			m := read.Detail.Manage
			if read.Detail.Number != 7 || len(read.Threads) != 1 {
				t.Fatalf("detail %d, threads %d: the PR was hidden", read.Detail.Number, len(read.Threads))
			}
			if !strings.HasPrefix(m.Unavailable, "Reviewers, assignees, labels and the PR's edit controls are not shown") ||
				m.Can != (PRManageCan{}) || len(m.Reviewers)+len(m.Assignees)+len(m.Labels) != 0 {
				t.Fatalf("manage = %+v", m)
			}
		})
	}
}

// The state events read is at most three requests, oldest first: every
// page within the bound; past it, by X-Total-Pages, page 1 and the last
// two; without a total, the first three. A reopen is "opened"
// in the state events API and "reopened" on some servers.
func TestGitLabStateEventsReadTheNewestPages(t *testing.T) {
	t.Parallel()
	events := gitlabTestProject + "/merge_requests/7/resource_state_events?"
	page := func(id int, state string) string {
		return `[{"id":` + strconv.Itoa(id) + `,"user":{"username":"u"},"created_at":"t","state":"` + state + `"}]`
	}
	for _, tc := range []struct {
		name  string
		pages map[string]forgeAPIAnswer
		want  []string
		reads []string
	}{
		{"within the bound", map[string]forgeAPIAnswer{
			"per_page=100":        {Body: page(1, "closed"), Header: http.Header{"X-Next-Page": {"2"}}},
			"page=2&per_page=100": {Body: page(2, "reopened"), Header: http.Header{"X-Next-Page": {""}}},
		}, []string{"state:1 closed", "state:2 reopened"}, []string{"per_page=100", "page=2&per_page=100"}},
		{"past the bound", map[string]forgeAPIAnswer{
			"per_page=100":        {Body: page(1, "closed"), Header: http.Header{"X-Total-Pages": {"5"}}},
			"page=4&per_page=100": {Body: page(4, "opened")},
			"page=5&per_page=100": {Body: page(5, "merged")},
		}, []string{"state:1 closed", "state:4 reopened", "state:5 merged"}, []string{"per_page=100", "page=4&per_page=100", "page=5&per_page=100"}},
		{"past the bound, without a total", map[string]forgeAPIAnswer{
			"per_page=100":        {Body: page(1, "closed"), Header: http.Header{"X-Next-Page": {"2"}}},
			"page=2&per_page=100": {Body: page(2, "opened"), Header: http.Header{"X-Next-Page": {"3"}}},
			"page=3&per_page=100": {Body: page(3, "closed"), Header: http.Header{"X-Next-Page": {"4"}}},
		}, []string{"state:1 closed", "state:2 reopened", "state:3 closed"}, []string{"per_page=100", "page=2&per_page=100", "page=3&per_page=100"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, calls := gitlabManageReadCore(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
				query, ok := strings.CutPrefix(call.Path, events)
				if !ok {
					return forgeAPIAnswer{}, false
				}
				answer, ok := tc.pages[query]
				if !ok {
					t.Errorf("unexpected state events page %q", query)
				}
				return answer, true
			})
			read, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Events: true}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, event := range read.Events {
				if strings.HasPrefix(event.ID, "state:") {
					got = append(got, event.ID+" "+event.Kind)
				}
			}
			var reads []string
			for _, call := range calls.all() {
				if query, ok := strings.CutPrefix(call.Path, events); ok {
					reads = append(reads, query)
				}
			}
			if !slices.Equal(got, tc.want) || !slices.Equal(reads, tc.reads) {
				t.Fatalf("state events %q from pages %q, want %q from %q", got, reads, tc.want, tc.reads)
			}
		})
	}
}

// A state events read that fails, on its first or a later page, leaves
// the events without them, and is not asked again until its retry is
// due; the wait doubles per failure and a success forgets it.
func TestGitLabStateEventsFailureIsBackedOff(t *testing.T) {
	t.Parallel()
	for _, failing := range []string{"per_page=100", "page=2&per_page=100"} {
		t.Run(failing, func(t *testing.T) {
			t.Parallel()
			events := gitlabTestProject + "/merge_requests/7/resource_state_events?"
			var mu sync.Mutex
			broken, reads := true, 0
			core, _ := gitlabManageReadCore(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
				query, ok := strings.CutPrefix(call.Path, events)
				if !ok {
					return forgeAPIAnswer{}, false
				}
				mu.Lock()
				defer mu.Unlock()
				if query == "per_page=100" {
					reads++
				}
				if broken && query == failing {
					return forgeAPIAnswer{Status: http.StatusInternalServerError, Body: `{"message":"500"}`}, true
				}
				if query == "per_page=100" {
					return forgeAPIAnswer{Body: `[{"id":1,"user":{"username":"u"},"created_at":"t","state":"closed"}]`, Header: http.Header{"X-Next-Page": {"2"}}}, true
				}
				return forgeAPIAnswer{Body: `[{"id":2,"user":{"username":"u"},"created_at":"t","state":"opened"}]`}, true
			})
			now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
			core.nowFn = func() time.Time { return now }
			stateKinds := func() []string {
				t.Helper()
				read, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Events: true}, nil, nil)
				if err != nil {
					t.Fatalf("ReadPR = %v, want the events without the state events", err)
				}
				var out []string
				for _, event := range read.Events {
					if strings.HasPrefix(event.ID, "state:") {
						out = append(out, event.Kind)
					}
				}
				return out
			}
			readsSoFar := func() int {
				mu.Lock()
				defer mu.Unlock()
				return reads
			}
			if got := stateKinds(); len(got) != 0 || readsSoFar() != 1 {
				t.Fatalf("failing read: state events %q, reads %d", got, readsSoFar())
			}
			now = now.Add(prLookupErrorBase - time.Second)
			if stateKinds(); readsSoFar() != 1 {
				t.Fatalf("reads before the retry is due = %d", readsSoFar())
			}
			now = now.Add(time.Second)
			if stateKinds(); readsSoFar() != 2 {
				t.Fatalf("reads once due = %d", readsSoFar())
			}
			// The second failure waits twice the first.
			now = now.Add(2*prLookupErrorBase - time.Second)
			if stateKinds(); readsSoFar() != 2 {
				t.Fatalf("reads inside the doubled wait = %d", readsSoFar())
			}
			now = now.Add(time.Second)
			mu.Lock()
			broken = false
			mu.Unlock()
			if got := stateKinds(); !slices.Equal(got, []string{EventClosed, EventReopened}) || readsSoFar() != 3 {
				t.Fatalf("after recovery: state events %q, reads %d", got, readsSoFar())
			}
			if stateKinds(); readsSoFar() != 4 {
				t.Fatalf("a success kept the retry: reads %d", readsSoFar())
			}
			// A failure after the success waits the first wait again.
			mu.Lock()
			broken = true
			mu.Unlock()
			stateKinds()
			now = now.Add(prLookupErrorBase)
			if stateKinds(); readsSoFar() != 6 {
				t.Fatalf("a failure after a success waited longer than the first wait: reads %d", readsSoFar())
			}
		})
	}
}

// The viewer's controls follow the host's schema and the merge request's
// own requested-changes check: a host without a set mutation offers no
// control for it, and the check, not the detailed status, says whether
// requested changes block and whether they were bypassed.
func TestGitLabManageControlsFollowSchemaAndCheck(t *testing.T) {
	t.Parallel()
	// schema is a host with the override and the re-review mutation,
	// and with operationMode on the given set mutations only.
	schema := func(reviewers, assignees, labels bool) string {
		input := func(has bool) string {
			if has {
				return `{"inputFields":[{"name":"operationMode"}]}`
			}
			return `{"inputFields":[]}`
		}
		return `{"data":{"rereview":{"name":"x"},"updateInput":{"inputFields":[{"name":"overrideRequestedChanges"}]},` +
			`"setReviewers":` + input(reviewers) + `,"setAssignees":` + input(assignees) + `,"setLabels":` + input(labels) + `}}`
	}
	manage := func(reviewState, check string) string {
		return `{"data":{"project":{"mergeRequest":{"userPermissions":{"updateMergeRequest":true,"adminMergeRequest":true},` +
			`"reviewers":{"nodes":[{"id":"gid://gitlab/User/5","username":"bob","name":"Bob","mergeRequestInteraction":{"reviewState":"` + reviewState + `"}}]},` +
			`"assignees":{"nodes":[]},"labels":{"nodes":[{"id":"gid://gitlab/ProjectLabel/3","title":"bug","color":"#ff0000"}]},` +
			`"mergeabilityChecks":[{"identifier":"REQUESTED_CHANGES","status":"` + check + `"}]}}}}`
	}
	for _, tc := range []struct {
		name, review, check string
		bypass, bypassed    bool
		sets                [3]bool
	}{
		{"check failing", "REQUESTED_CHANGES", "FAILED", true, false, [3]bool{true, false, true}},
		{"check passing over requested changes", "REQUESTED_CHANGES", "SUCCESS", false, true, [3]bool{true, false, true}},
		{"check passing, nothing requested", "APPROVED", "SUCCESS", false, false, [3]bool{false, true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, calls := gitlabManageReadCore(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
				switch call.Op {
				case "PRManageSchema":
					return forgeAPIAnswer{Body: schema(tc.sets[0], tc.sets[1], tc.sets[2])}, true
				case "PRManageChecks":
					return forgeAPIAnswer{Body: manage(tc.review, tc.check)}, true
				}
				return forgeAPIAnswer{}, false
			})
			read, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Detail: true}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			m := read.Detail.Manage
			want := PRManageCan{EditTitle: true, EditDescription: true, Reviewers: tc.sets[0], Assignees: tc.sets[1], Labels: tc.sets[2], Close: true, ConvertToDraft: true, Rerequest: true, Bypass: tc.bypass}
			if m.Can != want {
				t.Fatalf("can = %+v, want %+v", m.Can, want)
			}
			if len(m.Labels) != 1 || m.Labels[0].ID != "gid://gitlab/ProjectLabel/3" || m.Reviewers[0].ID != "5" {
				t.Fatalf("labels %+v, reviewers %+v", m.Labels, m.Reviewers)
			}
			bypassed := slices.ContainsFunc(read.Detail.Readiness.Settled, func(b MergeBlocker) bool { return b.Bypassed })
			if bypassed != tc.bypassed {
				t.Fatalf("settled = %+v, want bypassed %v", read.Detail.Readiness.Settled, tc.bypassed)
			}
			if ops := calls.ops(); slices.Contains(ops, "PRManage") || !slices.Contains(ops, "PRManageChecks") {
				t.Fatalf("operations = %q, want the checks read on a host with the override", ops)
			}
		})
	}
}

// A harness reset forgets each host's schema answer and each merge
// request's state events backoff: the next read probes again and reads
// the state events at once.
func TestResetForgeCachesForgetsSchemaAndBackoff(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	schema, eventsFail, eventReads := gitlabSchemaBare, true, 0
	core, calls := gitlabManageReadCore(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case call.Op == "PRManageSchema":
			return forgeAPIAnswer{Body: schema}, true
		case call.Op == "PRManageChecks":
			return forgeAPIAnswer{Body: `{"data":{"project":{"mergeRequest":{"userPermissions":{"updateMergeRequest":true,"adminMergeRequest":true},` +
				`"reviewers":{"nodes":[]},"assignees":{"nodes":[]},"labels":{"nodes":[]},"mergeabilityChecks":[]}}}}`}, true
		case strings.Contains(call.Path, "/resource_state_events"):
			eventReads++
			if eventsFail {
				return forgeAPIAnswer{Status: http.StatusInternalServerError, Body: `{"message":"500"}`}, true
			}
			return forgeAPIAnswer{Body: `[{"id":1,"user":{"username":"u"},"created_at":"t","state":"closed"}]`}, true
		}
		return forgeAPIAnswer{}, false
	})
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	core.nowFn = func() time.Time { return now }
	read := func() PRRead {
		t.Helper()
		out, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Detail: true, Events: true}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := read().Detail.Manage.Can; got.Reviewers || got.Rerequest {
		t.Fatalf("a bare server's controls = %+v", got)
	}
	read()
	mu.Lock()
	schema, eventsFail = gitlabSchemaAll, false
	reads := eventReads
	mu.Unlock()
	if probes := countOp(calls, "PRManageSchema"); probes != 1 || reads != 1 {
		t.Fatalf("warm: %d probes, %d state events reads", probes, reads)
	}
	core.ResetForgeCaches()
	got := read()
	mu.Lock()
	reads = eventReads
	mu.Unlock()
	if !got.Detail.Manage.Can.Reviewers || !got.Detail.Manage.Can.Rerequest || countOp(calls, "PRManageSchema") != 2 {
		t.Fatalf("after the reset: controls %+v, %d probes", got.Detail.Manage.Can, countOp(calls, "PRManageSchema"))
	}
	closed := slices.ContainsFunc(got.Events, func(e ReviewEvent) bool { return e.ID == "state:1" })
	if reads != 2 || !closed {
		t.Fatalf("after the reset: %d state events reads, close shown %v", reads, closed)
	}
	// The project's merge settings and the viewer are read again too.
	paths := gitlabPaths(calls)
	if n, v := countPrefix(paths, "GET "+gitlabTestProject), countPrefix(paths, "GET user"); n != 2 || v != 2 {
		t.Fatalf("project reads %d, viewer reads %d over a reset, want 2 each", n, v)
	}
}

func countOp(calls *forgeAPICalls, op string) int {
	n := 0
	for _, call := range calls.all() {
		if call.Op == op {
			n++
		}
	}
	return n
}

// ResetForgeCaches reaches every cache of both adapters.
func TestResetForgeCachesReachesEveryAdapterCache(t *testing.T) {
	t.Parallel()
	core := NewCore()
	now := time.Now()
	gl := core.forges["gitlab"].(*gitlabForge)
	gh := core.forges["github"].(*githubForge)
	gl.projects.put("k", gitlabProjectSettings{}, now)
	gl.viewers.put("k", gitlabViewer{}, now)
	gl.noApprovalState.put("k", true, now)
	gl.noCIJobCancelPermission.put("k", true, now)
	gl.schemas.put("k", gitlabSchema{}, now)
	gl.routes.remember("k")
	gl.stateEventRetries.failed("k", gitlabStateEventsRetry{failures: 1, due: now.Add(time.Hour)})
	state, start, _ := gl.pipelineTags.beginListing("k", []int64{1})
	gl.pipelineTags.endListing("k", state, start, map[int64]bool{1: true})
	gh.ciPermissions.put("k", CIAction(""), now.Add(time.Hour))
	gh.ciArtifacts.put("k", true, now.Add(time.Hour))

	core.ResetForgeCaches()

	_, project := gl.projects.get("k", now)
	_, viewer := gl.viewers.get("k", now)
	_, approval := gl.noApprovalState.get("k", now)
	_, cancel := gl.noCIJobCancelPermission.get("k", now)
	_, schema := gl.schemas.get("k", now)
	due, _ := gl.stateEventRetries.due("k", now)
	_, _, known := gl.pipelineTags.beginListing("k", []int64{1})
	_, _, permission := gh.ciPermissions.get("k", now)
	_, _, artifacts := gh.ciArtifacts.get("k", now)
	if project || viewer || approval || cancel || schema || gl.routes.get("k") || !due || len(known) != 0 || permission || artifacts {
		t.Fatalf("kept: project %v viewer %v approval %v cancel %v schema %v route %v backoff %v tags %v permission %v artifacts %v",
			project, viewer, approval, cancel, schema, gl.routes.get("k"), !due, known, permission, artifacts)
	}
}

// Two cold reads of one host share one schema probe; a waiter whose
// context ends gets its own error while the probe answers the other.
func TestGitLabColdReadsShareOneSchemaProbe(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	core, calls := gitlabManageReadCore(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		if call.Op == "PRManageSchema" {
			started <- struct{}{}
			<-release
			return forgeAPIAnswer{Body: gitlabSchemaAll}, true
		}
		if call.Op == "PRManageChecks" {
			return forgeAPIAnswer{Body: `{"data":{"project":{"mergeRequest":{"userPermissions":{"updateMergeRequest":true,"adminMergeRequest":true},` +
				`"reviewers":{"nodes":[]},"assignees":{"nodes":[]},"labels":{"nodes":[]},"mergeabilityChecks":[]}}}}`}, true
		}
		return forgeAPIAnswer{}, false
	})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	// Runs before the server's own cleanup, which waits for its handlers.
	t.Cleanup(unblock)
	f := core.forges["gitlab"].(*gitlabForge)
	client, err := core.gitlabAPI(testForgeHost)
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		schema gitlabSchema
		err    error
	}
	first := make(chan answer, 1)
	go func() {
		schema, err := f.manageSchema(t.Context(), client, testForgeHost)
		first <- answer{schema, err}
	}()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	second := make(chan answer, 1)
	go func() {
		schema, err := f.manageSchema(ctx, client, testForgeHost)
		second <- answer{schema, err}
	}()
	third := make(chan answer, 1)
	go func() {
		schema, err := f.manageSchema(t.Context(), client, testForgeHost)
		third <- answer{schema, err}
	}()
	waitFor(t, func() bool { return f.probes.waiters(testForgeHost) >= 3 })
	cancel()
	if got := within(t, second); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("a waiter whose context ended = %+v", got)
	}
	unblock()
	for _, got := range []answer{within(t, first), within(t, third)} {
		if got.err != nil || !got.schema.setReviewers {
			t.Fatalf("a waiter = %+v", got)
		}
	}
	if n := countOp(calls, "PRManageSchema"); n != 1 {
		t.Fatalf("probes = %d, want one for the three", n)
	}
}

// The probe outlives the caller that started it: its waiters get the
// answer when that caller gives up.
func TestGitLabSchemaProbeOutlivesItsFirstCaller(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	core, calls := gitlabManageReadCore(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		if call.Op == "PRManageSchema" {
			started <- struct{}{}
			<-release
			return forgeAPIAnswer{Body: gitlabSchemaAll}, true
		}
		return forgeAPIAnswer{}, false
	})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	// Runs before the server's own cleanup, which waits for its handlers.
	t.Cleanup(unblock)
	f := core.forges["gitlab"].(*gitlabForge)
	client, err := core.gitlabAPI(testForgeHost)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() {
		_, err := f.manageSchema(ctx, client, testForgeHost)
		first <- err
	}()
	<-started
	second := make(chan error, 1)
	go func() {
		_, err := f.manageSchema(t.Context(), client, testForgeHost)
		second <- err
	}()
	waitFor(t, func() bool { return f.probes.waiters(testForgeHost) >= 2 })
	cancel()
	if err := within(t, first); !errors.Is(err, context.Canceled) {
		t.Fatalf("the first caller = %v", err)
	}
	unblock()
	if err := within(t, second); err != nil {
		t.Fatalf("the waiter after the first gave up = %v", err)
	}
	if n := countOp(calls, "PRManageSchema"); n != 1 {
		t.Fatalf("probes = %d", n)
	}
}

// waitFor waits up to ten seconds for done.
func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(time.Millisecond)
	}
}

// within receives from ch, failing after ten seconds.
func within[V any](t *testing.T, ch <-chan V) V {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an answer")
		var zero V
		return zero
	}
}
