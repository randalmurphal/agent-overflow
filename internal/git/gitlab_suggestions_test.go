package git

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const gitlabTestDiscussionWithSuggestion = `{"id":"d1","notes":[{"id":31,"body":"` + "```suggestion\\nTWO\\n```" + `","system":false,"created_at":"t","resolvable":true,"resolved":false,
	"author":{"username":"rev","name":"Rev"},"position":{"head_sha":"head2","new_path":"a.go","old_path":"a.go","position_type":"text","new_line":2},
	"suggestions":[{"id":901,"from_line":2,"to_line":2,"appliable":true,"applied":false,"from_content":"two\n","to_content":"TWO\n"}]}]}`

func gitlabApplyReq() SuggestionApply {
	return SuggestionApply{Target: CommentTarget{ThreadID: "d1", CommentID: "31"}, SuggestionID: "901", HeadSHA: "dead2" + strings.Repeat("0", 35),
		Digest: SuggestionDigest("gitlab", 2, 2, []string{"TWO"})}
}

// The read carries GitLab's own suggestion and the apply checks it is
// the named note's in the named discussion of this merge request before
// PUT suggestions/:id/apply.
func TestGitLabSuggestionReadAndApply(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	head := "dead2" + strings.Repeat("0", 35)
	var applyAnswer atomic.Value
	applyAnswer.Store(forgeAPIAnswer{Body: `{"id":901,"from_line":2,"to_line":2,"appliable":false,"applied":true,"from_content":"two\n","to_content":"TWO\n"}`})
	mrBody := strings.ReplaceAll(gitlabTestMR(7, 0), `"head2"`, `"`+head+`"`)
	var discussion atomic.Value
	discussion.Store(gitlabTestDiscussionWithSuggestion)
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if reply, ok := gitlabManageExtras(call); ok {
			return reply
		}
		switch {
		case call.Path == mr:
			return forgeAPIAnswer{Body: mrBody}
		case call.Path == mr+"/discussions?per_page=100":
			return forgeAPIAnswer{Body: "[" + gitlabTestDiscussionWithSuggestion + "]"}
		case call.Path == mr+"/discussions/d1":
			return forgeAPIAnswer{Body: discussion.Load().(string)}
		case call.Path == mr+"/discussions/gone":
			return forgeAPIAnswer{Status: http.StatusNotFound, Body: `{"message":"404 Not found"}`}
		case call.Path == gitlabTestProject:
			return forgeAPIAnswer{Body: gitlabTestProjectSettings}
		case call.Path == "user":
			return forgeAPIAnswer{Body: `{"username":"viewer"}`}
		case call.Method == http.MethodPut && call.Path == "suggestions/901/apply":
			return applyAnswer.Load().(forgeAPIAnswer)
		}
		return forgeUnexpected(t, call)
	})
	threads, err := core.ListReviewThreads(t.Context(), gitlabTestRef(7))
	if err != nil || len(threads) != 1 {
		t.Fatalf("threads = %+v, %v", threads, err)
	}
	got := threads[0].Comments[0].Suggestions
	if len(got) != 1 || got[0].ID != "901" || !got[0].Appliable || !got[0].FromKnown || got[0].From[0] != "two" || got[0].To[0] != "TWO" || got[0].StartLine != 2 ||
		got[0].Digest != gitlabApplyReq().Digest {
		t.Fatalf("suggestions = %+v", got)
	}
	ref := gitlabTestRef(7)
	if err := core.ApplyPRSuggestion(t.Context(), ref, gitlabApplyReq()); err != nil {
		t.Fatal(err)
	}
	if put := countOps(calls, "PUT suggestions/901/apply"); put != 1 {
		t.Fatalf("apply requests = %d", put)
	}
	for _, tc := range []struct {
		name  string
		mod   func(*SuggestionApply)
		check func(error) bool
	}{
		{"moved head", func(r *SuggestionApply) { r.HeadSHA = strings.Repeat("9", 40) }, func(err error) bool { return errors.As(err, new(*HeadMovedError)) }},
		{"another note's suggestion", func(r *SuggestionApply) { r.Target.CommentID = "32" }, notOnPR},
		{"a suggestion not in the note", func(r *SuggestionApply) { r.SuggestionID = "902" }, changed},
		{"another digest", func(r *SuggestionApply) { r.Digest = SuggestionDigest("gitlab", 2, 2, []string{"THREE"}) }, changed},
		{"a discussion not on this merge request", func(r *SuggestionApply) { r.Target.ThreadID = "gone" }, notOnPR},
	} {
		req := gitlabApplyReq()
		tc.mod(&req)
		if err := core.ApplyPRSuggestion(t.Context(), ref, req); !tc.check(err) {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
	}
	// The note edited since the read: the block changed, a block inserted
	// before it (GitLab renumbers its suggestions) or the anchor moved is
	// refused; prose edited around it still applies.
	edited := func(suggestions string) string {
		return strings.Replace(gitlabTestDiscussionWithSuggestion,
			`[{"id":901,"from_line":2,"to_line":2,"appliable":true,"applied":false,"from_content":"two\n","to_content":"TWO\n"}]`, suggestions, 1)
	}
	for _, tc := range []struct {
		name, suggestions string
	}{
		{"the block edited", `[{"id":901,"from_line":2,"to_line":2,"appliable":true,"applied":false,"from_content":"two\n","to_content":"THREE\n"}]`},
		{"a block inserted before it", `[{"id":905,"from_line":2,"to_line":2,"appliable":true,"applied":false,"from_content":"two\n","to_content":"X\n"},{"id":906,"from_line":2,"to_line":2,"appliable":true,"applied":false,"from_content":"two\n","to_content":"TWO\n"}]`},
		{"the anchor moved", `[{"id":901,"from_line":3,"to_line":3,"appliable":true,"applied":false,"from_content":"three\n","to_content":"TWO\n"}]`},
	} {
		body := edited(tc.suggestions)
		if body == gitlabTestDiscussionWithSuggestion {
			t.Fatalf("%s: the fixture did not change", tc.name)
		}
		discussion.Store(body)
		if err := core.ApplyPRSuggestion(t.Context(), ref, gitlabApplyReq()); !changed(err) {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
	}
	if put := countOps(calls, "PUT suggestions/901/apply"); put != 1 {
		t.Fatalf("a refused apply was sent: %d applies", put)
	}
	discussion.Store(strings.Replace(gitlabTestDiscussionWithSuggestion, `"body":"`, `"body":"Please:\n`, 1))
	if err := core.ApplyPRSuggestion(t.Context(), ref, gitlabApplyReq()); err != nil {
		t.Fatalf("after a prose edit: %v", err)
	}
	discussion.Store(gitlabTestDiscussionWithSuggestion)
	if put := countOps(calls, "PUT suggestions/901/apply"); put != 2 {
		t.Fatalf("applies = %d", put)
	}
	// GitLab's refusal is a refusal; an answer that does not report the
	// suggestion applied, or no answer, is unconfirmed.
	applyAnswer.Store(forgeAPIAnswer{Status: http.StatusBadRequest, Body: `{"message":"A suggestion is not applicable."}`})
	if err := core.ApplyPRSuggestion(t.Context(), ref, gitlabApplyReq()); err == nil || unconfirmed(err) {
		t.Fatalf("refused apply = %v", err)
	}
	applyAnswer.Store(forgeAPIAnswer{Body: `{"id":901,"applied":false}`})
	if err := core.ApplyPRSuggestion(t.Context(), ref, gitlabApplyReq()); !unconfirmed(err) {
		t.Fatalf("an apply answered not applied = %v", err)
	}
	applyAnswer.Store(forgeAPIAnswer{Status: http.StatusBadGateway, Body: `{}`})
	if err := core.ApplyPRSuggestion(t.Context(), ref, gitlabApplyReq()); !unconfirmed(err) {
		t.Fatalf("an unanswered apply = %v", err)
	}
}

func countOps(calls *forgeAPICalls, op string) int {
	n := 0
	for _, got := range calls.ops() {
		if got == op {
			n++
		}
	}
	return n
}

// gitlabReactionsCore is a GitLab with one merge request whose discussion
// holds notes 1 and 2, answering MRNoteReactions through reactions.
func gitlabReactionsCore(t *testing.T, reactions func() forgeAPIAnswer) (*Core, *forgeAPICalls, *atomic.Int64) {
	t.Helper()
	var discussions atomic.Value
	discussions.Store(gitlabReactionsDiscussions)
	return gitlabReactionsCoreWith(t, reactions, &discussions)
}

const gitlabReactionsDiscussions = `[{"id":"d","notes":[{"id":1,"body":"a","author":{"username":"rev"}},{"id":2,"body":"b","author":{"username":"me"}}]}]`

// gitlabReactionsCoreWith is gitlabReactionsCore answering the
// discussions discussions holds, and the viewer's award on note 1.
func gitlabReactionsCoreWith(t *testing.T, reactions func() forgeAPIAnswer, discussions *atomic.Value) (*Core, *forgeAPICalls, *atomic.Int64) {
	t.Helper()
	mr := gitlabTestProject + "/merge_requests/7"
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Method == http.MethodGet && call.Path == mr+"/notes/1/award_emoji?per_page=100":
			return forgeAPIAnswer{Body: `[]`}
		case call.Method == http.MethodPost && call.Path == mr+"/notes/1/award_emoji":
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":80,"name":"heart","user":{"username":"me"}}`}
		case call.Path == mr:
			return forgeAPIAnswer{Body: gitlabTestMR(7, 0)}
		case strings.HasPrefix(call.Path, mr+"/discussions?"):
			return forgeAPIAnswer{Body: discussions.Load().(string)}
		case call.Path == gitlabTestProject:
			return forgeAPIAnswer{Body: gitlabTestProjectSettings}
		case call.Path == "user":
			return forgeAPIAnswer{Body: `{"id":1,"username":"me"}`}
		case call.Op == "MRNoteReactions":
			return reactions()
		}
		return forgeUnexpected(t, call)
	})
	var now atomic.Int64
	now.Store(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC).Unix())
	core.nowFn = func() time.Time { return time.Unix(now.Load(), 0) }
	return core, calls, &now
}

