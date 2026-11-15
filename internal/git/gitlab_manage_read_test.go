package git

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/forgeapi"
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
			return forgeAPIAnswer{Body: "[" + gitlabDiscussion("d1", "ab12cd34") + "]"}
		case gitlabTestProject:
			return forgeAPIAnswer{Body: gitlabTestProjectSettings}
		case "user":
			return forgeAPIAnswer{Body: `{"username":"viewer"}`}
		}
		return forgeUnexpected(t, call)
	})
}

// A management read that fails leaves the rest of the PR read standing: the detail says the
// controls are not shown, offers none and lists nobody.
func TestGitLabManageReadFailureHidesOnlyTheControls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, op string
		answer   forgeAPIAnswer
	}{
		{"read fails", "PRManageChecks", forgeAPIAnswer{Status: http.StatusBadGateway, Body: `{"message":"bad gateway"}`}},
		{"read refused", "PRManageChecks", forgeAPIAnswer{Body: `{"data":{"project":null},"errors":[{"message":"The resource that you are attempting to access does not exist or you don't have permission to perform this action"}]}`}},
		{"read answers no merge request", "PRManageChecks", forgeAPIAnswer{Body: `{"data":{"project":{"mergeRequest":null}}}`}},
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

// gitlabManageAnswer is a PRManageChecks answer (PRManage without the
// checks) for a viewer who may update and administer the merge request:
// bob reviewing in reviewState, the label bug, and the requested-changes
// check in check.
func gitlabManageAnswer(reviewState, check string) string {
	checks := ""
	if check != "" {
		checks = `,"mergeabilityChecks":[{"identifier":"REQUESTED_CHANGES","status":"` + check + `"}]`
	}
	return `{"data":{"project":{"mergeRequest":{"userPermissions":{"updateMergeRequest":true,"adminMergeRequest":true},` +
		`"reviewers":{"nodes":[{"id":"gid://gitlab/User/5","username":"bob","name":"Bob","mergeRequestInteraction":{"reviewState":"` + reviewState + `"}}]},` +
		`"assignees":{"nodes":[]},"labels":{"nodes":[{"id":"gid://gitlab/ProjectLabel/3","title":"bug","color":"#ff0000"}]}` + checks + `}}}}`
}

// gitlabIntrospectionRefused is how a server that refuses introspection
// answers a document asking for __type.
const gitlabIntrospectionRefused = `{"errors":[{"message":"Field '__type' doesn't exist on type 'Query'","locations":[{"line":2,"column":3}],"path":["query PRManageSchema","rereview"],"extensions":{"code":"undefinedField","typeName":"Query","fieldName":"__type"}}]}`

// On a current release, the viewer's controls follow their permissions
// and the merge request's own requested-changes check: the check, not the
// detailed status, says whether requested changes block and whether they
// were bypassed.
func TestGitLabManageControlsFollowTheCheck(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, review, check string
		bypass, bypassed    bool
	}{
		{"check failing", "REQUESTED_CHANGES", "FAILED", true, false},
		{"check passing over requested changes", "REQUESTED_CHANGES", "SUCCESS", false, true},
		{"check passing, nothing requested", "APPROVED", "SUCCESS", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, calls := gitlabManageReadCore(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
				if call.Op == "PRManageChecks" {
					return forgeAPIAnswer{Body: gitlabManageAnswer(tc.review, tc.check)}, true
				}
				return forgeAPIAnswer{}, false
			})
			read, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Detail: true}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			m := read.Detail.Manage
			want := PRManageCan{EditTitle: true, EditDescription: true, Reviewers: true, Assignees: true, Labels: true, Close: true, ConvertToDraft: true, Rerequest: true, Bypass: tc.bypass}
			if m.Can != want {
				t.Fatalf("can = %+v; want %+v", m.Can, want)
			}
			if len(m.Labels) != 1 || m.Labels[0].ID != "gid://gitlab/ProjectLabel/3" || m.Reviewers[0].ID != "5" {
				t.Fatalf("labels %+v, reviewers %+v", m.Labels, m.Reviewers)
			}
			bypassed := slices.ContainsFunc(read.Detail.Readiness.Settled, func(b MergeBlocker) bool { return b.Bypassed })
			if bypassed != tc.bypassed {
				t.Fatalf("settled = %+v, want bypassed %v", read.Detail.Readiness.Settled, tc.bypassed)
			}
			if ops := calls.ops(); slices.Contains(ops, "PRManage") || !slices.Contains(ops, "PRManageChecks") {
				t.Fatalf("operations = %q, want the checks read alone", ops)
			}
		})
	}
}

