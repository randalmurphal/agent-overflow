package forgeapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"agent-overflow/internal/appimage"
	"agent-overflow/internal/procutil"
)

// The forge ids a Service serves.
const (
	ForgeGitHub = "github"
	ForgeGitLab = "gitlab"
)

// SourceInfo is what a token source learned about a host's login beside
// the token.
type SourceInfo struct {
	// Header is how the token is presented.
	Header AuthHeader
	// APIHost and APIProtocol are glab's per-host api_host and
	// api_protocol, empty when unset. A GitLab REST base is
	// APIProtocol://APIHost/api/v4/, defaulting to https and the host.
	APIHost     string
	APIProtocol string
}

// TokenSource reads a host's token. forge is ForgeGitHub or ForgeGitLab;
// host is spelled as PRReference.Host (a port possibly included). It
// returns *SetupError when the login is missing, *TransientError when the
// read failed for another reason, and the caller's context error when the
// caller cancelled. The returned bytes belong to the caller, which zeroes
// them.
type TokenSource interface {
	Token(ctx context.Context, forge, host string) ([]byte, SourceInfo, error)
}

// Process is one handoff command a CLITokenSource runs.
type Process struct {
	Name string
	Args []string
	// Env is appended to the runner's base environment.
	Env []string
}

// ProcessResult is a finished handoff command. A non-zero ExitCode is a
// result, not an error.
type ProcessResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// ProcessRunner runs one handoff command under ctx. It returns an error
// only when the command could not run to an exit status: not found
// (exec.ErrNotFound in the chain), context cancelled or timed out, or
// output over its bound.
type ProcessRunner func(ctx context.Context, p Process) (ProcessResult, error)

// handoffTimeout bounds every handoff command.
const handoffTimeout = 10 * time.Second

// handoffEnv keeps the CLIs non-interactive and their output plain.
var handoffEnv = []string{"GH_PROMPT_DISABLED=1", "GH_DEBUG=", "GLAB_CHECK_UPDATE=false", "GLAB_DEBUG_HTTP=false"}

// maxHandoffOutput bounds what a handoff command may print.
const maxHandoffOutput = 64 << 10

// CLITokenSource reads tokens through the user's own gh and glab logins,
// running the handoff commands listed in
// docs/architecture/forge-transport.md#what-still-runs-a-cli. It keeps no
// state: the Service caches what it returns.
type CLITokenSource struct {
	// Run executes a command; nil uses ExecProcess.
	Run ProcessRunner
}

// Token implements TokenSource. A GitLab OAuth2 login is refreshed
// through `glab api --hostname H version` before every read, the first
// included, so the token read after it is current.
func (s CLITokenSource) Token(ctx context.Context, forge, host string) ([]byte, SourceInfo, error) {
	name := (&url.URL{Host: host}).Hostname()
	if name == "" {
		return nil, SourceInfo{}, fmt.Errorf("forgeapi: no host to read a %s token for", forge)
	}
	switch forge {
	case ForgeGitHub:
		out, err := s.run(ctx, forge, true, "gh", "auth", "token", "--hostname", name)
		if err != nil {
			return nil, SourceInfo{}, err
		}
		defer clear(out.Stdout)
		token, err := handoffToken(forge, bytes.TrimSpace(out.Stdout))
		return token, SourceInfo{Header: AuthBearer}, err
	case ForgeGitLab:
		return s.gitlabToken(ctx, name)
	}
	return nil, SourceInfo{}, fmt.Errorf("forgeapi: unknown forge %q", forge)
}

func (s CLITokenSource) gitlabToken(ctx context.Context, name string) ([]byte, SourceInfo, error) {
	config := func(key string) (string, error) {
		out, err := s.run(ctx, ForgeGitLab, false, "glab", "config", "get", key, "--host", name)
		if err != nil {
			return "", err
		}
		return string(bytes.TrimSpace(out.Stdout)), nil
	}
	oauth2, err := config("is_oauth2")
	if err != nil {
		return nil, SourceInfo{}, err
	}
	info := SourceInfo{Header: AuthPrivateToken}
	if oauth2 == "true" {
		info.Header = AuthBearer
		if _, err := s.run(ctx, ForgeGitLab, false, "glab", "api", "--hostname", name, "version"); err != nil {
			return nil, SourceInfo{}, err
		}
	}
	if info.APIHost, err = config("api_host"); err != nil {
		return nil, SourceInfo{}, err
	}
	if info.APIProtocol, err = config("api_protocol"); err != nil {
		return nil, SourceInfo{}, err
	}
	out, err := s.run(ctx, ForgeGitLab, true, "glab", "config", "get", "token", "--host", name)
	if err != nil {
		return nil, SourceInfo{}, err
	}
	defer clear(out.Stdout)
	if secret := bytes.TrimSpace(out.Stdout); len(secret) > 0 {
		token, err := handoffToken(ForgeGitLab, secret)
		return token, info, err
	}
	// The token lives in the OS keyring. glab prints it on stderr, and
	// exits non-zero when its own API check fails even though it printed
	// the token, so a token line is taken whatever the exit status and
	// this command's output never reaches an error.
	status, err := s.handoff(ctx, ForgeGitLab, "glab", "auth", "status", "--hostname", name, "-t")
	if err != nil {
		return nil, SourceInfo{}, err
	}
	combined := append(append([]byte(nil), status.Stderr...), status.Stdout...)
	defer clear(combined)
	clear(status.Stderr)
	clear(status.Stdout)
	if m := glabStatusToken.FindSubmatch(combined); m != nil {
		token, err := handoffToken(ForgeGitLab, m[1])
		return token, info, err
	}
	if status.ExitCode == 0 || namesMissingLogin(string(combined)) {
		return nil, SourceInfo{}, UnauthenticatedError(ForgeGitLab, fmt.Errorf("glab auth status --hostname %s printed no token", name))
	}
	return nil, SourceInfo{}, &TransientError{Err: fmt.Errorf("glab auth status --hostname %s exited with status %d and printed no token", name, status.ExitCode)}
}

