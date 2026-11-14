package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/testutil"
)

// prRevisionRead is one PR read at a revision, made with or without a
// person asking for it.
type prRevisionRead struct {
	name string
	read func(t *testing.T, app *App, ref WorkspaceRef, rev gitops.PRRevision, sha string, userInitiated bool) error
}

func prRevisionReads() []prRevisionRead {
	return []prRevisionRead{
		{"OpenPRDiff", func(t *testing.T, app *App, ref WorkspaceRef, rev gitops.PRRevision, _ string, userInitiated bool) error {
			ctx, _ := reviewDiffConn(t)
			_, err := app.OpenPRDiff(ctx, ref, prRef(), rev, false, userInitiated)
			return err
		}},
		{"ListPRCommits", func(_ *testing.T, app *App, ref WorkspaceRef, rev gitops.PRRevision, _ string, userInitiated bool) error {
			_, err := app.ListPRCommits(ref, prRef(), rev, userInitiated)
			return err
		}},
		{"OpenPRCommitDiff", func(t *testing.T, app *App, ref WorkspaceRef, rev gitops.PRRevision, sha string, userInitiated bool) error {
			ctx, _ := reviewDiffConn(t)
			_, err := app.OpenPRCommitDiff(ctx, ref, prRef(), rev, sha, sha, false, userInitiated)
			return err
		}},
		{"GetPRMergeConflicts", func(_ *testing.T, app *App, ref WorkspaceRef, rev gitops.PRRevision, _ string, userInitiated bool) error {
			_, err := app.GetPRMergeConflicts(ref, prRef(), rev, "feature", userInitiated)
			return err
		}},
	}
}

// Only a read the user asked for arms the pump's forced emit after a
// failure, for every PR read at a revision. An automatic read (a pump
// frame, a reconcile, a retry) that fails reports its error and arms
// nothing, so a failure cannot chain into another automatic read on every
// poll.
func TestPRRevisionReadsOnlyAUserReadArmsTheRetry(t *testing.T) {
	t.Parallel()
	for _, tc := range prRevisionReads() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newReviewDiffTestApp(t)
			ref, clone, prSHAs := prCloneFixture(t, app)
			origin := moveOriginMainOn(t, clone)
			rev := gitops.PRRevision{BaseRef: "main", BaseSHA: revParseIn(t, origin, "main"), HeadSHA: prSHAs[0]}
			testutil.RunGit(t, clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
			pump := &prUpdatePump{}
			app.prUpdates.mu.Lock()
			if app.prUpdates.pumps == nil {
				app.prUpdates.pumps = make(map[string]*prUpdatePump)
			}
			app.prUpdates.pumps[prUpdateKey(prRef())] = pump
			app.prUpdates.mu.Unlock()
			armed := func() bool {
				app.prUpdates.mu.Lock()
				defer app.prUpdates.mu.Unlock()
				return pump.readFailed
			}

			if err := tc.read(t, app, ref, rev, prSHAs[0], false); err == nil || !strings.Contains(err.Error(), "fetch pull request commits") {
				t.Fatalf("automatic read err = %v, want the fetch failure", err)
			}
			if armed() {
				t.Fatal("a failed automatic read armed the pump's forced emit")
			}
			if err := tc.read(t, app, ref, rev, prSHAs[0], true); err == nil {
				t.Fatal("user read: want the fetch failure")
			}
			if !armed() {
				t.Fatal("a failed user read did not arm the pump's forced emit")
			}
		})
	}
}

// Every PR read at a revision carries its intent to the fetch of the
// commits the clone lacks (EnsurePRObjects): against a remote that asks for
// credentials, an automatic read never launches the user's askpass helper
// and a read the user asked for does. Serial: it sets the process
// environment.
func TestPRRevisionReadsPromptOnlyForAUserRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script askpass helper is unix-only")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	// A clean config chain: a real user's credential.helper would answer
	// before git reaches the askpass helper this test is about.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	for _, tc := range prRevisionReads() {
		t.Run(tc.name, func(t *testing.T) {
			app := newReviewDiffTestApp(t)
			ref, clone, prSHAs := prCloneFixture(t, app)
			// The base commit is one the clone lacks, so the read must fetch.
			origin := moveOriginMainOn(t, clone)
			rev := gitops.PRRevision{BaseRef: "main", BaseSHA: revParseIn(t, origin, "main"), HeadSHA: prSHAs[0]}
			testutil.RunGit(t, clone, "remote", "set-url", "origin", server.URL+"/repo.git")
			dir := t.TempDir()
			askpassLog := filepath.Join(dir, "askpass.log")
			askpass := filepath.Join(dir, "askpass.sh")
			if err := os.WriteFile(askpass, []byte("#!/bin/sh\necho invoked >> '"+askpassLog+"'\necho someone\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_ASKPASS", askpass)
			asked := func() bool {
				_, err := os.Stat(askpassLog)
				return err == nil
			}

			if err := tc.read(t, app, ref, rev, prSHAs[0], false); err == nil || !strings.Contains(err.Error(), "fetch pull request commits") {
				t.Fatalf("automatic read err = %v, want the fetch failure", err)
			}
			if asked() {
				t.Fatal("an automatic read launched the askpass helper")
			}
			if err := tc.read(t, app, ref, rev, prSHAs[0], true); err == nil {
				t.Fatal("user read against a remote refusing every credential should fail")
			}
			if !asked() {
				t.Fatal("a user's read did not reach the askpass helper")
			}
		})
	}
}
