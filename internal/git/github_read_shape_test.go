package git

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/forgeapi"
)

// A PRTick refused for one optional part is sent again without it: the
// PR publishes, the detail says what is not shown and why, and a refusal
// of the token (FORBIDDEN, INSUFFICIENT_SCOPES, a document refused before
// it ran) is remembered for the repository and login, so the next read
// leaves the part out at once, until the TTL or a cache reset. An error
// outside every optional part fails the read.
func TestGitHubReadLeavesOutARefusedPart(t *testing.T) {
	t.Parallel()
	checkRunsPath := `["repository","pullRequest","commits","nodes",0,"commit","statusCheckRollup","contexts","nodes",0]`
	for _, tc := range []struct {
		name     string
		part     githubReadPart // the part whose selection draws the errors; "" draws them always
		errors   string
		dataless bool
		header   http.Header
		// fails is set when the read must fail; otherwise remembered says
		// whether the next read leaves the part out at once, manage and
		// unavailable are prefixes of the sentences.
		fails       bool
		remembered  bool
		manage      string
		unavailable string
		checksRow   bool
	}{
		{name: "labels refused", part: githubPartManage,
			errors:     `[{"type":"FORBIDDEN","message":"Resource not accessible by integration","path":["repository","pullRequest","labels"]}]`,
			remembered: true, manage: "Reviewers, assignees, labels and the PR's edit controls are not shown: forge.example refused them (Resource not accessible by integration)", checksRow: true},
		{name: "a team reviewer", part: githubPartTeams,
			errors:     `[{"type":"FORBIDDEN","message":"Resource not accessible by integration","path":["repository","pullRequest","reviewRequests","nodes",0,"requestedReviewer"]}]`,
			remembered: true, unavailable: "Team reviewers are not shown: forge.example refused them", checksRow: true},
		{name: "a token without read:org", part: githubPartTeams, dataless: true,
			errors:     `[{"type":"INSUFFICIENT_SCOPES","message":"Your token has not been granted the required scopes to execute this query. The 'combinedSlug' field requires one of the following scopes: ['read:org'], but your token has only been granted the: ['repo'] scopes. Please modify your token's scopes at: https://github.com/settings/tokens."}]`,
			remembered: true, unavailable: "Team reviewers are not shown: your forge.example token lacks the `read:org` scope.", checksRow: true},
		{name: "check runs a fine-grained token cannot read", part: githubPartCheckRuns,
			errors:     `[{"type":"FORBIDDEN","message":"Resource not accessible by personal access token","path":` + checkRunsPath + `}]`,
			remembered: true, unavailable: "Check runs are not shown: forge.example does not let this fine-grained token read them."},
		{name: "workflow runs", part: githubPartWorkflowRuns,
			errors:     `[{"type":"FORBIDDEN","message":"Resource not accessible by integration","path":` + strings.TrimSuffix(checkRunsPath, "]") + `,"checkSuite"]}]`,
			remembered: true, unavailable: "Workflow runs (each check's workflow, log and re-run) are not shown", checksRow: true},
		{name: "a passing error under labels", part: githubPartManage,
			errors: `[{"message":"Something went wrong","path":["repository","pullRequest","labels"]}]`,
			manage: "Reviewers, assignees, labels and the PR's edit controls are not shown: forge.example refused them (Something went wrong)", checksRow: true},
		{name: "an error outside the parts too", part: githubPartManage,
			errors: `[{"type":"FORBIDDEN","message":"no","path":["repository","pullRequest","labels"]},{"message":"no","path":["repository","pullRequest","latestReviews"]}]`, fails: true},
		{name: "an error with no path", errors: `[{"message":"Something went wrong"}]`, fails: true},
		{name: "single sign-on", errors: `[{"type":"FORBIDDEN","message":"Resource protected by organization SAML enforcement.","path":["repository","pullRequest","labels"]}]`,
			header: http.Header{"X-Github-Sso": {"required; url=https://github.com/orgs/o/sso?authorization_request=x"}}, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
				if call.Op != "PRTick" {
					return forgeUnexpected(t, call)
				}
				run := githubRun("build", "COMPLETED", "SUCCESS", true)
				if call.Vars[string(githubPartCheckRuns)] != true {
					run = map[string]any{"__typename": "CheckRun"}
				}
				body := githubTickJSON(t, githubMergeRepo(nil), githubMergePull(map[string]any{"commits": githubRollup(run), "viewerCanUpdate": true}))
				switch {
				case tc.part != "" && call.Vars[string(tc.part)] != true:
				case tc.dataless:
					body = `{"errors":` + tc.errors + `}`
				default:
					body = body[:len(body)-1] + `,"errors":` + tc.errors + `}`
				}
				return forgeAPIAnswer{Body: body, Header: tc.header}
			})
			sent := 0
			reads := func() []forgeAPICall {
				all := calls.all()
				out := all[sent:]
				sent = len(all)
				return out
			}
			read, err := core.ReadPR(t.Context(), githubTestRef(7), PRReadParts{Detail: true}, nil, nil)
			if tc.fails {
				if err == nil {
					t.Fatal("the read published")
				}
				if got := len(reads()); got != 1 {
					t.Fatalf("requests = %d, want the one that failed", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadPR = %v", err)
			}
			if got := reads(); len(got) != 2 || got[0].Vars[string(tc.part)] != true || got[1].Vars[string(tc.part)] != false {
				t.Fatalf("requests = %+v, want the read then the read without %s", got, tc.part)
			}
			for _, part := range githubReadParts {
				if part != tc.part && slices.ContainsFunc(calls.all(), func(call forgeAPICall) bool { return call.Vars[string(part)] != true }) {
					t.Fatalf("%s was left out too", part)
				}
			}
			d := read.Detail
			if d.Number != 7 || d.HeadSHA != "abc1234" {
				t.Fatalf("detail = %+v", d)
			}
			if !strings.HasPrefix(d.Manage.Unavailable, tc.manage) || (tc.manage == "") != (d.Manage.Unavailable == "") {
				t.Fatalf("manage unavailable = %q, want %q", d.Manage.Unavailable, tc.manage)
			}
			if tc.manage != "" && (d.Manage.Can != PRManageCan{} || len(d.Manage.Reviewers)+len(d.Manage.Labels) != 0) {
				t.Fatalf("manage = %+v, want no controls", d.Manage)
			}
			if tc.manage == "" && !d.Manage.Can.EditTitle {
				t.Fatalf("manage = %+v, want the controls", d.Manage)
			}
			if tc.part == githubPartTeams && d.Manage.Can.ReviewerTeams {
				t.Fatal("team reviewers are offered without teams")
			}
			if tc.unavailable == "" && len(d.Unavailable) != 0 || tc.unavailable != "" && (len(d.Unavailable) != 1 || !strings.HasPrefix(d.Unavailable[0], tc.unavailable)) {
				t.Fatalf("unavailable = %q, want %q", d.Unavailable, tc.unavailable)
			}
			hasChecksRow := slices.ContainsFunc(append(d.Readiness.Blockers, d.Readiness.Settled...), func(b MergeBlocker) bool { return b.Kind == BlockerChecks })
			if hasChecksRow != tc.checksRow {
				t.Fatalf("readiness = %+v, want a checks row %v", d.Readiness, tc.checksRow)
			}

			// The next read, of this PR and of another in the repository.
			for _, number := range []int{7, 8} {
				if _, err := core.ReadPR(t.Context(), githubTestRef(number), PRReadParts{Detail: true}, nil, nil); err != nil {
					t.Fatal(err)
				}
				got := reads()
				if first := got[0].Vars[string(tc.part)] != true; first != tc.remembered {
					t.Fatalf("#%d: first request leaves %s out = %v, want %v", number, tc.part, first, tc.remembered)
				}
			}
			if !tc.remembered {
				return
			}
			// Past the TTL, and after a reset, the full read is tried again.
			core.nowFn = func() time.Time { return time.Now().Add(githubReadShapeTTL + time.Second) }
			if _, err := core.ReadPR(t.Context(), githubTestRef(7), PRReadParts{Detail: true}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if got := reads(); got[0].Vars[string(tc.part)] != true {
				t.Fatal("the refusal outlived its TTL")
			}
			core.ResetForgeCaches()
			if _, err := core.ReadPR(t.Context(), githubTestRef(7), PRReadParts{Detail: true}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if got := reads(); got[0].Vars[string(tc.part)] != true {
				t.Fatal("the refusal outlived a cache reset")
			}
		})
	}
}

// A threads read too large to answer, by its body or by GitHub's 10
// second limit (a 502 or 504, or the timeout message), steps down
// githubReadLevels: without reactions (saying so), then in pages of 25,
// and the PR's later reads start from the level that fit. A read too
// large at the smallest level fails; a read that fits sends one request.
func TestGitHubThreadsReadStepsDownToTheSizeThatFits(t *testing.T) {
	t.Parallel()
	groups := []map[string]any{}
	for _, content := range []string{"THUMBS_UP", "THUMBS_DOWN", "LAUGH", "HOORAY", "CONFUSED", "HEART", "ROCKET", "EYES"} {
		groups = append(groups, map[string]any{"content": content, "viewerHasReacted": false, "reactors": map[string]any{"totalCount": 0}})
	}
	// comments is the page of the PR's 100 comments after after, pageSize
	// long.
	comments := func(bodyBytes, after, pageSize int, reactions bool) map[string]any {
		nodes := []map[string]any{}
		for i := after; i < min(after+pageSize, 100); i++ {
			node := map[string]any{"id": fmt.Sprintf("IC_%d", i), "fullDatabaseId": strconv.Itoa(i + 1), "author": map[string]any{"login": "octo"},
				"body": strings.Repeat("x", bodyBytes), "createdAt": "2026-01-01T00:00:00Z", "lastEditedAt": nil, "url": "u",
				"viewerCanUpdate": false, "viewerCanDelete": false, "isMinimized": false}
			if reactions {
				node["reactionGroups"] = groups
			}
			nodes = append(nodes, node)
		}
		end := after + len(nodes)
		return map[string]any{"pageInfo": map[string]any{"hasNextPage": end < 100, "endCursor": strconv.Itoa(end)}, "nodes": nodes}
	}
	tick := func(bodyBytes, pageSize int, reactions bool) string {
		return mustJSON(map[string]any{"data": map[string]any{"repository": map[string]any{"viewerPermission": "WRITE", "pullRequest": map[string]any{
			"number": 5, "headRefName": "f", "state": "OPEN", "viewerCanApplySuggestion": true,
			"reviewThreads": map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": []any{}},
			"comments":      comments(bodyBytes, 0, pageSize, reactions),
		}}}})
	}
	// Each comment's eight groups add some 700 bytes: a body size that
	// fits without them and not with them, one that fits only 25 to a
	// page, and one that fits no way.
	without := len(tick(0, 100, false))
	with := len(tick(0, 100, true))
	fits := (forgeAPIDefaultBound - without - (with-without)/2) / 100
	if len(tick(fits, 100, false)) > forgeAPIDefaultBound || len(tick(fits, 100, true)) <= forgeAPIDefaultBound {
		t.Fatalf("fixture sizes %d and %d do not straddle the bound", len(tick(fits, 100, false)), len(tick(fits, 100, true)))
	}
	paged := forgeAPIDefaultBound / 50
	if len(tick(paged, 100, false)) <= forgeAPIDefaultBound || len(tick(paged, 25, false)) > forgeAPIDefaultBound {
		t.Fatal("the paged fixture does not straddle the bound")
	}
	type request struct {
		op        string
		reactions bool
		pageSize  float64
	}
	full, bare, small := request{"PRTick", true, 100}, request{"PRTick", false, 100}, request{"PRTick", false, 25}
	page := request{"PRCommentsPage", false, 25}
	timeout := func(status int, message string) func(pageSize int) (forgeAPIAnswer, bool) {
		return func(pageSize int) (forgeAPIAnswer, bool) {
			if pageSize == 25 {
				return forgeAPIAnswer{}, false
			}
			if status != 0 {
				return forgeAPIAnswer{Status: status, Body: `{"message":"` + message + `"}`}, true
			}
			return forgeAPIAnswer{Body: `{"data":null,"errors":[{"message":"` + message + `"}]}`}, true
		}
	}
	for _, tc := range []struct {
		name      string
		bodyBytes int
		// stopped answers a read GitHub stopped at its time limit.
		stopped  func(pageSize int) (forgeAPIAnswer, bool)
		requests []request
		again    []request // the PR's next read
		err      error
		sentence string
	}{
		{name: "fits with reactions", bodyBytes: 10, requests: []request{full}, again: []request{full}},
		{name: "fits only without reactions", bodyBytes: fits, requests: []request{full, bare}, again: []request{bare}, sentence: githubReactionsTooLarge},
		{name: "fits only in smaller pages", bodyBytes: paged, requests: []request{full, bare, small, page, page, page}, again: []request{small, page, page, page}, sentence: githubReactionsTooLarge},
		{name: "stopped with a 502", bodyBytes: 10, stopped: timeout(http.StatusBadGateway, "Server Error"), requests: []request{full, bare, small, page, page, page}, again: []request{small, page, page, page}, sentence: githubReactionsTooLarge},
		{name: "stopped with a 504", bodyBytes: 10, stopped: timeout(http.StatusGatewayTimeout, "We couldn't respond to your request in time. Sorry about that."), requests: []request{full, bare, small, page, page, page}, again: []request{small, page, page, page}, sentence: githubReactionsTooLarge},
		{name: "stopped with the timeout message", bodyBytes: 10, stopped: timeout(0, "Something went wrong while executing your query. This may be the result of a timeout, or it could be a GitHub bug."), requests: []request{full, bare, small, page, page, page}, again: []request{small, page, page, page}, sentence: githubReactionsTooLarge},
		{name: "fits no way", bodyBytes: forgeAPIDefaultBound / 20, requests: []request{full, bare, small}, again: []request{full, bare, small}, err: forgeapi.ErrBodyTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
				size, _ := call.Vars["pageSize"].(float64)
				reactions := call.Vars["wantReactions"] == true
				if tc.stopped != nil {
					if answer, stop := tc.stopped(int(size)); stop {
						return answer
					}
				}
				switch call.Op {
				case "PRTick":
					return forgeAPIAnswer{Body: tick(tc.bodyBytes, int(size), reactions)}
				case "PRCommentsPage":
					after, _ := strconv.Atoi(fmt.Sprint(call.Vars["after"]))
					return forgeAPIAnswer{Body: mustJSON(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
						"comments": comments(tc.bodyBytes, after, int(size), reactions)}}}})}
				}
				return forgeUnexpected(t, call)
			})
			sent := 0
			requests := func() []request {
				all := calls.all()
				got := []request{}
				for _, call := range all[sent:] {
					size, _ := call.Vars["pageSize"].(float64)
					got = append(got, request{call.Op, call.Vars["wantReactions"] == true, size})
				}
				sent = len(all)
				return got
			}
			for i, want := range [][]request{tc.requests, tc.again} {
				read, err := core.ReadPR(t.Context(), githubTestRef(5), PRReadParts{Threads: true}, nil, nil)
				if tc.err != nil {
					if !errors.Is(err, tc.err) {
						t.Fatalf("read %d: err = %v, want %v", i, err, tc.err)
					}
				} else if err != nil {
					t.Fatalf("read %d: err = %v", i, err)
				}
				if got := requests(); !slices.Equal(got, want) {
					t.Fatalf("read %d: requests = %v, want %v", i, got, want)
				}
				if read.ReactionsUnavailable != tc.sentence {
					t.Fatalf("read %d: ReactionsUnavailable = %q", i, read.ReactionsUnavailable)
				}
				if tc.err == nil && len(read.Threads) != 100 {
					t.Fatalf("read %d: threads = %d", i, len(read.Threads))
				}
			}
			// The level is the PR's: another PR's read starts in full.
			_, _ = core.ReadPR(t.Context(), githubTestRef(6), PRReadParts{Threads: true}, nil, nil)
			if got := requests(); len(got) == 0 || got[0] != full {
				t.Fatalf("another PR's requests = %v, want the full read first", got)
			}
		})
	}
}

