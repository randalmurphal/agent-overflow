// Package forgefake answers forge CLI calls, forge API requests and SSH
// configuration reads from a fixture a test seeds.
//
// internal/git runs every forge CLI through one seam that, under --harness
// and --soak, executes cmd/ao-mockforge instead; that binary forwards its
// argv, cwd and stdin over the harness control channel
// (internal/harness/control, POST /forge) and prints whatever
// Engine.Handle answers. The forge API transport of an isolated boot
// sends its requests to the harness's own listener, which Engine.ServeHTTP
// answers from the same route tables. Routing, state and the invocation
// log therefore live in the harness process, where the test can seed and
// inspect them through the harness wire.
//
// Every invocation is recorded. One no handler claims, or one carrying a
// flag, JSON field, query parameter or GraphQL operation signature its
// handler does not implement, fails with the full argv or request, so a
// change to what the app asks a forge surfaces as a failing spec rather
// than a plausible empty answer. See AGENTS.md beside this file for how to add
// an endpoint.
package forgefake

import (
	"encoding/json"
	"fmt"
	"net/http"
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

// Invocation is one recorded forge call and its outcome: a CLI
// invocation ao-mockforge forwarded (Via "cli") or a request to the HTTP
// mounts (Via "http"). Each serializes only its own variant's fields; see
// MarshalJSON.
type Invocation struct {
	Seq int    `json:"seq"`
	Via string `json:"via"`
	// Route names the handler that answered. Empty when Unhandled.
	Route string `json:"route,omitempty"`
	// Unhandled marks an invocation no handler implements. Stderr (CLI) or
	// Detail (HTTP) then carries the reason and the call.
	Unhandled bool      `json:"unhandled,omitempty"`
	At        time.Time `json:"at"`

	// CLI variant.
	CLI  string   `json:"cli,omitempty"`
	Args []string `json:"args,omitempty"`
	Cwd  string   `json:"cwd,omitempty"`
	// Stdin is what the app wrote to the CLI, as text.
	Stdin       string `json:"stdin,omitempty"`
	ExitCode    int    `json:"exitCode"`
	Stderr      string `json:"stderr,omitempty"`
	StdoutBytes int    `json:"stdoutBytes"`

	// HTTP variant. Host is the forge host the request addressed (its
	// Host header, port included) or an attachment's host. Path is the
	// request path and query relative to the forge base
	// ("repos/o/r/pulls/1", "graphql"), or an attachment's absolute URL
	// without its query.
	Forge  string `json:"forge,omitempty"`
	Host   string `json:"host,omitempty"`
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
	// Operation and Variables are a GraphQL request's operationName and
	// variables as sent.
	Operation string          `json:"operation,omitempty"`
	Variables json.RawMessage `json:"variables,omitempty"`
	Status    int             `json:"status,omitempty"`
	Detail    string          `json:"detail,omitempty"`
}

// Invocation variants.
const (
	ViaCLI  = "cli"
	ViaHTTP = "http"
)

// MarshalJSON writes the union the e2e helpers narrow on via:
// {seq, route, unhandled, via: "cli", cli, args, cwd, stdin, exitCode,
// stderr, ...} or {seq, route, unhandled, via: "http", forge, host,
// method, path, operation, variables, status, detail}.
func (inv Invocation) MarshalJSON() ([]byte, error) {
	type common struct {
		Seq       int       `json:"seq"`
		Via       string    `json:"via"`
		Route     string    `json:"route,omitempty"`
		Unhandled bool      `json:"unhandled,omitempty"`
		At        time.Time `json:"at"`
	}
	head := common{Seq: inv.Seq, Via: inv.Via, Route: inv.Route, Unhandled: inv.Unhandled, At: inv.At}
	if inv.Via == ViaHTTP {
		return json.Marshal(struct {
			common
			Forge     string          `json:"forge"`
			Host      string          `json:"host"`
			Method    string          `json:"method"`
			Path      string          `json:"path"`
			Operation string          `json:"operation,omitempty"`
			Variables json.RawMessage `json:"variables,omitempty"`
			Status    int             `json:"status"`
			Detail    string          `json:"detail,omitempty"`
		}{head, inv.Forge, inv.Host, inv.Method, inv.Path, inv.Operation, inv.Variables, inv.Status, inv.Detail})
	}
	return json.Marshal(struct {
		common
		CLI         string   `json:"cli"`
		Args        []string `json:"args"`
		Cwd         string   `json:"cwd"`
		Stdin       string   `json:"stdin,omitempty"`
		ExitCode    int      `json:"exitCode"`
		Stderr      string   `json:"stderr,omitempty"`
		StdoutBytes int      `json:"stdoutBytes"`
	}{head, inv.CLI, inv.Args, inv.Cwd, inv.Stdin, inv.ExitCode, inv.Stderr, inv.StdoutBytes})
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
	// the checkout (`gh pr create`, `glab mr create`). Defaults to
	// asking git.
	Origin func(cwd string) (string, error)
	// Head answers a working directory's current branch and commit, for
	// the create calls that open a pull or merge request from the
	// checkout. Defaults to asking git.
	Head func(cwd string) (CheckoutHead, error)
	// APIToken is the fixed token every request to the HTTP mounts must
	// carry (Authorization: Bearer or Private-Token). Empty refuses every
	// request.
	APIToken string
}

// Engine is the fake forge: seeded state, the route table's host and the
// invocation log. Safe for concurrent use; every call is answered under
// one lock, so concurrent CLI processes see a consistent fixture.
type Engine struct {
	opts Options

	mu       sync.Mutex
	ids      idSource
	viewer   string
	sshHosts map[string]string
	repos    map[string]*Repo
	// offline answers every call as a forge the network cannot reach,
	// the way gh and glab fail when DNS or the link is down.
	offline bool
	// rateLimits are the pools under a limit (SetRateLimit).
	rateLimits map[rateLimitKey]RateLimit
	log        []Invocation
	seq        int
	dropped    int
}

// New builds an empty engine. Every invocation against it answers "not
// found" until a fixture is seeded.
func New(opts Options) *Engine {
	if opts.Origin == nil {
		opts.Origin = gitOrigin
	}
	if opts.Head == nil {
		opts.Head = gitHead
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
	if e.sshHosts == nil {
		e.sshHosts = make(map[string]string)
	}
	for alias, host := range fixture.SSHHosts {
		e.sshHosts[alias] = host
	}
	for i := range fixture.Repos {
		repo := fixture.Repos[i]
		e.repos[repo.key()] = &repo
	}
	return fixture.clone(), nil
}

// Reset drops the seeded state, the rate limits and the invocation log.
// Ids keep counting
// so none repeats across tests.
func (e *Engine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.viewer = defaultViewer
	clear(e.repos)
	clear(e.sshHosts)
	e.offline = false
	clear(e.rateLimits)
	e.log = nil
	e.dropped = 0
}

// SetOffline makes the forge unreachable (or reachable again). While
// offline every gh and glab invocation exits 1 with the CLI's own
// connection failure on stderr and nothing on stdout, and every HTTP
// request is dropped without a reply, both recorded under route
// "offline"; ssh is answered as usual. Seeded state is kept, so a
// forge that comes back answers what it answered before.
func (e *Engine) SetOffline(offline bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.offline = offline
}

// offlineResponse is what a forge CLI prints when its host does not
// resolve: gh names the API host, glab echoes the failed request.
func offlineResponse(c *call) response {
	var stderr string
	switch c.cli {
	case "glab":
		stderr = "ERROR Get \"https://gitlab.com/api/v4/" + strings.Join(c.args, "/") + "\": dial tcp: lookup gitlab.com: i/o timeout\n"
	default:
		stderr = "error connecting to api.github.com\ncheck your internet connection or https://githubstatus.com\n"
	}
	return response{route: "offline", stderr: stderr, exit: 1}
}

// Invocations returns the recorded invocations with Seq > since.
func (e *Engine) Invocations(since int) InvocationLog {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := InvocationLog{Invocations: []Invocation{}, Dropped: e.dropped}
	for _, inv := range e.log {
		if inv.Seq > since {
			inv.Args = slices.Clone(inv.Args)
			inv.Variables = slices.Clone(inv.Variables)
			out.Invocations = append(out.Invocations, inv)
		}
	}
	return out
}

// Handle answers one forwarded invocation and records it.
func (e *Engine) Handle(fc control.ForgeCall) control.ForgeResult {
	c := &call{cli: fc.CLI, args: slices.Clone(fc.Args), cwd: fc.Cwd, stdin: fc.Stdin}

	e.mu.Lock()
	var resp response
	if e.offline && c.cli != "ssh" {
		resp = offlineResponse(c)
	} else {
		resp = e.dispatch(c)
	}
	if resp.unhandled != "" {
		resp.stdout = nil
		resp.stderr = fmt.Sprintf("ao-mockforge: unhandled %s invocation (%s): %s\n"+
			"Add a handler in internal/harness/forgefake (see its AGENTS.md).\n",
			c.cli, resp.unhandled, formatArgv(c.cli, c.args))
		resp.exit = 1
	}
	inv := Invocation{
		Via:         ViaCLI,
		CLI:         c.cli,
		Args:        slices.Clone(c.args),
		Cwd:         c.cwd,
		Stdin:       string(c.stdin),
		Route:       resp.route,
		Unhandled:   resp.unhandled != "",
		ExitCode:    resp.exit,
		Stderr:      resp.stderr,
		StdoutBytes: len(resp.stdout),
	}
	e.record(inv)
	return control.ForgeResult{Stdout: resp.stdout, Stderr: resp.stderr, ExitCode: resp.exit}
}

// record appends inv to the log and hands it to OnInvocation. Callers hold
// mu; record releases it.
func (e *Engine) record(inv Invocation) {
	e.seq++
	inv.Seq = e.seq
	inv.At = time.Now().UTC()
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
		inv.Variables = slices.Clone(inv.Variables)
		e.opts.OnInvocation(inv)
	}
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
		resp.route = strings.Join(append([]string{best.cli}, best.path...), " ")
	}
	return resp
}

// response is a handler's answer. A CLI call renders stdout, stderr and
// exit; an HTTP request renders status (0 means 200 for exit 0 and 500
// otherwise), header and stdout as the body.
type response struct {
	route     string
	stdout    []byte
	stderr    string
	exit      int
	status    int
	header    http.Header
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

// repoOn returns the seeded repository when it lives on host, the forge
// host an HTTP request addressed (its Host header, port included), which
// must match the seeded host exactly. A repository seeded on another host
// is as absent as an unknown one: the request asked a host that does not
// have it. Callers hold mu.
func (e *Engine) repoOn(forge, host, project string) *Repo {
	r := e.repo(forge, project)
	if r == nil || !strings.EqualFold(r.Host, host) {
		return nil
	}
	return r
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