func gitlabReactionsAnswer(notes ...string) forgeAPIAnswer {
	return forgeAPIAnswer{Body: `{"data":{"project":{"mergeRequest":{"notes":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[` + strings.Join(notes, ",") + `]}}}}}`}
}

func gitlabNoteAwards(gid string, awards ...string) string {
	nodes := make([]string, 0, len(awards))
	for _, award := range awards {
		name, user, _ := strings.Cut(award, ":")
		nodes = append(nodes, fmt.Sprintf(`{"name":%q,"user":{"username":%q}}`, name, user))
	}
	return fmt.Sprintf(`{"id":%q,"awardEmoji":{"pageInfo":{"hasNextPage":false},"nodes":[%s]}}`, gid, strings.Join(nodes, ","))
}

// The reactions map onto each note by its id: GitLab's names as they are
// (an unknown one included), counted, with who reacted and whether the
// viewer did.
func TestGitLabReactionsRead(t *testing.T) {
	t.Parallel()
	core, _, _ := gitlabReactionsCore(t, func() forgeAPIAnswer {
		return gitlabReactionsAnswer(
			gitlabNoteAwards("gid://gitlab/DiffNote/1", "thumbsup:rev", "thumbsup:me", "party_parrot:rev"),
			gitlabNoteAwards("gid://gitlab/DiscussionNote/2", "eyes:rev"),
			gitlabNoteAwards("gid://gitlab/Note/99", "heart:rev"),
		)
	})
	read, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Threads: true}, nil, nil)
	if err != nil || read.ReactionsUnavailable != "" {
		t.Fatalf("read = %+v, %v", read, err)
	}
	first, second := read.Threads[0].Comments[0].Reactions, read.Threads[0].Comments[1].Reactions
	if len(first) != 2 || first[0].Name != "thumbsup" || first[0].Count != 2 || !first[0].ViewerReacted || strings.Join(first[0].Users, ",") != "rev,me" ||
		first[1].Name != "party_parrot" || first[1].Count != 1 || first[1].ViewerReacted {
		t.Fatalf("note 1 reactions = %+v", first)
	}
	if len(second) != 1 || second[0].Name != "eyes" || second[0].ViewerReacted {
		t.Fatalf("note 2 reactions = %+v", second)
	}
}

