// Package forgefake answers the gh and glab invocations an isolated boot
// makes, from a fixture a test seeds.
//
// The app never talks to this package directly. internal/git runs every
// forge CLI through one seam that, under --harness and --soak, executes
// cmd/ao-mockforge instead; that binary forwards its argv, cwd and stdin
// over the harness control channel (internal/harness/control, POST
// /forge) and prints whatever Engine.Handle answers. Routing, state and
// the invocation log therefore live in the harness process, where the
// test can seed and inspect them through the harness wire.
//
// Every invocation is recorded. One no handler claims, or one carrying a
// flag, JSON field, query parameter or GraphQL query shape its handler
// does not implement, fails with the full argv on stderr, so a change to
// what the app asks a forge CLI surfaces as a failing spec rather than a
// plausible empty answer. See AGENTS.md beside this file for how to add
// an endpoint.
package forgefake

import (
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/harness/control"
)

// maxInvocations bounds the recorded log. A soak run polls PR state for
// hours; the oldest entries go first and Dropped counts them.
const maxInvocations = 4096

// defaultViewer is the signed-in user when the fixture names none.
const defaultViewer = "ao-viewer"

// Invocation is one recorded forge CLI call and its outcome.
type Invocation struct {
	Seq  int      `json:"seq"`
	CLI  string   `json:"cli"`
	Args []string `json:"args"`
	Cwd  string   `json:"cwd"`
	// Stdin is what the app wrote to the CLI, as text.
	Stdin string `json:"stdin,omitempty"`
	// Route names the handler that answered. Empty when Unhandled.
	Route string `json:"route,omitempty"`
	// Unhandled marks an invocation no handler implements. Stderr then
	// carries the reason and the full argv.
	Unhandled   bool      `json:"unhandled,omitempty"`
	ExitCode    int       `json:"exitCode"`
	Stderr      string    `json:"stderr,omitempty"`
	StdoutBytes int       `json:"stdoutBytes"`
	At          time.Time `json:"at"`
}

// InvocationLog is a window of the recorded invocations.
type InvocationLog struct {
	Invocations []Invocation `json:"invocations"`
	// Dropped counts invocations evicted by the log bound since the last
	// reset.
	Dropped int `json:"dropped"`
}

// Options wires an Engine to its host.
type Options struct {
	// OnInvocation observes every recorded invocation after it is
	// answered. The harness re-emits it as a harness:forge event. Called
	// without the engine lock held.
	OnInvocation func(Invocation)
	// Origin answers a working directory's origin remote URL, for the
	// invocations that name no repository and let the CLI infer it from
	// the checkout (`gh pr list`, glab's `:fullpath`). Defaults to asking
	// git.
	Origin func(cwd string) (string, error)
}

// Engine is the fake forge: seeded state, the route table's host and the
// invocation log. Safe for concurrent use; every call is answered under
// one lock, so concurrent CLI processes see a consistent fixture.
type Engine struct {
	opts Options

	mu      sync.Mutex
	ids     idSource
	viewer  string
	repos   map[string]*Repo
	log     []Invocation
	seq     int
	dropped int
}

// New builds an empty engine. Every invocation against it answers "not
// found" until a fixture is seeded.
func New(opts Options) *Engine {
	if opts.Origin == nil {
		opts.Origin = gitOrigin
	}
	return &Engine{opts: opts, viewer: defaultViewer, repos: make(map[string]*Repo)}
}

// Seed adds the fixture's repositories, replacing any already seeded
// under the same (forge, project), and returns the fixture with every
// generated id and default filled in. The caller decodes the fixture
// strictly: an unknown field is a typo that would seed something other
// than what the test asserts against.
func (e *Engine) Seed(fixture Fixture) (Fixture, error) {
	fixture = fixture.clone()
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := fixture.normalize(&e.ids); err != nil {
		return Fixture{}, err
	}
	for i := range fixture.Repos {
		repo := &fixture.Repos[i]
		if repo.Forge != "gitlab" {
			continue
		}
		for key, other := range e.repos {
			if key != repo.key() && other.Forge == "gitlab" && other.ID == repo.ID {
				return Fixture{}, fmt.Errorf("forge fixture: GitLab project id %d is already %s", repo.ID, other.Project)
			}
		}
	}
	if fixture.Viewer != "" {
		e.viewer = fixture.Viewer
	}
	fixture.Viewer = e.viewer
	for i := range fixture.Repos {
		repo := fixture.Repos[i]
		e.repos[repo.key()] = &repo
	}
	return fixture.clone(), nil
}

// Reset drops the seeded state and the invocation log. Ids keep counting
// so none repeats across tests.
func (e *Engine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.viewer = defaultViewer
	clear(e.repos)
	e.log = nil
	e.dropped = 0
}

// Invocations returns the recorded invocations with Seq > since.
func (e *Engine) Invocations(since int) InvocationLog {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := InvocationLog{Invocations: []Invocation{}, Dropped: e.dropped}
	for _, inv := range e.log {
		if inv.Seq > since {
			inv.Args = slices.Clone(inv.Args)
			out.Invocations = append(out.Invocations, inv)
		}
	}
	return out
}