// gitlabMetadata is a GET metadata answer of a server running version.
func gitlabMetadata(version string) forgeAPIAnswer {
	return forgeAPIAnswer{Body: `{"version":"` + version + `","revision":"abc","enterprise":true}`}
}

// A server at or past gitlabSupportedFloor gets every control its viewer
// may use; an older one is not read: its Details say which release it
// runs and which it needs, and every merge request write is refused
// with that sentence before anything is sent. Nothing introspects, so a
// server refusing introspection loses nothing, and the release is read
// once per host.
func TestGitLabManageFollowsTheSupportedFloor(t *testing.T) {
	t.Parallel()
	full := PRManageCan{EditTitle: true, EditDescription: true, Reviewers: true, Assignees: true, Labels: true, Close: true, ConvertToDraft: true, Rerequest: true, Bypass: true}
	for _, tc := range []struct {
		version, refused string
	}{
		{"19.6.0-ee", ""},
		{"18.0.1", ""},
		{"17.11.3-ee", "GitLab on forge.example runs 17.11; GitLab 18.0 or later is needed."},
		{"15.11.0", "GitLab on forge.example runs 15.11; GitLab 18.0 or later is needed."},
	} {
		t.Run(tc.version, func(t *testing.T) {
			t.Parallel()
			core, calls := gitlabManageReadCore(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
				switch {
				case strings.Contains(call.Query, "__type") || strings.Contains(call.Query, "__schema"):
					return forgeAPIAnswer{Body: gitlabIntrospectionRefused}, true
				case call.Op == "" && call.Path == "metadata":
					return gitlabMetadata(tc.version), true
				case call.Op == "PRManageChecks":
					return forgeAPIAnswer{Body: gitlabManageAnswer("REQUESTED_CHANGES", "FAILED")}, true
				}
				return forgeAPIAnswer{}, false
			})
			ref := gitlabTestRef(7)
			for range 2 {
				read, err := core.ReadPR(t.Context(), ref, PRReadParts{Detail: true}, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				m := read.Detail.Manage
				if tc.refused == "" && (m.Unavailable != "" || len(m.Reviewers) != 1 || len(m.Labels) != 1 || m.Can != full) {
					t.Fatalf("manage = %+v; want every control", m)
				}
				if tc.refused != "" && (m.Unavailable != "Reviewers, assignees, labels and the PR's edit controls are not shown. "+tc.refused || m.Can != (PRManageCan{}) || len(m.Reviewers) != 0) {
					t.Fatalf("manage = %+v; want none and %q", m, tc.refused)
				}
			}
			if want := map[bool]int{true: 2, false: 0}[tc.refused == ""]; countOp(calls, "PRManageChecks") != want {
				t.Fatalf("operations = %q, want %d management reads", calls.ops(), want)
			}
			if n := countPrefix(gitlabPaths(calls), "GET metadata"); n != 1 {
				t.Fatalf("%d release reads, want one per host", n)
			}
			for _, call := range calls.all() {
				if strings.Contains(call.Query, "__type") || strings.Contains(call.Query, "__schema") {
					t.Fatalf("%s introspects the schema", call.Op)
				}
			}
			if tc.refused == "" {
				return
			}
			target := CommentTarget{ThreadID: "d1", CommentID: "1"}
			title := "t"
			writes := len(calls.all())
			for name, run := range map[string]func() error{
				"submit": func() error {
					_, err := core.SubmitReview(t.Context(), ref, SubmitReviewRequest{Verdict: ReviewVerdictComment, HeadSHA: gitlabTestHead, Body: "b"})
					return err
				},
				"line comment": func() error {
					return core.PostPRLineComment(t.Context(), ref, gitlabTestHead, ReviewLineComment{Path: "a.go", Line: new(1), Side: "RIGHT", Body: "b"})
				},
				"comment":        func() error { _, err := core.PostPRComment(t.Context(), ref, "b"); return err },
				"edit comment":   func() error { return core.EditPRComment(t.Context(), ref, target, "b") },
				"delete comment": func() error { return core.DeletePRComment(t.Context(), ref, target) },
				"reply":          func() error { return core.ReplyToThread(t.Context(), ref, "d1", 0, "b") },
				"resolve":        func() error { return core.SetThreadResolved(t.Context(), ref, "d1", true) },
				"reaction":       func() error { return core.SetPRCommentReaction(t.Context(), ref, target, "thumbsup", true) },
				"suggestion": func() error {
					return core.ApplyPRSuggestion(t.Context(), ref, SuggestionApply{Target: target, SuggestionID: "1", Digest: strings.Repeat("a", 64), HeadSHA: gitlabTestHead})
				},
				"edit": func() error { return core.EditPR(t.Context(), ref, PREdit{Title: &title}) },
				"reviewers": func() error {
					return core.SetPRPeople(t.Context(), ref, true, PRPeopleChange{Add: []PRPerson{{Login: "bob", ID: "5"}}})
				},
				"labels": func() error {
					return core.SetPRLabels(t.Context(), ref, PRLabelChange{Add: []PRLabel{{ID: "gid://gitlab/ProjectLabel/9", Name: "ui"}}})
				},
				"state":     func() error { return core.SetPRState(t.Context(), ref, PRStateClose) },
				"rerequest": func() error { return core.RerequestPRReview(t.Context(), ref, PRPerson{Login: "bob", ID: "5"}) },
				"bypass":    func() error { return core.BypassPRRequestedChanges(t.Context(), ref) },
				"approve":   func() error { return core.ApprovePR(t.Context(), ref, gitlabTestHead) },
				"unapprove": func() error { return core.UnapprovePR(t.Context(), ref) },
				"merge": func() error {
					_, err := core.MergePR(t.Context(), ref, MergeRequest{Method: "merge", HeadSHA: gitlabTestHead})
					return err
				},
				"cancel auto-merge": func() error { return core.CancelPRAutoMerge(t.Context(), ref) },
				"update branch":     func() error { return core.UpdatePRBranch(t.Context(), ref, gitlabTestHead) },
				"ready":             func() error { return core.MarkPRReady(t.Context(), ref) },
			} {
				err := run()
				if explained, ok := errors.AsType[*ForgeActionError](err); !ok || explained.Kind != ForgeErrorRefused || explained.Message != tc.refused || !forgeapi.WriteNotApplied(err) {
					t.Fatalf("%s = %v, want refused with %q and not applied", name, err, tc.refused)
				}
			}
			if sent := calls.all()[writes:]; len(sent) != 0 {
				t.Fatalf("a refused write sent %s %s", sent[0].Method, sent[0].Path)
			}
		})
	}
}

