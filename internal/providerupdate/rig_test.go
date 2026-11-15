package providerupdate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

// hostRoutes is an httptest server standing in for every release host. The
// client's transport sends each request to it with the URL the service
// asked for, so routes are keyed by the production URL.
type hostRoutes struct {
	mu       sync.Mutex
	routes   map[string]func(w http.ResponseWriter, r *http.Request)
	requests []string
	agents   []string
	holds    map[string]hostHold
	server   *httptest.Server
}

const originalURLHeader = "X-Test-Original-Url"

func newHostRoutes(t *testing.T) *hostRoutes {
	t.Helper()
	h := &hostRoutes{routes: map[string]func(http.ResponseWriter, *http.Request){}}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		url := r.Header.Get(originalURLHeader)
		h.mu.Lock()
		h.requests = append(h.requests, url)
		h.agents = append(h.agents, r.Header.Get("User-Agent"))
		route := h.routes[url]
		h.mu.Unlock()
		if route == nil {
			http.Error(w, "no route for "+url, http.StatusNotFound)
			return
		}
		route(w, r)
	}))
	t.Cleanup(h.server.Close)
	return h
}

func (h *hostRoutes) text(url string, status int, body string) {
	h.set(url, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

func (h *hostRoutes) set(url string, route func(http.ResponseWriter, *http.Request)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.routes[url] = route
}

func (h *hostRoutes) requested() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.requests)
}

func (h *hostRoutes) count(url string) int {
	n := 0
	for _, got := range h.requested() {
		if got == url {
			n++
		}
	}
	return n
}

// hostHold is a request held in the client, deaf to its context, as a
// stalled connection would be.
type hostHold struct {
	reached chan struct{}
	release <-chan struct{}
}

// hold makes requests for url wait in the client until release runs, and
// signals reached as each arrives.
func (h *hostRoutes) hold(t *testing.T, url string) (reached <-chan struct{}, release func()) {
	gateCh, unblock := gate(t)
	held := hostHold{reached: make(chan struct{}, 8), release: gateCh}
	h.mu.Lock()
	if h.holds == nil {
		h.holds = map[string]hostHold{}
	}
	h.holds[url] = held
	h.mu.Unlock()
	return held.reached, unblock
}

func (h *hostRoutes) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		h.mu.Lock()
		held, isHeld := h.holds[req.URL.String()]
		h.mu.Unlock()
		if isHeld {
			held.reached <- struct{}{}
			<-held.release
		}
		out := req.Clone(req.Context())
		out.Header.Set(originalURLHeader, req.URL.String())
		out.URL.Scheme = "http"
		out.URL.Host = h.server.Listener.Addr().String()
		out.Host = ""
		return http.DefaultTransport.RoundTrip(out)
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// fakeRunner answers commands by "<binary> <first argument>", else by
// their first argument.
type fakeRunner struct {
	mu       sync.Mutex
	commands []Command
	answer   map[string]func(ctx context.Context) (Output, error)
}

func (f *fakeRunner) run(ctx context.Context, command Command) (Output, error) {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	answer := f.answer[command.Binary+" "+command.Args[0]]
	if answer == nil {
		answer = f.answer[command.Args[0]]
	}
	f.mu.Unlock()
	if answer == nil {
		return Output{}, errNoAnswer
	}
	return answer(ctx)
}

var errNoAnswer = errors.New("fake runner: no answer for this command")

func (f *fakeRunner) on(arg string, answer func(ctx context.Context) (Output, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer[arg] = answer
}

func (f *fakeRunner) ran(arg string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, command := range f.commands {
		if command.Args[0] == arg {
			n++
		}
	}
	return n
}

func (f *fakeRunner) lastOf(arg string) Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.commands) - 1; i >= 0; i-- {
		if f.commands[i].Args[0] == arg {
			return f.commands[i]
		}
	}
	return Command{}
}

// gate is a channel a fake command blocks on until unblock runs. A failed
// test unblocks it before the rig's Stop joins the command.
func gate(t *testing.T) (<-chan struct{}, func()) {
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	return release, unblock
}

func doctorSays(channel string) func(context.Context) (Output, error) {
	return func(context.Context) (Output, error) {
		return Output{Text: "Diagnostics\n  Running: native (2.1.300)\n  Auto-update channel: " + channel + "\n"}, nil
	}
}

