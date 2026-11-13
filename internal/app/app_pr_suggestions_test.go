package app

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/mdsuggest"
)

// The suggestion and reaction writes run under the PR's write lock: while
// another write holds it, neither reaches the forge, and each gives up
// with its caller's context.
func TestPRSuggestionAndReactionWritesHoldThePRLock(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	app := NewApp()
	isolatedForgeCore(t, app, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	release, err := app.prManage.acquire(context.Background(), testGitLabPR.Identity())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	target := gitops.CommentTarget{ThreadID: "d1", CommentID: "31"}
	for name, write := range map[string]func(context.Context) error{
		"apply": func(ctx context.Context) error {
			_, err := app.ApplyPRSuggestion(ctx, testGitLabPR, gitops.SuggestionApply{Target: target, SuggestionID: "9", HeadSHA: "abcdef1", Digest: strings.Repeat("a", 64)})
			return err
		},
		"react": func(ctx context.Context) error {
			_, err := app.SetPRCommentReaction(ctx, testGitLabPR, target, gitops.ReactionHeart, true)
			return err
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		err := write(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s while the PR's lock is held = %v, want the context's end", name, err)
		}
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("%d forge requests while the lock was held", n)
	}
	release()
	// Free, the write reaches the forge and its failure is explained.
	_, err = app.SetPRCommentReaction(context.Background(), testGitLabPR, target, gitops.ReactionHeart, true)
	if explained, ok := errors.AsType[*gitops.ForgeActionError](err); !ok || explained.Kind != gitops.ForgeErrorNotFound {
		t.Fatalf("reaction against a forge that answers 404 = %T %v", err, err)
	}
	if requests.Load() == 0 {
		t.Fatal("the free write never reached the forge")
	}
}

// A request the adapter refuses never reaches the forge.
func TestPRSuggestionAndReactionWritesValidate(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	app := NewApp()
	isolatedForgeCore(t, app, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	ctx := context.Background()
	target := gitops.CommentTarget{ThreadID: "d1", CommentID: "31"}
	if _, err := app.ApplyPRSuggestion(ctx, testGitLabPR, gitops.SuggestionApply{Target: target, SuggestionID: "9", HeadSHA: "not a sha"}); err == nil {
		t.Fatal("ApplyPRSuggestion accepted a malformed head")
	}
	if _, err := app.ApplyPRSuggestion(ctx, testGitLabPR, gitops.SuggestionApply{Target: target, HeadSHA: "abcdef1"}); err == nil {
		t.Fatal("ApplyPRSuggestion accepted a GitLab apply without its suggestion id")
	}
	if _, err := app.SetPRCommentReaction(ctx, testPR, gitops.CommentTarget{ThreadID: "PRRT_x", CommentID: "PRRC_x"}, "party_parrot", true); err == nil {
		t.Fatal("SetPRCommentReaction accepted a reaction GitHub does not have")
	}
	if _, err := app.SetPRCommentReaction(ctx, testGitLabPR, target, "Not An Emoji", true); err == nil {
		t.Fatal("SetPRCommentReaction accepted a malformed GitLab name")
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("%d forge requests for refused input", n)
	}
}

// The reactions sentence reaches the subscriber and its frames, and a
// change of it alone is a frame.
func TestReactionsUnavailableReachesThePane(t *testing.T) {
	t.Parallel()
	app := NewApp()
	stubPRCIFetch(app)
	app.prUpdates.interval = 5 * time.Millisecond
	var sentence atomic.Value
	sentence.Store("Reactions are not shown: GitLab's reactions could not be read.")
	app.prUpdates.fetchFn = func(_ context.Context, pr gitops.PRReference) (prUpdateSnapshot, error) {
		return prUpdateSnapshot{Detail: gitops.PRDetail{Number: pr.Number, HeadSHA: "head"}, ReactionsUnavailable: sentence.Load().(string)}, nil
	}
	events := capturePRUpdates(t, app)
	sub, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("SubscribePRUpdates: %v", err)
	}
	t.Cleanup(func() {
		_ = app.UnsubscribePRUpdates(context.Background(), sub.ID)
		app.prUpdates.wg.Wait()
	})
	if sub.ReactionsUnavailable != "Reactions are not shown: GitLab's reactions could not be read." {
		t.Fatalf("subscribe ReactionsUnavailable = %q", sub.ReactionsUnavailable)
	}
	expectNoPRUpdate(t, events, "an unchanged snapshot")
	sentence.Store("")
	if frame := awaitPRUpdate(t, events, "the sentence cleared"); frame.ReactionsUnavailable != "" {
		t.Fatalf("frame ReactionsUnavailable = %q", frame.ReactionsUnavailable)
	}
}

func TestParseCommentSuggestions(t *testing.T) {
	t.Parallel()
	app := NewApp()
	got, err := app.ParseCommentSuggestions("Try\n```suggestion\nx\n```\n")
	if err != nil || !reflect.DeepEqual(got, []mdsuggest.Block{{Lines: []string{"x"}}}) {
		t.Fatalf("one block = %#v, %v", got, err)
	}
	// None is an empty list, not null, on the wire.
	if got, err := app.ParseCommentSuggestions("plain"); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no block = %#v, %v", got, err)
	}
	if _, err := app.ParseCommentSuggestions(strings.Repeat("a", commentBodyMaxBytes+1)); err == nil {
		t.Fatal("an oversized body was parsed")
	}
}