// A failed reactions read leaves the threads whole with one sentence, is
// logged and not asked again until its retry is due; a server without
// the field is not asked for gitlabCacheTTL.
func TestGitLabReactionsReadFailureKeepsTheThreads(t *testing.T) {
	t.Parallel()
	var answer atomic.Value
	answer.Store(forgeAPIAnswer{Status: http.StatusBadGateway, Body: `{"message":"bad gateway"}`})
	core, calls, now := gitlabReactionsCore(t, func() forgeAPIAnswer { return answer.Load().(forgeAPIAnswer) })
	read := func() PRRead {
		t.Helper()
		out, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Threads: true}, nil, nil)
		if err != nil || len(out.Threads) != 1 || len(out.Threads[0].Comments) != 2 {
			t.Fatalf("read = %+v, %v: the threads failed with the reactions", out, err)
		}
		return out
	}
	if r := read(); r.ReactionsUnavailable != "Reactions are not shown: GitLab's reactions could not be read." || r.Threads[0].Comments[0].Reactions != nil {
		t.Fatalf("first failure = %q", r.ReactionsUnavailable)
	}
	if r := read(); r.ReactionsUnavailable == "" || countOps(calls, "MRNoteReactions") != 1 {
		t.Fatalf("a read inside the backoff asked again (%d reads) or forgot the failure %q", countOps(calls, "MRNoteReactions"), r.ReactionsUnavailable)
	}
	now.Add(int64(prLookupErrorBase / time.Second))
	answer.Store(gitlabReactionsAnswer(gitlabNoteAwards("gid://gitlab/DiffNote/1", "rocket:rev")))
	if r := read(); r.ReactionsUnavailable != "" || len(r.Threads[0].Comments[0].Reactions) != 1 || countOps(calls, "MRNoteReactions") != 2 {
		t.Fatalf("after the retry was due = %+v (%d reads)", r, countOps(calls, "MRNoteReactions"))
	}

	lacking, lackingCalls, _ := gitlabReactionsCore(t, func() forgeAPIAnswer {
		return forgeAPIAnswer{Body: `{"errors":[{"message":"Field 'awardEmoji' doesn't exist on type 'Note'"}]}`}
	})
	for range 2 {
		out, err := lacking.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Threads: true}, nil, nil)
		if err != nil || out.ReactionsUnavailable != "Reactions are not shown: this GitLab server does not report them." {
			t.Fatalf("server without the field = %q, %v", out.ReactionsUnavailable, err)
		}
	}
	if n := countOps(lackingCalls, "MRNoteReactions"); n != 1 {
		t.Fatalf("a server without the field was asked %d times", n)
	}
	lacking.ResetForgeCaches()
	if _, err := lacking.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Threads: true}, nil, nil); err != nil || countOps(lackingCalls, "MRNoteReactions") != 2 {
		t.Fatalf("ResetForgeCaches kept the host's answer")
	}
}