// The Codex installs a test can give the rig.
var (
	npmCodex = Binary{
		Path:         "/home/u/.nvm/lib/node_modules/@openai/codex/vendor/x86_64-unknown-linux-musl/bin/codex",
		Launcher:     "/home/u/.nvm/lib/node_modules/@openai/codex/bin/codex.js",
		ResolvedPath: "/home/u/.nvm/lib/node_modules/@openai/codex/vendor/x86_64-unknown-linux-musl/bin/codex",
	}
	standaloneCodex = Binary{
		Path:         "codex",
		ResolvedPath: "/home/u/.codex/packages/standalone/releases/0.160.0-x86_64-unknown-linux-musl/codex",
	}
)

type rig struct {
	t      *testing.T
	svc    *Service
	hosts  *hostRoutes
	runner *fakeRunner
	ticks  chan time.Time

	mu        sync.Mutex
	installed map[string]string
	binaries  map[string]Binary
	// env replaces a provider's default {"PROXY_FOR": name}.
	env       map[string]map[string]string
	frames    [][]Status
	afterRuns int
	notify    chan struct{}
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{
		t:         t,
		hosts:     newHostRoutes(t),
		runner:    &fakeRunner{answer: map[string]func(context.Context) (Output, error){}},
		ticks:     make(chan time.Time),
		installed: map[string]string{ProviderClaude: "2.1.300", ProviderCodex: "0.160.0"},
		binaries: map[string]Binary{
			ProviderClaude: {Path: "/opt/claude", ResolvedPath: "/home/u/.local/share/claude/versions/2.1.300"},
			ProviderCodex:  npmCodex,
		},
		notify: make(chan struct{}, 1),
	}
	r.svc = New(Deps{
		Binary: func(name string) Binary {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.binaries[name]
		},
		Installed: r.installedVersion,
		Env: func(name string) map[string]string {
			r.mu.Lock()
			defer r.mu.Unlock()
			if env, ok := r.env[name]; ok {
				return env
			}
			return map[string]string{"PROXY_FOR": name}
		},
		WorkDir: func() (string, error) { return "/home/u", nil },
		Runner:  r.runner.run,
		AfterRun: func() {
			r.mu.Lock()
			r.afterRuns++
			r.mu.Unlock()
		},
		Client: r.hosts.client,
		Emit: func(statuses []Status) {
			r.mu.Lock()
			r.frames = append(r.frames, slices.Clone(statuses))
			r.mu.Unlock()
			select {
			case r.notify <- struct{}{}:
			default:
			}
		},
		Now:   func() time.Time { return time.UnixMilli(1_700_000_000_000) },
		Ticks: r.ticks,
	})
	t.Cleanup(r.svc.Stop)
	r.runner.on("doctor", doctorSays("latest"))
	r.hosts.text(ClaudeChannelURL("latest"), http.StatusOK, "2.1.325\n")
	r.hosts.text(ClaudeChannelURL("stable"), http.StatusOK, "2.1.311\n")
	r.hosts.text(CodexNPMLatestURL, http.StatusOK, `{"name":"@openai/codex","version":"0.169.0"}`)
	r.hosts.text(CodexLatestReleaseURL, http.StatusOK, `{"tag_name":"rust-v0.168.0","assets":[]}`)
	return r
}

func (r *rig) installedVersion(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.installed[name]
}

func (r *rig) setInstalled(name, version string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.installed[name] = version
}

func (r *rig) setBinary(name string, binary Binary) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.binaries[name] = binary
}

func (r *rig) allFrames() [][]Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.frames)
}

// mark is the count of frames emitted so far, for waitFrame.
func (r *rig) mark() int { return len(r.allFrames()) }

// waitFrame waits for a frame emitted after mark that satisfies want.
func (r *rig) waitFrame(mark int, want func([]Status) bool) []Status {
	r.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		for _, frame := range r.allFrames()[mark:] {
			if want(frame) {
				return frame
			}
		}
		select {
		case <-r.notify:
		case <-deadline:
			r.t.Fatalf("no frame matched; frames: %+v", r.allFrames())
		}
	}
}

func statusOf(statuses []Status, name string) Status {
	for _, status := range statuses {
		if status.Provider == name {
			return status
		}
	}
	return Status{}
}

func (r *rig) check() (claude, codex Status) {
	r.t.Helper()
	statuses := r.svc.Check()
	return statusOf(statuses, ProviderClaude), statusOf(statuses, ProviderCodex)
}

// waitRunSettled waits for a frame after mark in which the provider's run
// has left running.
func (r *rig) waitRunSettled(mark int, name string) Status {
	r.t.Helper()
	frame := r.waitFrame(mark, func(statuses []Status) bool {
		phase := statusOf(statuses, name).Run.Phase
		return phase == PhaseSucceeded || phase == PhaseFailed
	})
	return statusOf(frame, name)
}
