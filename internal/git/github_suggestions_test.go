package git

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/forgeapi"
)

var githubTestHead = strings.Repeat("a", 40)

// githubSuggestionTarget is a SuggestionTarget answer: thread T1 of o/r#5
// on a.go lines 2 to 3, holding comment C1 by octo, at githubTestHead.
func githubSuggestionTarget(mod func(thread, pull map[string]any)) string {
	pull := map[string]any{"number": 5, "state": "OPEN", "headRefName": "feature", "headRefOid": githubTestHead,
		"repository": map[string]any{"nameWithOwner": "o/r"}, "headRepository": map[string]any{"nameWithOwner": "o/r"}}
	thread := map[string]any{"__typename": "PullRequestReviewThread", "id": "T1", "isResolved": false, "isOutdated": false, "path": "a.go",
		"line": 3, "startLine": 2, "diffSide": "RIGHT", "startDiffSide": "RIGHT", "subjectType": "LINE", "pullRequest": pull,
		"comments": map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": []map[string]any{{"id": "C0"}, {"id": "C1"}}}}
	if mod != nil {
		mod(thread, pull)
	}
	return mustJSON(map[string]any{"data": map[string]any{
		"viewer":  map[string]any{"login": "me"},
		"thread":  thread,
		"comment": map[string]any{"__typename": "PullRequestReviewComment", "id": "C1", "body": "```suggestion\nTWO\n```", "author": map[string]any{"login": "octo", "databaseId": 42, "name": "Octo Cat"}},
	}})
}

// githubSuggestionTargetBody is the default target with the comment's
// body replaced.
func githubSuggestionTargetBody(body string) string {
	target := githubSuggestionTarget(nil)
	out := strings.Replace(target, `"body":`+mustJSON("```suggestion\nTWO\n```"), `"body":`+mustJSON(body), 1)
	if out == target {
		panic("the default body is not in the target")
	}
	return out
}

// Prose around the block edited after the read leaves the suggestion
// as shown, so it still applies.
func TestGitHubApplySuggestionAfterAProseEdit(t *testing.T) {
	t.Parallel()
	core, calls := newGitHubApplyCore(t, githubApplyRig{target: githubSuggestionTargetBody("Please:\n```suggestion\nTWO\n```\nthanks")})
	if err := core.ApplyPRSuggestion(t.Context(), githubTestRef(5), applyReq()); err != nil {
		t.Fatal(err)
	}
	if lastOp(calls, "SuggestionCommit") == nil {
		t.Fatal("no commit")
	}
}

type githubApplyRig struct {
	target  string
	file    forgeAPIAnswer
	commit  forgeAPIAnswer
	resolve forgeAPIAnswer
}

func newGitHubApplyCore(t *testing.T, rig githubApplyRig) (*Core, *forgeAPICalls) {
	t.Helper()
	if rig.target == "" {
		rig.target = githubSuggestionTarget(nil)
	}
	if rig.file.Body == "" && rig.file.Status == 0 {
		rig.file = forgeAPIAnswer{Body: "one\ntwo\nthree\nfour\n"}
	}
	if rig.commit.Body == "" && rig.commit.Status == 0 {
		rig.commit = githubData(`{"createCommitOnBranch":{"commit":{"oid":"bbbb"}}}`)
	}
	if rig.resolve.Body == "" && rig.resolve.Status == 0 {
		rig.resolve = githubData(`{"resolve":{"thread":{"isResolved":true}}}`)
	}
	return newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Op == "SuggestionTarget":
			return forgeAPIAnswer{Body: rig.target}
		case call.Op == "SuggestionCommit":
			return rig.commit
		case call.Op == "SetThreadResolved":
			return rig.resolve
		case call.Method == http.MethodGet && call.Path == "repos/o/r/contents/a.go?ref="+githubTestHead:
			if call.Header.Get("Accept") != "application/vnd.github.raw" {
				t.Errorf("contents read with Accept %q", call.Header.Get("Accept"))
			}
			return rig.file
		}
		return forgeUnexpected(t, call)
	})
}

func applyReq() SuggestionApply {
	return SuggestionApply{Target: CommentTarget{ThreadID: "T1", CommentID: "C1"}, Index: 0, HeadSHA: githubTestHead, Digest: SuggestionDigest("github", 2, 3, []string{"TWO"})}
}

func lastOp(calls *forgeAPICalls, op string) *forgeAPICall {
	all := calls.all()
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Op == op {
			return &all[i]
		}
	}
	return nil
}

