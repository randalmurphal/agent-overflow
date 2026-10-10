package forgeapi

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner answers handoff commands from a table keyed by the joined
// argv and records every call.
type fakeRunner struct {
	mu      sync.Mutex
	answers map[string]fakeAnswer
	calls   []Process
	block   bool
}

type fakeAnswer struct {
	result ProcessResult
	err    error
}

func (f *fakeRunner) run(ctx context.Context, p Process) (ProcessResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, p)
	answer, ok := f.answers[p.Name+" "+strings.Join(p.Args, " ")]
	block := f.block
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return ProcessResult{}, ctx.Err()
	}
	if !ok {
		return ProcessResult{}, fmt.Errorf("fake runner: unexpected %s %v", p.Name, p.Args)
	}
	return answer.result, answer.err
}

func (f *fakeRunner) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		out = append(out, call.Name+" "+strings.Join(call.Args, " "))
	}
	return out
}

func ok(stdout string) fakeAnswer { return fakeAnswer{result: ProcessResult{Stdout: []byte(stdout)}} }

func exit(code int, stderr string) fakeAnswer {
	return fakeAnswer{result: ProcessResult{Stderr: []byte(stderr), ExitCode: code}}
}

func gitlabConfig(oauth2, apiHost, apiProtocol, token string) map[string]fakeAnswer {
	return map[string]fakeAnswer{
		"glab config get is_oauth2 --host gl.example":    ok(oauth2 + "\n"),
		"glab config get api_host --host gl.example":     ok(apiHost + "\n"),
		"glab config get api_protocol --host gl.example": ok(apiProtocol + "\n"),
		"glab config get token --host gl.example":        ok(token + "\n"),
	}
}

func TestCLITokenSourceReadsEachLoginKind(t *testing.T) {
	t.Parallel()
	t.Run("gh, with the port dropped from the host", func(t *testing.T) {
		runner := &fakeRunner{answers: map[string]fakeAnswer{"gh auth token --hostname ghe.example": ok(testSecret + "\n")}}
		token, info, err := CLITokenSource{Run: runner.run}.Token(t.Context(), ForgeGitHub, "ghe.example:8443")
		if err != nil || string(token) != testSecret || info.Header != AuthBearer {
			t.Fatalf("Token = %q, %+v, %v", token, info, err)
		}
		for _, call := range runner.calls {
			for _, want := range handoffEnv {
				if !slices.Contains(call.Env, want) {
					t.Fatalf("%v ran without %s", call.Args, want)
				}
			}
		}
	})
	t.Run("glab personal token with api_host", func(t *testing.T) {
		runner := &fakeRunner{answers: gitlabConfig("", "api.gl.example:8080", "http", "glpat-fake.token-1")}
		token, info, err := CLITokenSource{Run: runner.run}.Token(t.Context(), ForgeGitLab, "gl.example")
		if err != nil || string(token) != "glpat-fake.token-1" {
			t.Fatalf("Token = %q, %v", token, err)
		}
		if info.Header != AuthPrivateToken || info.APIHost != "api.gl.example:8080" || info.APIProtocol != "http" {
			t.Fatalf("info = %+v", info)
		}
		if slices.Contains(runner.commands(), "glab api --hostname gl.example version") {
			t.Fatal("a personal token was refreshed through glab api")
		}
	})
	t.Run("glab OAuth2 refreshes before the read and sends Bearer", func(t *testing.T) {
		answers := gitlabConfig("true", "", "", testSecret)
		answers["glab api --hostname gl.example version"] = ok(`{"version":"17"}`)
		runner := &fakeRunner{answers: answers}
		_, info, err := CLITokenSource{Run: runner.run}.Token(t.Context(), ForgeGitLab, "gl.example")
		if err != nil || info.Header != AuthBearer {
			t.Fatalf("info = %+v, err %v", info, err)
		}
		cmds := runner.commands()
		refresh := slices.Index(cmds, "glab api --hostname gl.example version")
		read := slices.Index(cmds, "glab config get token --host gl.example")
		if refresh < 0 || read < refresh {
			t.Fatalf("commands = %v, want the refresh before the token read", cmds)
		}
	})
	t.Run("glab keyring token from auth status, even on a failed exit", func(t *testing.T) {
		answers := gitlabConfig("", "", "", "")
		answers["glab auth status --hostname gl.example -t"] = exit(1, "gl.example\n  ✓ Logged in to gl.example as someone\n  x API check failed\n  ✓ Token: "+testSecret+"\n")
		runner := &fakeRunner{answers: answers}
		token, _, err := CLITokenSource{Run: runner.run}.Token(t.Context(), ForgeGitLab, "gl.example")
		if err != nil || string(token) != testSecret {
			t.Fatalf("Token = %q, %v", token, err)
		}
	})
}

func TestCLITokenSourceClassifiesFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		forge   string
		answers map[string]fakeAnswer
		block   bool
		kind    string // SetupError kind, or "transient"
	}{
		{name: "gh missing", forge: ForgeGitHub, answers: map[string]fakeAnswer{"gh auth token --hostname github.com": {err: &exec.Error{Name: "gh", Err: exec.ErrNotFound}}}, kind: SetupMissing},
		{name: "gh no oauth token", forge: ForgeGitHub, answers: map[string]fakeAnswer{"gh auth token --hostname github.com": exit(1, "no oauth token found for github.com")}, kind: SetupUnauthenticated},
		{name: "gh not logged in hint", forge: ForgeGitHub, answers: map[string]fakeAnswer{"gh auth token --hostname github.com": exit(4, "To get started with GitHub CLI, please run:  gh auth login")}, kind: SetupUnauthenticated},
		{name: "gh empty token", forge: ForgeGitHub, answers: map[string]fakeAnswer{"gh auth token --hostname github.com": ok("\n")}, kind: SetupUnauthenticated},
		{name: "gh locked keyring is transient", forge: ForgeGitHub, answers: map[string]fakeAnswer{"gh auth token --hostname github.com": exit(1, "failed to read from keyring: the keyring is locked")}, kind: "transient"},
		{name: "gh timeout is transient", forge: ForgeGitHub, block: true, kind: "transient"},
		{name: "gh token with a space is refused", forge: ForgeGitHub, answers: map[string]fakeAnswer{"gh auth token --hostname github.com": ok("two words\n")}, kind: "transient"},
		{name: "glab missing", forge: ForgeGitLab, answers: map[string]fakeAnswer{"glab config get is_oauth2 --host gitlab.com": {err: fmt.Errorf("exec: %w", exec.ErrNotFound)}}, kind: SetupMissing},
		{name: "glab not authenticated", forge: ForgeGitLab, answers: func() map[string]fakeAnswer {
			a := map[string]fakeAnswer{}
			for k, v := range gitlabConfig("", "", "", "") {
				a[strings.ReplaceAll(k, "gl.example", "gitlab.com")] = v
			}
			a["glab auth status --hostname gitlab.com -t"] = exit(1, "x gitlab.com has not been authenticated with glab. Run `glab auth login --hostname gitlab.com`")
			return a
		}(), kind: SetupUnauthenticated},
		{name: "glab OAuth2 refresh refused", forge: ForgeGitLab, answers: map[string]fakeAnswer{
			"glab config get is_oauth2 --host gitlab.com": ok("true"),
			"glab api --hostname gitlab.com version":      exit(1, "glab: 401 Unauthorized (HTTP 401)"),
		}, kind: SetupUnauthenticated},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &fakeRunner{answers: tc.answers, block: tc.block}
			host := "github.com"
			if tc.forge == ForgeGitLab {
				host = "gitlab.com"
			}
			ctx := t.Context()
			if tc.block {
				// The handoff's own 10s deadline is what fires in
				// production; a shorter caller deadline exercises the
				// same path without the wait.
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
			}
			_, _, err := CLITokenSource{Run: runner.run}.Token(ctx, tc.forge, host)
			if tc.block {
				if err == nil || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("err = %v, want the deadline", err)
				}
				return
			}
			var setup *SetupError
			switch {
			case tc.kind == "transient":
				var transient *TransientError
				if !errors.As(err, &transient) {
					t.Fatalf("err = %T %v, want *TransientError", err, err)
				}
			case !errors.As(err, &setup) || setup.Kind != tc.kind || setup.Forge != tc.forge:
				t.Fatalf("err = %T %v, want *SetupError{Kind: %s}", err, err, tc.kind)
			}
		})
	}
}

func TestHandoffDeadlineIsTransient(t *testing.T) {
	t.Parallel()
	err := classifyRunError(t.Context(), ForgeGitHub, "gh auth token", context.DeadlineExceeded)
	var transient *TransientError
	if !errors.As(err, &transient) || !strings.Contains(err.Error(), "timed out after 10s") {
		t.Fatalf("err = %v, want a transient timeout", err)
	}
}

func TestHandoffOutputNeverReachesAnError(t *testing.T) {
	t.Parallel()
	answers := map[string]fakeAnswer{
		"glab config get is_oauth2 --host gitlab.com":    ok(""),
		"glab config get api_host --host gitlab.com":     ok(""),
		"glab config get api_protocol --host gitlab.com": ok(""),
		"glab config get token --host gitlab.com":        exit(3, "keyring daemon crashed"),
	}
	runner := &fakeRunner{answers: answers}
	_, _, err := CLITokenSource{Run: runner.run}.Token(t.Context(), ForgeGitLab, "gitlab.com")
	if err == nil {
		t.Fatal("want an error")
	}
	// auth status prints the token and exits non-zero with a message that
	// names no login; neither the token nor that output may surface.
	answers = gitlabConfig("", "", "", "")
	answers["glab auth status --hostname gl.example -t"] = exit(1, "Token: "+"bad token with spaces "+testSecret)
	runner = &fakeRunner{answers: answers}
	_, _, err = CLITokenSource{Run: runner.run}.Token(t.Context(), ForgeGitLab, "gl.example")
	if err == nil || strings.Contains(err.Error(), testSecret) {
		t.Fatalf("err = %v leaked the auth status output", err)
	}
}