// A release read that fails takes the Details with it, saying why, and
// a write is not sent and not reported as one GitLab may have applied;
// the next read asks again.
func TestGitLabManageWithoutTheRelease(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	fail := true
	core, calls := gitlabManageReadCore(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case call.Op == "" && call.Path == "metadata" && fail:
			return forgeAPIAnswer{Status: http.StatusBadGateway, Body: `{"message":"502"}`}, true
		case call.Op == "" && call.Path == "metadata":
			return gitlabMetadata("19.6.0-ee"), true
		case call.Op == "PRManageChecks":
			return forgeAPIAnswer{Body: gitlabManageAnswer("APPROVED", "SUCCESS")}, true
		}
		return forgeAPIAnswer{}, false
	})
	read := func() PRManage {
		t.Helper()
		got, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Detail: true}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return got.Detail.Manage
	}
	if m := read(); m.Unavailable == "" || m.Can != (PRManageCan{}) || countOp(calls, "PRManageChecks") != 0 {
		t.Fatalf("manage without the release = %+v", m)
	}
	err := core.RerequestPRReview(t.Context(), gitlabTestRef(7), PRPerson{Login: "bob", ID: "5"})
	if _, unconfirmed := errors.AsType[*UnconfirmedWriteError](err); err == nil || unconfirmed || !forgeapi.WriteNotApplied(err) || countOp(calls, "PRRereview") != 0 {
		t.Fatalf("re-request without the release = %v, %d sent", err, countOp(calls, "PRRereview"))
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if m := read(); !m.Can.Reviewers || m.Unavailable != "" || countOp(calls, "PRManageChecks") != 1 {
		t.Fatalf("after the release answered: %+v", m)
	}
}

