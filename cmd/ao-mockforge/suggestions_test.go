package main

import (
	"encoding/base64"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// Suggestions, reactions and comment links through the app's own adapters
// against the engine: each write changes what the next read answers, and
// what the forge refuses changes nothing.

const suggestionFile = "one\ntwo\nthree\n"

// suggestionFixture adds to a merge fixture's first pull a file, a thread
// on its line 2 whose comment suggests "TWO", with another author's heart,
// and a conversation comment; its second pull gets a comment of its own,
// which a write naming it against the first pull must refuse.
func suggestionFixture(fixture forgefake.Fixture) forgefake.Fixture {
	p := &fixture.Repos[0].Pulls[0]
	p.Files = map[string]string{"a.go": suggestionFile}
	p.Threads = []forgefake.Thread{{Path: "a.go", Line: intPtr(2), Side: "right", Comments: []forgefake.Comment{{
		Author: "octo", AuthorName: "Octo Cat", Body: "Use:\n```suggestion\nTWO\n```\n",
		Reactions: []forgefake.Reaction{{Name: "heart", User: "octo"}},
	}}}}
	p.Comments = []forgefake.Comment{{Author: "octo", Body: "their note"}}
	fixture.Repos[0].Pulls[1].Comments = []forgefake.Comment{{Author: "octo", Body: "elsewhere"}}
	return fixture
}

func reaction(c gitops.ReviewComment, name string) (gitops.CommentReaction, bool) {
	for _, r := range c.Reactions {
		if r.Name == name {
			return r, true
		}
	}
	return gitops.CommentReaction{}, false
}

func TestSuggestionsAndReactionsThroughTheAppsForgeCode(t *testing.T) {
	for _, tc := range []struct {
		forge   string
		fixture forgefake.Fixture
		ref     gitops.PRReference
		other   gitops.PRReference
		head    string
		addRoute,
		removeRoute string
	}{
		{"github", githubMergeFixture("success"), gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "merge", Number: 9},
			gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "merge", Number: 10}, strings.Repeat("9", 40),
			"gh graphql AddReaction", "gh graphql RemoveReaction"},
		{"gitlab", gitlabMergeFixture("success"), gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "merge", Number: 4},
			gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "merge", Number: 5}, strings.Repeat("4", 40),
			"glab api create note award emoji", "glab api delete note award emoji"},
	} {
		t.Run(tc.forge, func(t *testing.T) {
			r := newRig(t, suggestionFixture(tc.fixture))
			ctx := forgeapi.WithInteractive(t.Context())
			ref := tc.ref

			// Read: the suggestion, the heart, every comment's link.
			read := mergeRead(t, r, ref)
			thread, note := threadByBody(read, "Use:\n```suggestion\nTWO\n```\n"), threadByBody(read, "their note")
			if thread == nil || note == nil {
				t.Fatalf("seeded threads = %+v", read.Threads)
			}
			if read.ReactionsUnavailable != "" {
				t.Fatalf("reactions unavailable: %s", read.ReactionsUnavailable)
			}
			comment := thread.Comments[0]
			if len(comment.Suggestions) != 1 {
				t.Fatalf("suggestions = %+v", comment.Suggestions)
			}
			s := comment.Suggestions[0]
			if !slices.Equal(s.To, []string{"TWO"}) || !s.Appliable || s.Applied {
				t.Fatalf("suggestion = %+v", s)
			}
			switch tc.forge {
			case "github":
				if s.StartLine != 2 || s.EndLine != 2 || s.FromKnown || s.ID != "" {
					t.Fatalf("GitHub suggestion = %+v", s)
				}
			case "gitlab":
				if !s.FromKnown || !slices.Equal(s.From, []string{"two"}) || s.ID == "" {
					t.Fatalf("GitLab suggestion = %+v", s)
				}
				for _, c := range []gitops.ReviewComment{comment, note.Comments[0]} {
					if want := read.Detail.URL + "#note_" + strconv.FormatInt(c.DatabaseID, 10); c.URL != want {
						t.Fatalf("GitLab comment URL = %q, want %q", c.URL, want)
					}
				}
			}
			if comment.URL == "" || note.Comments[0].URL == "" {
				t.Fatalf("comment URLs = %q, %q", comment.URL, note.Comments[0].URL)
			}
			if heart, ok := reaction(comment, gitops.ReactionHeart); !ok || heart.Count != 1 || heart.ViewerReacted {
				t.Fatalf("seeded reactions = %+v", comment.Reactions)
			}
			if len(note.Comments[0].Reactions) != 0 {
				t.Fatalf("a comment with no reactions = %+v", note.Comments[0].Reactions)
			}

			// React: add on a thread comment and on the PR comment, an add
			// already made and a remove not made each answer success with
			// no write, and a comment of another PR is refused.
			onThread, onPR := targetOf(thread, 0), targetOf(note, 0)
			if err := r.core.SetPRCommentReaction(ctx, ref, onThread, gitops.ReactionHeart, true); err != nil {
				t.Fatalf("add heart: %v", err)
			}
			if err := r.core.SetPRCommentReaction(ctx, ref, onPR, gitops.ReactionRocket, true); err != nil {
				t.Fatalf("add rocket to the PR comment: %v", err)
			}
			mark := lastSeq(r)
			if err := r.core.SetPRCommentReaction(ctx, ref, onThread, gitops.ReactionHeart, true); err != nil {
				t.Fatalf("add heart again: %v", err)
			}
			if err := r.core.SetPRCommentReaction(ctx, ref, onThread, gitops.ReactionEyes, false); err != nil {
				t.Fatalf("remove eyes never added: %v", err)
			}
			if n := routeCount(r, mark, tc.addRoute) + routeCount(r, mark, tc.removeRoute); n != 0 {
				t.Fatalf("%d writes for reactions already in their state", n)
			}
			elsewhere := threadByBody(mergeRead(t, r, tc.other), "elsewhere")
			if elsewhere == nil {
				t.Fatal("the other PR's comment did not read")
			}
			if err := r.core.SetPRCommentReaction(ctx, ref, targetOf(elsewhere, 0), gitops.ReactionHeart, true); !errors.As(err, new(*gitops.CommentNotOnPRError)) {
				t.Fatalf("a reaction on another PR's comment = %v", err)
			}
			read = mergeRead(t, r, ref)
			if heart, _ := reaction(threadByBody(read, comment.Body).Comments[0], gitops.ReactionHeart); heart.Count != 2 || !heart.ViewerReacted {
				t.Fatalf("after the add = %+v", threadByBody(read, comment.Body).Comments[0].Reactions)
			}
			if rocket, _ := reaction(threadByBody(read, "their note").Comments[0], gitops.ReactionRocket); rocket.Count != 1 || !rocket.ViewerReacted {
				t.Fatalf("PR comment after the add = %+v", threadByBody(read, "their note").Comments[0].Reactions)
			}
			if r := threadByBody(mergeRead(t, r, tc.other), "elsewhere").Comments[0].Reactions; len(r) != 0 {
				t.Fatalf("the refused reaction landed: %+v", r)
			}
			if err := r.core.SetPRCommentReaction(ctx, ref, onThread, gitops.ReactionHeart, false); err != nil {
				t.Fatalf("remove heart: %v", err)
			}
			read = mergeRead(t, r, ref)
			if heart, _ := reaction(threadByBody(read, comment.Body).Comments[0], gitops.ReactionHeart); heart.Count != 1 || heart.ViewerReacted {
				t.Fatalf("after the remove = %+v", threadByBody(read, comment.Body).Comments[0].Reactions)
			}

			// Apply: a head the pane did not load is refused and changes
			// nothing; then the suggestion lands, the head moves and the
			// suggestion no longer applies.
			apply := gitops.SuggestionApply{Target: onThread, Index: 0, SuggestionID: s.ID, HeadSHA: tc.head[:39] + "0"}
			if err := r.core.ApplyPRSuggestion(ctx, ref, apply); !errors.As(err, new(*gitops.HeadMovedError)) {
				t.Fatalf("apply on a moved head = %v", err)
			}
			if read = mergeRead(t, r, ref); read.Detail.HeadSHA != tc.head {
				t.Fatalf("a refused apply moved the head to %s", read.Detail.HeadSHA)
			}
			apply.HeadSHA = tc.head
			if err := r.core.ApplyPRSuggestion(ctx, ref, apply); err != nil {
				t.Fatalf("ApplyPRSuggestion: %v", err)
			}
			read = mergeRead(t, r, ref)
			if read.Detail.HeadSHA == tc.head {
				t.Fatal("the apply left the head")
			}
			after := threadByBody(read, comment.Body)
			if got := after.Comments[0].Suggestions; len(got) != 1 || got[0].Appliable {
				t.Fatalf("suggestion after the apply = %+v", got)
			}
			switch tc.forge {
			case "github":
				// The commit is the whole file with the line replaced,
				// GitHub's message and the author's trailer; the thread is
				// resolved after it.
				vars := decodeRaw(t, lastInvocation(t, r, "gh graphql SuggestionCommit").Variables)
				contents, _ := base64.StdEncoding.DecodeString(vars["contents"].(string))
				if string(contents) != "one\nTWO\nthree\n" || vars["headline"] != gitops.SuggestionCommitHeadline || vars["expectedHeadOid"] != tc.head ||
					!strings.HasPrefix(vars["body"].(string), "Co-authored-by: Octo Cat <") {
					t.Fatalf("commit = %v (contents %q)", vars, contents)
				}
				if !after.IsResolved {
					t.Fatal("the thread was not resolved after the apply")
				}
			case "gitlab":
				if !after.Comments[0].Suggestions[0].Applied {
					t.Fatalf("GitLab suggestion after the apply = %+v", after.Comments[0].Suggestions[0])
				}
			}
			// A second apply on the new head is refused.
			apply.HeadSHA = read.Detail.HeadSHA
			if err := r.core.ApplyPRSuggestion(ctx, ref, apply); err == nil {
				t.Fatal("a second apply succeeded")
			}
			expectAllHandled(t, r)
		})
	}
}