// Without check runs a required check that has not reported is unknown,
// not pending; a failed status context still blocks.
func TestGitHubChecksRowWithoutCheckRuns(t *testing.T) {
	t.Parallel()
	rules := githubBaseRules{requiredChecks: []string{"build", "lint"}}
	nameless := githubContextRaw{Typename: "CheckRun"}
	in := githubMergeInputs{contexts: []githubContextRaw{nameless}, checksUnknown: true}
	if row, _, present := in.checksCondition(rules); present {
		t.Fatalf("row = %+v, want none", row)
	}
	failed := githubContextRaw{Typename: "StatusContext", Context: "lint", State: "FAILURE", IsRequired: true}
	in.contexts = append(in.contexts, failed)
	if row, ok, present := in.checksCondition(rules); !present || ok || row.Text != "1 required check failed" {
		t.Fatalf("row = %+v ok %v present %v, want the failed context alone", row, ok, present)
	}
}

// A read without threads is never made smaller: a 502 on a detail read
// fails it.
func TestGitHubDetailReadIsNotStepped(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		return forgeAPIAnswer{Status: http.StatusBadGateway, Body: `{"message":"Server Error"}`}
	})
	if _, err := core.ReadPR(t.Context(), githubTestRef(5), PRReadParts{Detail: true}, nil, nil); err == nil {
		t.Fatal("the read published")
	}
	if got := len(calls.all()); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}