// The release is parsed from what GET metadata answers.
func TestParseGitLabVersion(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]gitlabVersion{"17.9.2-ee": {17, 9}, "18.0.0-pre": {18, 0}, "16.11.10": {16, 11}, " 19.3.0 ": {19, 3}, "18.10": {18, 10}} {
		if got, err := parseGitLabVersion(in); err != nil || got != want {
			t.Errorf("parseGitLabVersion(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "18", "x.y", "-1.0", "18.x"} {
		if _, err := parseGitLabVersion(in); err == nil {
			t.Errorf("parseGitLabVersion(%q) accepted", in)
		}
	}
}

// A refusal of anything but the element asked about, or one beside data,
// is not a schema gap.
func TestGitLabSchemaLacksNamesTheElement(t *testing.T) {
	t.Parallel()
	gqlErr := func(data, message string) error {
		return &forgeapi.GraphQLError{Problems: []forgeapi.GraphQLProblem{{Message: message}}, Data: json.RawMessage(data)}
	}
	for _, tc := range []struct {
		err     error
		element string
		want    bool
	}{
		{gqlErr("", "Field 'mergeabilityChecks' doesn't exist on type 'MergeRequest'"), "mergeabilityChecks", true},
		{gqlErr("", "InputObject 'MergeRequestSetLabelsInput' doesn't accept argument 'operationMode'"), "operationMode", true},
		{gqlErr("", "Field 'mergeRequestReviewerRereview' doesn't exist on type 'Mutation'"), "mergeRequestReviewerRereview", true},
		{gqlErr("", "Field '__type' doesn't exist on type 'Query'"), "mergeabilityChecks", false},
		{gqlErr("", "Field 'mergeabilityChecksV2' doesn't exist on type 'MergeRequest'"), "mergeabilityChecks", false},
		{gqlErr(`{"x":1}`, "Field 'mergeabilityChecks' doesn't exist on type 'MergeRequest'"), "mergeabilityChecks", false},
		{errors.New("Field 'mergeabilityChecks' doesn't exist on type 'MergeRequest'"), "mergeabilityChecks", false},
	} {
		if got := gitlabSchemaLacks(tc.err, tc.element); got != tc.want {
			t.Errorf("gitlabSchemaLacks(%v, %q) = %v, want %v", tc.err, tc.element, got, tc.want)
		}
	}
}

// A harness reset forgets each host's release and each merge request's
// state events backoff: the next read asks the release again and reads
// the state events at once.
func TestResetForgeCachesForgetsTheReleaseAndBackoff(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	version, eventsFail, eventReads := "17.11.0", true, 0
	core, calls := gitlabManageReadCore(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case call.Op == "" && call.Path == "metadata":
			return gitlabMetadata(version), true
		case call.Op == "PRManageChecks":
			return forgeAPIAnswer{Body: gitlabManageAnswer("APPROVED", "")}, true
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
	read()
	mu.Lock()
	version, eventsFail = "19.6.0", false
	mu.Unlock()
	read()
	mu.Lock()
	reads := eventReads
	mu.Unlock()
	if checks := countOp(calls, "PRManageChecks"); checks != 0 || reads != 1 {
		t.Fatalf("warm: %d management reads on a cached unsupported release, %d state events reads", checks, reads)
	}
	core.ResetForgeCaches()
	got := read()
	mu.Lock()
	reads = eventReads
	mu.Unlock()
	if countOp(calls, "PRManageChecks") != 1 || countPrefix(gitlabPaths(calls), "GET metadata") != 2 {
		t.Fatalf("after the reset: %q", calls.ops())
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
	gl.versions.put("k", gitlabVersion{19, 3}, now)
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
	_, schema := gl.versions.get("k", now)
	due, _ := gl.stateEventRetries.due("k", now)
	_, _, known := gl.pipelineTags.beginListing("k", []int64{1})
	_, _, permission := gh.ciPermissions.get("k", now)
	_, _, artifacts := gh.ciArtifacts.get("k", now)
	if project || viewer || approval || schema || gl.routes.get("k") || !due || len(known) != 0 || permission || artifacts {
		t.Fatalf("kept: project %v viewer %v approval %v schema %v route %v backoff %v tags %v permission %v artifacts %v",
			project, viewer, approval, schema, gl.routes.get("k"), !due, known, permission, artifacts)
	}
}