// A read is made again when the notes change or the last one aged out,
// not every tick.
func TestGitLabReactionsReadOnlyWhenTheyCanHaveChanged(t *testing.T) {
	t.Parallel()
	core, calls, now := gitlabReactionsCore(t, func() forgeAPIAnswer { return gitlabReactionsAnswer() })
	read := func() {
		t.Helper()
		if _, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Threads: true}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	read()
	read()
	if n := countOps(calls, "MRNoteReactions"); n != 1 {
		t.Fatalf("unchanged notes read reactions %d times", n)
	}
	now.Add(int64(gitlabReactionsMaxAge / time.Second))
	read()
	if n := countOps(calls, "MRNoteReactions"); n != 2 {
		t.Fatalf("an aged read was not made again (%d)", n)
	}
}

// GitLab's reaction writes find the viewer's award inside the write: an
// add posts the name, a removal deletes the award the read found, a
// reaction already as asked writes nothing, and a note not on this merge
// request is refused.
func TestGitLabReactionWrites(t *testing.T) {
	t.Parallel()
	notes := gitlabTestProject + "/merge_requests/7/notes/"
	awards := `[{"id":70,"name":"heart","user":{"username":"me"}},{"id":71,"name":"thumbsup","user":{"username":"rev"}}]`
	var postAnswer atomic.Value
	postAnswer.Store(forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":72,"name":"thumbsup","user":{"username":"me"}}`})
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Path == "user":
			return forgeAPIAnswer{Body: `{"id":1,"username":"me"}`}
		case call.Method == http.MethodGet && call.Path == notes+"5/award_emoji?per_page=100":
			return forgeAPIAnswer{Body: awards}
		case call.Method == http.MethodGet && call.Path == notes+"6/award_emoji?per_page=100":
			return forgeAPIAnswer{Status: http.StatusNotFound, Body: `{"message":"404 Note Not Found"}`}
		case call.Method == http.MethodPost && call.Path == notes+"5/award_emoji":
			if string(call.Body) != `{"name":"thumbsup"}` {
				t.Errorf("award body = %s", call.Body)
			}
			return postAnswer.Load().(forgeAPIAnswer)
		case call.Method == http.MethodDelete && call.Path == notes+"5/award_emoji/70":
			return forgeAPIAnswer{Status: http.StatusNoContent}
		}
		return forgeUnexpected(t, call)
	})
	ref := gitlabTestRef(7)
	target := CommentTarget{ThreadID: "d", CommentID: "5"}
	for _, tc := range []struct {
		name    string
		target  CommentTarget
		emoji   string
		reacted bool
		write   string
		check   func(error) bool
	}{
		{"add", target, "thumbsup", true, "POST " + notes + "5/award_emoji", nil},
		{"remove names the award found", target, "heart", false, "DELETE " + notes + "5/award_emoji/70", nil},
		{"already reacted", target, "heart", true, "", nil},
		{"not reacted", target, "thumbsup", false, "", nil},
		{"another merge request's note", CommentTarget{ThreadID: "d", CommentID: "6"}, "heart", true, "", notOnPR},
	} {
		before := len(calls.all())
		err := core.SetPRCommentReaction(t.Context(), ref, tc.target, tc.emoji, tc.reacted)
		if tc.check == nil && err != nil || tc.check != nil && !tc.check(err) {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
		var writes []string
		for _, call := range calls.all()[before:] {
			if call.Method != http.MethodGet {
				writes = append(writes, call.Method+" "+call.Path)
			}
		}
		if strings.Join(writes, ",") != tc.write {
			t.Fatalf("%s: writes = %v, want %q", tc.name, writes, tc.write)
		}
	}
	postAnswer.Store(forgeAPIAnswer{Status: http.StatusNotFound, Body: `{"message":"404 Award Emoji Name has already been taken Not Found"}`})
	if err := core.SetPRCommentReaction(t.Context(), ref, target, "thumbsup", true); err != nil {
		t.Fatalf("an award GitLab already has = %v", err)
	}
	postAnswer.Store(forgeAPIAnswer{Status: http.StatusBadGateway, Body: `{}`})
	if err := core.SetPRCommentReaction(t.Context(), ref, target, "thumbsup", true); !unconfirmed(err) {
		t.Fatalf("an unanswered award = %v", err)
	}
}