// glabStatusToken finds the token line of `glab auth status -t`.
var glabStatusToken = regexp.MustCompile(`(?m)Token:\s*([!-~]+)\s*$`)

func (s CLITokenSource) runner() ProcessRunner {
	if s.Run != nil {
		return s.Run
	}
	return ExecProcess
}

// handoff runs one handoff command under its deadline. It fails only when
// the command did not reach an exit status.
func (s CLITokenSource) handoff(ctx context.Context, forge, name string, args ...string) (ProcessResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, handoffTimeout)
	defer cancel()
	out, err := s.runner()(runCtx, Process{Name: name, Args: args, Env: handoffEnv})
	if err != nil {
		return ProcessResult{}, classifyRunError(ctx, forge, name+" "+strings.Join(args, " "), err)
	}
	return out, nil
}

// run executes one handoff command and classifies a non-zero exit. secret
// marks a command whose stdout is a token: its output never reaches an
// error.
func (s CLITokenSource) run(ctx context.Context, forge string, secret bool, name string, args ...string) (ProcessResult, error) {
	out, err := s.handoff(ctx, forge, name, args...)
	if err != nil || out.ExitCode == 0 {
		return out, err
	}
	if secret {
		clear(out.Stdout)
	}
	command := name + " " + strings.Join(args, " ")
	stderr := string(bytes.TrimSpace(out.Stderr))
	failure := fmt.Errorf("%s exited with status %d: %s", command, out.ExitCode, boundText(stderr))
	if namesMissingLogin(stderr) {
		return ProcessResult{}, UnauthenticatedError(forge, failure)
	}
	return ProcessResult{}, &TransientError{Err: failure}
}

// classifyRunError maps a command that did not reach an exit status. ctx
// is the caller's: its cancellation is returned as is, while the handoff
// deadline is a transient failure.
func classifyRunError(ctx context.Context, forge, command string, err error) error {
	if errors.Is(err, exec.ErrNotFound) {
		return MissingCLIError(forge, err)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", command, ctx.Err())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &TransientError{Err: fmt.Errorf("%s timed out after %s", command, handoffTimeout)}
	}
	return &TransientError{Err: fmt.Errorf("%s: %w", command, err)}
}

// missingLogin matches the stderr of a CLI that has no usable login for
// the host: gh's "no oauth token found for H" and its "gh auth login"
// hint, glab's "has not been authenticated", its "glab auth login" hint
// and a refresh glab's API answered 401 Unauthorized.
var missingLogin = regexp.MustCompile(`(?i)auth login|not logged in|not been authenticated|no (oauth )?token|unauthorized`)

func namesMissingLogin(stderr string) bool { return missingLogin.MatchString(stderr) }

// handoffToken validates a printed token: one line of printable ASCII
// with no spaces, so it can never split a header. An empty one is no
// login.
func handoffToken(forge string, secret []byte) ([]byte, error) {
	if len(secret) == 0 {
		return nil, UnauthenticatedError(forge, errors.New("the CLI printed no token"))
	}
	for _, c := range secret {
		if c < '!' || c > '~' {
			return nil, &TransientError{Err: errors.New("the CLI printed a token with characters a header cannot carry")}
		}
	}
	return append([]byte(nil), secret...), nil
}

func boundText(s string) string {
	const limit = 512
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}

// ExecProcess is the production ProcessRunner: the named binary on PATH,
// with the scrubbed process environment plus p.Env. It is the only place
// this package starts a process.
func ExecProcess(ctx context.Context, p Process) (ProcessResult, error) {
	path, err := exec.LookPath(p.Name)
	if err != nil {
		return ProcessResult{}, err
	}
	cmd := exec.CommandContext(ctx, path, p.Args...)
	cmd.Args[0] = p.Name
	cmd.Env = append(appimage.Scrub(os.Environ()), p.Env...)
	stdout := &boundedBuffer{limit: maxHandoffOutput}
	stderr := &boundedBuffer{limit: maxHandoffOutput}
	err = procutil.RunDrained(ctx, cmd, stdout, stderr, 2*time.Second)
	if ctx.Err() != nil {
		return ProcessResult{}, ctx.Err()
	}
	if stdout.over || stderr.over {
		return ProcessResult{}, fmt.Errorf("%s printed more than %d bytes", p.Name, maxHandoffOutput)
	}
	result := ProcessResult{Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes()}
	if err == nil {
		return result, nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return ProcessResult{}, err
}

type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); len(p) > room {
		b.over = true
		b.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return b.buf.Write(p)
}
