package git

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
	"agent-overflow/internal/testutil/mockexec"
)

// A forge write the caller stops waiting for still finishes: the forge may
// already have applied it, and a caller that gave up cannot tell. Each case
// holds its first write until the caller's context is cancelled, then lets
// the forge answer.
func TestForgeWritesFinishAfterTheirCallerCancels(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	discussion := mr + "/discussions/abc"
	line := 4
	review := SubmitReviewRequest{Verdict: ReviewVerdictApprove, Body: "Looks good", Comments: []ReviewLineComment{
		{Path: "a.go", Body: "nit", Line: &line, Side: "right"},
	}}
	cases := map[string]struct {
		hold  string
		write func(context.Context, *Core) error
		want  []string
	}{
		"submit review": {
			hold: "POST " + mr + "/draft_notes",
			write: func(ctx context.Context, core *Core) error {
				_, err := core.SubmitReview(ctx, gitlabTestRef(7), review)
				return err
			},
			want: []string{"GET " + mr, "POST " + mr + "/draft_notes", "POST " + mr + "/draft_notes", "POST " + mr + "/draft_notes/bulk_publish", "POST " + mr + "/approve"},
		},
		"approve": {
			hold:  "POST " + mr + "/approve",
			write: func(ctx context.Context, core *Core) error { return core.ApprovePR(ctx, gitlabTestRef(7), "abc1234") },
			want:  []string{"POST " + mr + "/approve"},
		},
		"reply": {
			hold: "POST " + discussion + "/notes",
			write: func(ctx context.Context, core *Core) error {
				return core.ReplyToThread(ctx, gitlabTestRef(7), "abc", 0, "Thanks")
			},
			want: []string{"POST " + discussion + "/notes"},
		},
		"resolve": {
			hold: "PUT " + discussion,
			write: func(ctx context.Context, core *Core) error {
				return core.SetThreadResolved(ctx, gitlabTestRef(7), "abc", true)
			},
			want: []string{"PUT " + discussion},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			arrived := make(chan struct{})
			release := make(chan struct{})
			held := false
			core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
				key := call.Method + " " + call.Path
				if key == tc.hold && !held {
					held = true
					close(arrived)
					<-release
				}
				switch {
				case key == "GET "+mr:
					return forgeAPIAnswer{Body: gitlabTestMR(7, 0)}
				case key == "POST "+mr+"/draft_notes":
					return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":11}`}
				case key == "POST "+mr+"/draft_notes/bulk_publish":
					return forgeAPIAnswer{Status: http.StatusNoContent}
				case strings.HasPrefix(key, "POST ") || strings.HasPrefix(key, "PUT "):
					return forgeAPIAnswer{Status: http.StatusCreated, Body: `{}`}
				}
				return forgeUnexpected(t, call)
			})
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- tc.write(ctx, core) }()
			select {
			case <-arrived:
			case <-time.After(10 * time.Second):
				t.Fatal("the write never reached the forge")
			}
			cancel()
			// Long enough for an aborted request to have returned already.
			time.Sleep(50 * time.Millisecond)
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("write after the caller cancelled = %v, want it to finish", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the write did not finish")
			}
			if got := gitlabPaths(calls); strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("requests = %q, want %q", got, tc.want)
			}
		})
	}
}

// gh pr create pushes and then opens the PR; a caller that stops waiting
// once it started does not kill it between the two.
func TestCreatePRFinishesAfterItsCallerCancels(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}
	binDir := t.TempDir()
	started := filepath.Join(binDir, "started")
	release := filepath.Join(binDir, "release")
	script := fmt.Sprintf("#!/bin/sh\n: > %q\nwhile [ ! -e %q ]; do sleep 0.01; done\necho 'https://github.com/acme/repo/pull/9'\n", started, release)
	mockexec.Write(t, filepath.Join(binDir, "gh"), script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	repo := initGitRepo(t)
	testutil.RunGit(t, repo, "remote", "add", "origin", "https://github.com/acme/repo.git")

	ctx, cancel := context.WithCancel(t.Context())
	type answer struct {
		url string
		err error
	}
	done := make(chan answer, 1)
	go func() {
		url, err := NewCore().CreatePR(ctx, repo, "Demo", "Body", "", false)
		done <- answer{url, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gh never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.url != "https://github.com/acme/repo/pull/9" {
			t.Fatalf("CreatePR after the caller cancelled = %q, %v; want the PR it opened", got.url, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CreatePR did not finish")
	}
}