// A GitHub head in a fork is attempted and GitHub's refusal reaches the
// user as a refusal; nothing changes.
func TestGitHubForkSuggestionRefusedThroughTheAppsForgeCode(t *testing.T) {
	fixture := githubMergeFixture("success")
	head := strings.Repeat("f", 40)
	fixture.Repos[0].Pulls = append(fixture.Repos[0].Pulls, forgefake.Pull{
		Number: 13, Title: "Forked", Author: "octo", HeadRef: "forked", HeadRepo: "octo/merge", HeadSHA: head,
		Files: map[string]string{"a.go": suggestionFile},
		Threads: []forgefake.Thread{{Path: "a.go", Line: intPtr(2), Side: "right", Comments: []forgefake.Comment{{
			Author: "octo", Body: "```suggestion\nTWO\n```",
		}}}},
	})
	r := newRig(t, fixture)
	ctx := forgeapi.WithInteractive(t.Context())
	ref := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "merge", Number: 13}
	thread := threadByBody(mergeRead(t, r, ref), "```suggestion\nTWO\n```")
	if thread == nil {
		t.Fatal("the fork's thread did not read")
	}
	if s := thread.Comments[0].Suggestions; len(s) != 1 || s[0].Appliable {
		t.Fatalf("a fork head's suggestion = %+v (GitHub's viewerCanApplySuggestion says no)", s)
	}
	err := r.core.ApplyPRSuggestion(ctx, ref, gitops.SuggestionApply{Target: targetOf(thread, 0), HeadSHA: head})
	if got := explained(t, ref, gitops.ActionApplySuggestion, err); got.Kind != gitops.ForgeErrorRefused {
		t.Fatalf("fork apply = %s: %s", got.Kind, got.Message)
	}
	if routeCount(r, 0, "gh graphql SuggestionCommit") != 1 {
		t.Fatal("the fork apply was not attempted")
	}
	if read := mergeRead(t, r, ref); read.Detail.HeadSHA != head || threadByBody(read, "```suggestion\nTWO\n```").IsResolved {
		t.Fatalf("the refused apply changed the pull: head %s", read.Detail.HeadSHA)
	}
	expectAllHandled(t, r)
}