// Handle answers one forwarded invocation and records it.
func (e *Engine) Handle(fc control.ForgeCall) control.ForgeResult {
	c := &call{cli: fc.CLI, args: slices.Clone(fc.Args), cwd: fc.Cwd, stdin: fc.Stdin}

	e.mu.Lock()
	resp := e.dispatch(c)
	if resp.unhandled != "" {
		resp.stdout = nil
		resp.stderr = fmt.Sprintf("ao-mockforge: unhandled %s invocation (%s): %s\n"+
			"Add a handler in internal/harness/forgefake (see its AGENTS.md).\n",
			c.cli, resp.unhandled, formatArgv(c.cli, c.args))
		resp.exit = 1
	}
	e.seq++
	inv := Invocation{
		Seq:         e.seq,
		CLI:         c.cli,
		Args:        slices.Clone(c.args),
		Cwd:         c.cwd,
		Stdin:       string(c.stdin),
		Route:       resp.route,
		Unhandled:   resp.unhandled != "",
		ExitCode:    resp.exit,
		Stderr:      resp.stderr,
		StdoutBytes: len(resp.stdout),
		At:          time.Now().UTC(),
	}
	if inv.Unhandled {
		inv.Route = ""
	}
	e.log = append(e.log, inv)
	if over := len(e.log) - maxInvocations; over > 0 {
		e.log = slices.Delete(e.log, 0, over)
		e.dropped += over
	}
	e.mu.Unlock()

	if e.opts.OnInvocation != nil {
		inv.Args = slices.Clone(inv.Args)
		e.opts.OnInvocation(inv)
	}
	return control.ForgeResult{Stdout: resp.stdout, Stderr: resp.stderr, ExitCode: resp.exit}
}

// dispatch finds the command whose path is the longest prefix of the
// argv, parses its flags and runs it. Callers hold mu.
func (e *Engine) dispatch(c *call) response {
	var best *command
	for i := range commands {
		cmd := &commands[i]
		if cmd.cli != c.cli || len(cmd.path) > len(c.args) || !slices.Equal(cmd.path, c.args[:len(cmd.path)]) {
			continue
		}
		if best == nil || len(cmd.path) > len(best.path) {
			best = cmd
		}
	}
	if best == nil {
		return unhandled("no command matches")
	}
	flags, positional, err := parseFlags(c.args[len(best.path):], best.flags)
	if err != nil {
		return unhandled("%v", err)
	}
	c.flags, c.positional = flags, positional
	resp := best.run(e, c)
	if resp.route == "" {
		resp.route = best.cli + " " + strings.Join(best.path, " ")
	}
	return resp
}

// response is a handler's answer.
type response struct {
	route     string
	stdout    []byte
	stderr    string
	exit      int
	unhandled string
}

func unhandled(reason string, args ...any) response {
	return response{unhandled: fmt.Sprintf(reason, args...)}
}

func formatArgv(cli string, args []string) string {
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, cli)
	for _, arg := range args {
		quoted = append(quoted, strconv.Quote(arg))
	}
	return strings.Join(quoted, " ")
}

// repo returns the seeded repository, matching the project
// case-insensitively as both forges do. A GitLab project may also be
// named by its numeric id. Callers hold mu.
func (e *Engine) repo(forge, project string) *Repo {
	if r, ok := e.repos[forge+"\x00"+strings.ToLower(project)]; ok {
		return r
	}
	if forge == "gitlab" {
		if id, err := strconv.ParseInt(project, 10, 64); err == nil {
			for _, r := range e.repos {
				if r.Forge == "gitlab" && r.ID == id {
					return r
				}
			}
		}
	}
	return nil
}

func (r *Repo) pull(number int) *Pull {
	for i := range r.Pulls {
		if r.Pulls[i].Number == number {
			return &r.Pulls[i]
		}
	}
	return nil
}

// repoForCheckout resolves the repository a CLI would infer from the
// checkout at cwd: its origin remote, matched on host and project.
// Callers hold mu.
func (e *Engine) repoForCheckout(forge, cwd string) (*Repo, error) {
	if cwd == "" {
		return nil, fmt.Errorf("no working directory to infer the %s repository from", forge)
	}
	remote, err := e.opts.Origin(cwd)
	if err != nil {
		return nil, fmt.Errorf("could not read the origin remote of %s: %w", cwd, err)
	}
	host, project, ok := parseRemote(remote)
	if !ok {
		return nil, fmt.Errorf("origin remote %q of %s is not a forge URL", remote, cwd)
	}
	r := e.repo(forge, project)
	if r == nil || !strings.EqualFold(r.Host, host) {
		return nil, fmt.Errorf("no seeded %s repository matches the origin remote %q of %s", forge, remote, cwd)
	}
	return r, nil
}

// gitOrigin reads origin the way the app's forge detection does.
func gitOrigin(cwd string) (string, error) {
	out, err := exec.Command("git", "-C", cwd, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// parseRemote splits an https, ssh:// or scp-style remote into host and
// project path.
func parseRemote(remote string) (host, project string, ok bool) {
	rest := remote
	switch {
	case strings.Contains(rest, "://"):
		_, rest, _ = strings.Cut(rest, "://")
		if at := strings.Index(rest, "@"); at >= 0 && at < strings.Index(rest+"/", "/") {
			rest = rest[at+1:]
		}
		host, rest, ok = strings.Cut(rest, "/")
		if h, _, hasPort := strings.Cut(host, ":"); hasPort {
			host = h
		}
	default:
		if at := strings.Index(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		host, rest, ok = strings.Cut(rest, ":")
	}
	project = strings.TrimSuffix(strings.Trim(rest, "/"), ".git")
	return host, project, ok && host != "" && strings.Contains(project, "/")
}