// gitlabBlockedReactions answers MRNoteReactions with answer, holding the
// first request until release closes, and signals started when it
// arrives.
func gitlabBlockedReactions(answer func() forgeAPIAnswer) (reactions func() forgeAPIAnswer, started, release chan struct{}) {
	started, release = make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	return func() forgeAPIAnswer {
		if first.CompareAndSwap(false, true) {
			close(started)
			<-release
		}
		return answer()
	}, started, release
}

// A reactions read in flight across the viewer's reaction write, or
// across ResetForgeCaches, is not kept, and a read after either does not
// join it: the next read asks the forge.
func TestGitLabReactionsReadAcrossAnInvalidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		invalidate func(t *testing.T, core *Core)
	}{
		{"the viewer's reaction write", func(t *testing.T, core *Core) {
			if err := core.SetPRCommentReaction(t.Context(), gitlabTestRef(7), CommentTarget{ThreadID: "d", CommentID: "1"}, "heart", true); err != nil {
				t.Fatal(err)
			}
		}},
		{"a reset", func(_ *testing.T, core *Core) { core.ResetForgeCaches() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reactions, started, release := gitlabBlockedReactions(func() forgeAPIAnswer {
				return gitlabReactionsAnswer(gitlabNoteAwards("gid://gitlab/DiffNote/1", "rocket:rev"))
			})
			core, calls, _ := gitlabReactionsCore(t, reactions)
			t.Cleanup(func() { releaseHeld(release) })
			read := func() PRRead {
				// Bounded, so a read that wrongly waits on the held one
				// fails here instead of hanging.
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				out, err := core.ReadPR(ctx, gitlabTestRef(7), PRReadParts{Threads: true}, nil, nil)
				if err != nil {
					t.Error(err)
				}
				return out
			}
			done := make(chan PRRead, 1)
			go func() { done <- read() }()
			<-started
			tc.invalidate(t, core)
			// A read after the invalidation starts its own request
			// instead of joining the one in flight.
			if after := read(); len(after.Threads[0].Comments[0].Reactions) != 1 || countOps(calls, "MRNoteReactions") != 2 {
				t.Fatalf("a read after the invalidation joined the stale one: %d requests", countOps(calls, "MRNoteReactions"))
			}
			releaseHeld(release)
			<-done
			// The stale read finished last; it must not replace the read
			// made after the invalidation, and nothing it learned is kept
			// past the next change of generation.
			before := countOps(calls, "MRNoteReactions")
			read()
			if n := countOps(calls, "MRNoteReactions"); n != before {
				t.Fatalf("the current read was not kept: %d requests after %d", n, before)
			}
		})
	}
}