// GitLab's reactions read failing leaves the threads, says so once in
// ReactionsUnavailable, and is not retried before its backoff.
func TestGitLabReactionsReadFailureKeepsThreads(t *testing.T) {
	r := newRig(t, suggestionFixture(gitlabMergeFixture("success")))
	ref := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "merge", Number: 4}
	if err := r.engine.SetRefusal(forgefake.Refusal{Route: "glab graphql MRNoteReactions", Status: 403, Body: []byte(`{"message":"403 Forbidden"}`)}); err != nil {
		t.Fatalf("SetRefusal: %v", err)
	}
	read := mergeRead(t, r, ref)
	if threadByBody(read, "their note") == nil || len(read.Threads) != 2 {
		t.Fatalf("threads with the reactions read refused = %+v", read.Threads)
	}
	if read.ReactionsUnavailable != "Reactions are not shown: GitLab's reactions could not be read." {
		t.Fatalf("ReactionsUnavailable = %q", read.ReactionsUnavailable)
	}
	for _, thread := range read.Threads {
		if len(thread.Comments[0].Reactions) != 0 {
			t.Fatalf("reactions shown after a failed read: %+v", thread.Comments[0].Reactions)
		}
	}
	if err := r.engine.SetRefusal(forgefake.Refusal{Route: "glab graphql MRNoteReactions"}); err != nil {
		t.Fatalf("clear refusal: %v", err)
	}
	mark := lastSeq(r)
	if read = mergeRead(t, r, ref); read.ReactionsUnavailable == "" || routeCount(r, mark, "glab graphql MRNoteReactions") != 0 {
		t.Fatalf("a read inside the backoff retried: %q", read.ReactionsUnavailable)
	}
	expectAllHandled(t, r)
}

func lastSeq(r *rig) int {
	all := r.engine.Invocations(0).Invocations
	if len(all) == 0 {
		return 0
	}
	return all[len(all)-1].Seq
}