// The apply reads the file at the head the pane saw, commits the anchored
// lines rewritten with the message and trailer GitHub writes, against
// that head, then resolves the thread.
func TestGitHubApplySuggestionCommitsAndResolves(t *testing.T) {
	t.Parallel()
	core, calls := newGitHubApplyCore(t, githubApplyRig{})
	if err := core.ApplyPRSuggestion(t.Context(), githubTestRef(5), applyReq()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls.ops(), ","); got != "SuggestionTarget,GET repos/o/r/contents/a.go?ref="+githubTestHead+",SuggestionCommit,SetThreadResolved" {
		t.Fatalf("requests = %s", got)
	}
	commit := lastOp(calls, "SuggestionCommit").Vars
	contents, _ := base64.StdEncoding.DecodeString(commit["contents"].(string))
	if string(contents) != "one\nTWO\nfour\n" || commit["path"] != "a.go" || commit["branch"] != "feature" || commit["repository"] != "o/r" ||
		commit["expectedHeadOid"] != githubTestHead || commit["headline"] != "Apply suggestion from code review" ||
		commit["body"] != "Co-authored-by: Octo Cat <42+octo@users.noreply.github.com>" {
		t.Fatalf("commit = %v (contents %q)", commit, contents)
	}
	if resolve := lastOp(calls, "SetThreadResolved").Vars; resolve["threadID"] != "T1" || resolve["resolved"] != true {
		t.Fatalf("resolve = %v", resolve)
	}
}

// The viewer's own suggestion, and an author GitHub gives no user id,
// get no trailer.
func TestGitHubApplySuggestionTrailerNeedsAnotherUser(t *testing.T) {
	t.Parallel()
	for _, author := range []string{`{"login":"me","databaseId":7,"name":"Me"}`, `{"login":"bot[bot]"}`, `null`} {
		target := strings.Replace(githubSuggestionTarget(nil), `{"databaseId":42,"login":"octo","name":"Octo Cat"}`, author, 1)
		core, calls := newGitHubApplyCore(t, githubApplyRig{target: target})
		if err := core.ApplyPRSuggestion(t.Context(), githubTestRef(5), applyReq()); err != nil {
			t.Fatal(err)
		}
		if _, ok := lastOp(calls, "SuggestionCommit").Vars["body"]; ok {
			t.Fatalf("author %s: a trailer was sent", author)
		}
	}
}

// Everything checked before the commit refuses with nothing written.
func TestGitHubApplySuggestionRefusals(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x\n", suggestionFileMaxBytes/2+1)
	for _, tc := range []struct {
		name   string
		rig    githubApplyRig
		req    func(*SuggestionApply)
		ref    PRReference
		expect func(error) bool
	}{
		{"moved head", githubApplyRig{target: githubSuggestionTarget(func(_, pull map[string]any) { pull["headRefOid"] = strings.Repeat("b", 40) })}, nil, githubTestRef(5),
			func(err error) bool { return errors.As(err, new(*HeadMovedError)) }},
		{"another PR's thread", githubApplyRig{}, nil, githubTestRef(6), notOnPR},
		{"another repository's thread", githubApplyRig{target: githubSuggestionTarget(func(_, pull map[string]any) {
			pull["repository"] = map[string]any{"nameWithOwner": "o/other"}
		})}, nil, githubTestRef(5), notOnPR},
		{"a comment of another thread", githubApplyRig{target: githubSuggestionTarget(func(thread, _ map[string]any) {
			thread["comments"] = map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": []map[string]any{{"id": "C0"}}}
		})}, nil, githubTestRef(5), notOnPR},
		{"unknown node", githubApplyRig{target: `{"data":{"viewer":{"login":"me"},"thread":null,"comment":null},"errors":[{"type":"NOT_FOUND","message":"Could not resolve"}]}`}, nil, githubTestRef(5), notOnPR},
		{"closed", githubApplyRig{target: githubSuggestionTarget(func(_, pull map[string]any) { pull["state"] = "CLOSED" })}, nil, githubTestRef(5), notApplied},
		{"outdated", githubApplyRig{target: githubSuggestionTarget(func(thread, _ map[string]any) { thread["isOutdated"] = true })}, nil, githubTestRef(5), notApplied},
		{"resolved", githubApplyRig{target: githubSuggestionTarget(func(thread, _ map[string]any) { thread["isResolved"] = true })}, nil, githubTestRef(5), notApplied},
		{"old side", githubApplyRig{target: githubSuggestionTarget(func(thread, _ map[string]any) { thread["diffSide"] = "LEFT" })}, nil, githubTestRef(5), notApplied},
		{"no such suggestion", githubApplyRig{}, func(r *SuggestionApply) { r.Index = 1 }, githubTestRef(5), changed},
		{"the block edited", githubApplyRig{target: githubSuggestionTargetBody("```suggestion\nTHREE\n```")}, nil, githubTestRef(5), changed},
		{"a block inserted before it", githubApplyRig{target: githubSuggestionTargetBody("```suggestion\nX\n```\n```suggestion\nTWO\n```")}, nil, githubTestRef(5), changed},
		{"the anchor moved", githubApplyRig{target: githubSuggestionTarget(func(thread, _ map[string]any) { thread["line"], thread["startLine"] = 4, 3 })}, nil, githubTestRef(5), changed},
		{"a digest of another forge", githubApplyRig{}, func(r *SuggestionApply) { r.Digest = SuggestionDigest("gitlab", 2, 3, []string{"TWO"}) }, githubTestRef(5), changed},
		{"file past the bound", githubApplyRig{file: forgeAPIAnswer{Body: big}}, nil, githubTestRef(5), notApplied},
		{"binary file", githubApplyRig{file: forgeAPIAnswer{Body: "one\x00\ntwo\nthree\n"}}, nil, githubTestRef(5), notApplied},
		{"file shorter than the anchor", githubApplyRig{file: forgeAPIAnswer{Body: "one\ntwo\n"}}, nil, githubTestRef(5), notApplied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, calls := newGitHubApplyCore(t, tc.rig)
			req := applyReq()
			if tc.req != nil {
				tc.req(&req)
			}
			err := core.ApplyPRSuggestion(t.Context(), tc.ref, req)
			if !tc.expect(err) {
				t.Fatalf("err = %v", err)
			}
			if lastOp(calls, "SuggestionCommit") != nil || lastOp(calls, "SetThreadResolved") != nil {
				t.Fatalf("a refused apply wrote: %v", calls.ops())
			}
		})
	}
}