// The same, for a read whose answer is stored: started before the
// invalidation and finished after it, it is not kept.
func TestGitLabReactionsStaleReadIsNotStored(t *testing.T) {
	t.Parallel()
	for _, reset := range []bool{false, true} {
		reactions, started, release := gitlabBlockedReactions(func() forgeAPIAnswer { return gitlabReactionsAnswer() })
		core, calls, _ := gitlabReactionsCore(t, reactions)
		t.Cleanup(func() { releaseHeld(release) })
		done := make(chan error, 1)
		go func() {
			_, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Threads: true}, nil, nil)
			done <- err
		}()
		<-started
		if reset {
			core.ResetForgeCaches()
		} else if err := core.SetPRCommentReaction(t.Context(), gitlabTestRef(7), CommentTarget{ThreadID: "d", CommentID: "1"}, "heart", true); err != nil {
			t.Fatal(err)
		}
		releaseHeld(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if _, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Threads: true}, nil, nil); err != nil {
			t.Fatal(err)
		}
		if n := countOps(calls, "MRNoteReactions"); n != 2 {
			t.Fatalf("reset %t: the read in flight was kept (%d requests)", reset, n)
		}
	}
}

// A failure and a server without the field learned by a read that began
// before a reset are not kept past it.
func TestGitLabReactionsFailureAcrossAReset(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]forgeAPIAnswer{
		"failure":       {Status: http.StatusBadGateway, Body: `{"message":"bad gateway"}`},
		"lacking field": {Body: `{"errors":[{"message":"Field 'awardEmoji' doesn't exist on type 'Note'"}]}`},
	} {
		var calls atomic.Int32
		reactions, started, release := gitlabBlockedReactions(func() forgeAPIAnswer {
			if calls.Add(1) == 1 {
				return answer
			}
			return gitlabReactionsAnswer()
		})
		core, ops, _ := gitlabReactionsCore(t, reactions)
		t.Cleanup(func() { releaseHeld(release) })
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Threads: true}, nil, nil)
		}()
		<-started
		core.ResetForgeCaches()
		releaseHeld(release)
		<-done
		read, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Threads: true}, nil, nil)
		if err != nil || read.ReactionsUnavailable != "" || countOps(ops, "MRNoteReactions") != 2 {
			t.Fatalf("%s: kept past the reset: %q, %d requests, %v", name, read.ReactionsUnavailable, countOps(ops, "MRNoteReactions"), err)
		}
	}
}

// Two cold reads at once share one request.
func TestGitLabReactionsConcurrentColdReadsShareOneRequest(t *testing.T) {
	t.Parallel()
	reactions, started, release := gitlabBlockedReactions(func() forgeAPIAnswer {
		return gitlabReactionsAnswer(gitlabNoteAwards("gid://gitlab/DiffNote/1", "rocket:rev"))
	})
	core, calls, _ := gitlabReactionsCore(t, reactions)
	t.Cleanup(func() { releaseHeld(release) })
	results := make(chan PRRead, 2)
	for range 2 {
		go func() {
			out, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Threads: true}, nil, nil)
			if err != nil {
				t.Error(err)
			}
			results <- out
		}()
	}
	<-started
	// The second read reaches the flight while the first is held: wait
	// until both reads made their discussions request.
	deadline := time.Now().Add(5 * time.Second)
	for countOps(calls, "GET "+gitlabTestProject+"/merge_requests/7/discussions?per_page=100") < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	releaseHeld(release)
	for range 2 {
		if out := <-results; len(out.Threads[0].Comments[0].Reactions) != 1 {
			t.Fatalf("reactions = %+v", out.Threads[0].Comments[0].Reactions)
		}
	}
	if n := countOps(calls, "MRNoteReactions"); n != 1 {
		t.Fatalf("two cold reads made %d requests", n)
	}
}

// A note added or removed is a new read even while the last one is
// fresh.
func TestGitLabReactionsReadAgainWhenTheNotesChange(t *testing.T) {
	t.Parallel()
	var discussions atomic.Value
	discussions.Store(gitlabReactionsDiscussions)
	core, calls, _ := gitlabReactionsCoreWith(t, func() forgeAPIAnswer { return gitlabReactionsAnswer() }, &discussions)
	read := func() {
		t.Helper()
		if _, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Threads: true}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	read()
	read()
	discussions.Store(strings.Replace(gitlabReactionsDiscussions, `]}]`, `,{"id":3,"body":"c","author":{"username":"rev"}}]}]`, 1))
	read()
	if n := countOps(calls, "MRNoteReactions"); n != 2 {
		t.Fatalf("a new note made %d reads, want 2", n)
	}
	discussions.Store(gitlabReactionsDiscussions)
	read()
	if n := countOps(calls, "MRNoteReactions"); n != 3 {
		t.Fatalf("a removed note made %d reads, want 3", n)
	}
}

// Past the read's bounds the reactions are partial and say so: a note
// with more than one page of awards carries none, and notes past the
// last page carry none.
func TestGitLabReactionsTruncation(t *testing.T) {
	t.Parallel()
	hundred := make([]string, 100)
	for i := range hundred {
		hundred[i] = fmt.Sprintf("e%d:rev", i)
	}
	many := strings.Replace(gitlabNoteAwards("gid://gitlab/DiffNote/1", hundred...), `"hasNextPage":false`, `"hasNextPage":true`, 1)
	core, _, _ := gitlabReactionsCore(t, func() forgeAPIAnswer {
		return gitlabReactionsAnswer(many, gitlabNoteAwards("gid://gitlab/DiscussionNote/2", "eyes:rev"))
	})
	read, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Threads: true}, nil, nil)
	if err != nil || read.ReactionsUnavailable != "Reactions are not shown on comments with more than 100." ||
		read.Threads[0].Comments[0].Reactions != nil || len(read.Threads[0].Comments[1].Reactions) != 1 {
		t.Fatalf("a note with more than 100 awards = %q, %+v, %v", read.ReactionsUnavailable, read.Threads[0].Comments, err)
	}

	// 1000 notes in 10 pages and more after: the read stops there.
	var page atomic.Int32
	paged, calls, _ := gitlabReactionsCore(t, func() forgeAPIAnswer {
		n := page.Add(1)
		nodes := make([]string, 100)
		for i := range nodes {
			id := int(n-1)*100 + i + 1
			awards := []string{}
			if id == 2 {
				awards = []string{"eyes:rev"}
			}
			nodes[i] = gitlabNoteAwards(fmt.Sprintf("gid://gitlab/Note/%d", id), awards...)
		}
		return forgeAPIAnswer{Body: fmt.Sprintf(`{"data":{"project":{"mergeRequest":{"notes":{"pageInfo":{"hasNextPage":true,"endCursor":"c%d"},"nodes":[%s]}}}}}`, n, strings.Join(nodes, ","))}
	})
	read, err = paged.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Threads: true}, nil, nil)
	if err != nil || read.ReactionsUnavailable != "Reactions are shown on the first 1000 comments only." || len(read.Threads[0].Comments[1].Reactions) != 1 {
		t.Fatalf("1000 notes and more = %q, %v", read.ReactionsUnavailable, err)
	}
	if n := countOps(calls, "MRNoteReactions"); n != gitlabReactionNotesMaxPages {
		t.Fatalf("pages read = %d", n)
	}
}

// releaseHeld releases the held reactions request once; a test that
// fails while it is held still lets its server close.
func releaseHeld(release chan struct{}) {
	select {
	case <-release:
	default:
		close(release)
	}
}