func changed(err error) bool     { return errors.As(err, new(*SuggestionChangedError)) }
func notOnPR(err error) bool     { return errors.As(err, new(*CommentNotOnPRError)) }
func notApplied(err error) bool  { return errors.As(err, new(*SuggestionNotAppliedError)) }
func unconfirmed(err error) bool { return errors.As(err, new(*UnconfirmedWriteError)) }

// The commit's outcomes: refused (the head moved under it) is a refusal;
// lost, or data beside errors, is unconfirmed and the thread is not
// resolved; a resolve refused or lost after the commit is a partial write.
func TestGitHubApplySuggestionOutcomes(t *testing.T) {
	t.Parallel()
	movedErr := `{"data":{"createCommitOnBranch":null},"errors":[{"type":"UNPROCESSABLE","path":["createCommitOnBranch"],"message":"Expected branch to point to \"x\" but it did not. Pull and try again."}]}`
	for _, tc := range []struct {
		name     string
		rig      githubApplyRig
		check    func(error) bool
		resolved bool
	}{
		{"commit refused for a moved head", githubApplyRig{commit: forgeAPIAnswer{Body: movedErr}}, func(err error) bool {
			var explained *ForgeActionError
			return !unconfirmed(err) && forgeapi.WriteNotApplied(err) && errors.As(ExplainForgeError(githubTestRef(5), ActionApplySuggestion, err), &explained) && explained.Kind == ForgeErrorStaleHead
		}, false},
		{"commit unanswered", githubApplyRig{commit: forgeAPIAnswer{Status: http.StatusBadGateway, Body: `{"message":"bad gateway"}`}}, unconfirmed, false},
		{"commit data beside errors", githubApplyRig{commit: forgeAPIAnswer{Body: `{"data":{"createCommitOnBranch":{"commit":{"oid":"b"}}},"errors":[{"message":"something"}]}`}}, unconfirmed, false},
		{"commit answered without a commit", githubApplyRig{commit: githubData(`{"createCommitOnBranch":{"commit":null}}`)}, unconfirmed, false},
		{"resolve refused", githubApplyRig{resolve: forgeAPIAnswer{Body: `{"data":{"resolve":null},"errors":[{"type":"FORBIDDEN","path":["resolve"],"message":"no"}]}`}}, func(err error) bool {
			partial, ok := errors.AsType[*PartialWriteError](err)
			return ok && !partial.Unconfirmed() && partial.Done == "committed the suggestion to the head branch"
		}, true},
		{"resolve unanswered", githubApplyRig{resolve: forgeAPIAnswer{Status: http.StatusInternalServerError, Body: `{}`}}, func(err error) bool {
			partial, ok := errors.AsType[*PartialWriteError](err)
			return ok && partial.Unconfirmed()
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, calls := newGitHubApplyCore(t, tc.rig)
			err := core.ApplyPRSuggestion(t.Context(), githubTestRef(5), applyReq())
			if !tc.check(err) {
				t.Fatalf("err = %v", err)
			}
			commits := 0
			for _, op := range calls.ops() {
				if op == "SuggestionCommit" {
					commits++
				}
			}
			if commits != 1 || (lastOp(calls, "SetThreadResolved") != nil) != tc.resolved {
				t.Fatalf("requests = %v", calls.ops())
			}
		})
	}
}

// githubReactionTarget answers ReactionTarget for comment IC_1 (an issue
// comment) of o/r#5 with the viewer's heart.
func githubReactionTarget(typename string, number int) forgeAPIAnswer {
	return githubData(fmt.Sprintf(`{"node":{"__typename":%q,"pullRequest":{"number":%d,"repository":{"nameWithOwner":"o/r"}},
		"reactionGroups":[{"content":"THUMBS_UP","viewerHasReacted":false,"reactors":{"totalCount":1}},{"content":"HEART","viewerHasReacted":true,"reactors":{"totalCount":1}}]}}`, typename, number))
}

func TestGitHubReactionWrites(t *testing.T) {
	t.Parallel()
	issue := CommentTarget{ThreadID: "IC_1", CommentID: "IC_1"}
	review := CommentTarget{ThreadID: "T1", CommentID: "RC_1"}
	for _, tc := range []struct {
		name    string
		target  CommentTarget
		answer  forgeAPIAnswer
		reactTo string
		reacted bool
		ref     PRReference
		ops     string
		check   func(error) bool
	}{
		{"add", issue, githubReactionTarget("IssueComment", 5), "thumbsup", true, githubTestRef(5), "ReactionTarget,AddReaction", nil},
		{"remove", issue, githubReactionTarget("IssueComment", 5), "heart", false, githubTestRef(5), "ReactionTarget,RemoveReaction", nil},
		{"already reacted", issue, githubReactionTarget("IssueComment", 5), "heart", true, githubTestRef(5), "ReactionTarget", nil},
		{"not reacted", issue, githubReactionTarget("IssueComment", 5), "thumbsup", false, githubTestRef(5), "ReactionTarget", nil},
		{"review comment", review, githubReactionTarget("PullRequestReviewComment", 5), "rocket", true, githubTestRef(5), "ReactionTarget,AddReaction", nil},
		{"another PR's comment", issue, githubReactionTarget("IssueComment", 6), "rocket", true, githubTestRef(5), "ReactionTarget", notOnPR},
		{"a review comment named as an issue comment", issue, githubReactionTarget("PullRequestReviewComment", 5), "rocket", true, githubTestRef(5), "ReactionTarget", notOnPR},
		{"unknown id", issue, forgeAPIAnswer{Body: `{"data":{"node":null},"errors":[{"type":"NOT_FOUND","message":"x"}]}`}, "rocket", true, githubTestRef(5), "ReactionTarget", notOnPR},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
				switch call.Op {
				case "ReactionTarget":
					return tc.answer
				case "AddReaction":
					return githubData(`{"addReaction":{"reaction":{"content":"THUMBS_UP"}}}`)
				case "RemoveReaction":
					return githubData(`{"removeReaction":{"reaction":{"content":"HEART"}}}`)
				}
				return forgeUnexpected(t, call)
			})
			err := core.SetPRCommentReaction(t.Context(), tc.ref, tc.target, tc.reactTo, tc.reacted)
			if tc.check == nil && err != nil || tc.check != nil && !tc.check(err) {
				t.Fatalf("err = %v", err)
			}
			if got := strings.Join(calls.ops(), ","); got != tc.ops {
				t.Fatalf("requests = %s, want %s", got, tc.ops)
			}
			if write := lastOp(calls, "AddReaction"); write != nil && (write.Vars["subjectID"] != tc.target.CommentID || write.Vars["content"] != githubReactionContent[tc.reactTo]) {
				t.Fatalf("add = %v", write.Vars)
			}
		})
	}
	// A write that may have applied is unconfirmed, never refused.
	core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op == "ReactionTarget" {
			return githubReactionTarget("IssueComment", 5)
		}
		return forgeAPIAnswer{Body: `{"data":{"addReaction":{"reaction":{"content":"ROCKET"}}},"errors":[{"message":"partial"}]}`}
	})
	if err := core.SetPRCommentReaction(t.Context(), githubTestRef(5), issue, "rocket", true); !unconfirmed(err) {
		t.Fatalf("data beside errors = %v", err)
	}
}

func mustJSON(v any) string {
	out, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(out)
}
