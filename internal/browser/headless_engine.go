package browser

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// The headless Chromium engine: the SERVE-mode implementation of the
// driver.go seam (docs/specs/remote-access.md §7, "Headless Chromium
// engine (serve mode)").
//
// A page's OPERATIONS are plain CDP — cdp_page.go drives a headless
// Chromium target exactly as it drives a launcher-hosted WebView2
// controller, and that file is reused whole. What differs is LIFETIME:
//
//   - a Chromium process belongs to a PROFILE, not to the engine. One
//     workspace is one process with its own --user-data-dir, which is what
//     makes two workspaces' logins genuinely separate on a deployment with
//     no window and no per-view network session to isolate them with;
//   - a process is launched by the profile's FIRST page and dies with the
//     profile. The Manager already disposes a profile when its last page
//     closes and stops the engine two minutes after the last profile goes
//     (idleBrowserDelay), so an idle serve host runs no browser at all;
//   - there is no window, so there is no pane. paneHost, paneDevTools,
//     engineSiteData, engineUIThread and engineAccelerators are all
//     deliberately unimplemented: omission is refusal on this seam, and
//     the Manager's own assertion failures are the true answers here.
//     Site data is under the Manager's own browser-profiles/ tree, so the
//     tree delete already IS the clear.
//
// Policy stays in the Manager, as everywhere else on this seam.

const (
	// headlessLaunchTimeout bounds one profile's launch end to end:
	// spawning the binary, waiting for its "DevTools listening on" line,
	// and the browser-level handshake. A cold Chromium on a loaded serve
	// host is seconds; past this the process is wedged and the tool call
	// must fail rather than hang.
	headlessLaunchTimeout = 45 * time.Second
	// headlessOutputTail bounds how much of a failed launch's output an
	// error carries. The diagnosis is the last lines — the sandbox
	// refusal, the missing shared library — and the process chose how much
	// noise came before them, so it must not also choose how much memory
	// the error costs.
	headlessOutputTail = 4 << 10
)

// HeadlessChromiumOptions selects this engine. It is an explicit POSITIVE
// choice, set only by the serve boot: the absence of a window must never
// select a browser, or `go test` and every `--connect` backend would start
// launching one (manager_test.go pins that rule).
type HeadlessChromiumOptions struct {
	// Binary is the operator's browserChromiumPath, empty to search PATH.
	// It is the whole of what this engine takes from settings; everything
	// else about a profile is the Manager's own profileOptions.
	Binary string
}

type headlessEngine struct {
	configDir string
	// binary is resolved ONCE, at selection: a boot that found no browser
	// has no engine at all, so every profile below shares one answer.
	binary string
	events engineEvents
	logf   func(string, ...any)

	// tempRoot is where an ephemeral profile's directory is created and
	// the only directory Start sweeps. Empty in a unit test's engine,
	// which is what keeps a `go test` run off the machine's real temp
	// directory (headless_ephemeral.go).
	tempRoot string

	mu       sync.Mutex
	started  bool
	profiles map[*headlessProfile]struct{}
	// pageProfile is the only engine-wide page bookkeeping: DiscardPage
	// and the popup path address a page by handle with no profile in hand,
	// and a target this engine never bound must not be closed as one.
	pageProfile map[string]*headlessProfile
}

// newHeadlessChromiumEngine resolves the browser or explains why it could
// not. The error is what selectEngine logs before falling back to no
// engine at all; nothing here starts a process.
func newHeadlessChromiumEngine(configDir string, opts HeadlessChromiumOptions, events engineEvents) (*headlessEngine, error) {
	binary, err := findChromium(opts.Binary, exec.LookPath, runtime.GOOS)
	if err != nil {
		return nil, err
	}
	return &headlessEngine{
		configDir:   configDir,
		tempRoot:    os.TempDir(),
		binary:      binary,
		events:      events,
		logf:        log.Printf,
		profiles:    make(map[*headlessProfile]struct{}),
		pageProfile: make(map[string]*headlessProfile),
	}, nil
}

// Start validates the binary and does nothing else, which is the design
// rather than a gap: a process belongs to a profile here, and the first
// page of the first profile is what launches one. Starting a browser now
// would run one on every serve host whose agent never opens a page.
func (e *headlessEngine) Start(context.Context) error {
	if err := validateChromiumBinary(e.binary, exec.LookPath); err != nil {
		return fmt.Errorf("browser: %w", err)
	}
	e.mu.Lock()
	e.started = true
	e.mu.Unlock()
	// A backend that was killed or lost power ran no Dispose, so its
	// ephemeral profile, a full Chromium cookie jar, is still on disk.
	// Start is where it is reclaimed, and the owner marker is what keeps
	// the sweep off a directory another live backend is using.
	sweepEphemeralRoots(e.tempRoot, ownerAlive, e.logf)
	return nil
}

func (e *headlessEngine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.started
}

// Interrupt releases a launch that is waiting on a browser which may never
// print its endpoint, so a concurrent shutdown can proceed.
func (e *headlessEngine) Interrupt() {
	for _, p := range e.liveProfiles() {
		p.interrupt()
	}
}

// Stop disposes every profile, which is what kills every process: this
// engine holds no process of its own.
//
// One profile's refusal does not stop the others, or a shutdown that gave up
// halfway would leave the rest of the browsers running. It is not swallowed
// either: a Dispose that fails means a Chromium may still be alive holding a
// user-data directory this backend believes it released, and the profile
// handle is what names both.
func (e *headlessEngine) Stop() {
	e.mu.Lock()
	e.started = false
	e.mu.Unlock()
	for _, p := range e.liveProfiles() {
		if err := p.Dispose(context.Background()); err != nil {
			e.logf("browser: profile %s did not stop cleanly: %v", p.Handle(), err)
		}
	}
}

func (e *headlessEngine) liveProfiles() []*headlessProfile {
	e.mu.Lock()
	defer e.mu.Unlock()
	live := make([]*headlessProfile, 0, len(e.profiles))
	for p := range e.profiles {
		live = append(live, p)
	}
	return live
}

// NewProfile lays one workspace's Chromium user-data directory out; the
// process itself waits for the first page.
//
// The workspace DIGEST names the directory, not its path: a workspace root
// can be long, can hold characters a directory name cannot, and must not be
// readable from a listing of the profile tree. That is the same rule and
// the same tree (browserProfileDir) the WebKitGTK engine uses, which is
// what makes Settings → Clear site data a real clear here — deleting the
// tree deletes this engine's cookies too, so it implements no engineSiteData
// of its own.
func (e *headlessEngine) NewProfile(_ context.Context, opts profileOptions) (engineProfile, error) {
	if !e.Running() {
		return nil, errors.New("browser: engine unavailable")
	}
	if strings.TrimSpace(opts.Workspace) == "" {
		return nil, errors.New("browser: workspace is required for a browser profile")
	}
	digest := sha256.Sum256([]byte(opts.Workspace))
	p := &headlessProfile{engine: e, handle: fmt.Sprintf("%x", digest[:12]), downloadDir: opts.DownloadDir}
	if opts.Persist {
		p.userDataDir = filepath.Join(e.configDir, browserProfileDir, p.handle, "chromium")
	} else {
		// An ephemeral session is a real Chromium profile on a directory
		// nothing outlives (spec §4: browserPersistSiteData=false is an
		// ephemeral session, not a suppressed write). Chromium has no
		// in-memory profile mode, so the directory IS the promise, and
		// Dispose removing it is what keeps it.
		root, err := os.MkdirTemp(e.tempRoot, ephemeralDirPrefix)
		if err != nil {
			return nil, fmt.Errorf("browser: create ephemeral site data directory: %w", err)
		}
		p.ephemeralRoot = root
		// Stamped before anything is written into it, so a crash between
		// here and the first page still leaves a root a later start can
		// attribute and reclaim.
		if err := writeEphemeralOwner(root); err != nil {
			e.logf("browser: mark the ephemeral profile root %s: %v", root, err)
		}
		p.userDataDir = filepath.Join(root, "chromium")
	}
	if err := os.MkdirAll(p.userDataDir, 0o700); err != nil {
		p.removeEphemeralRoot()
		return nil, fmt.Errorf("browser: create site data directory: %w", err)
	}
	e.mu.Lock()
	e.profiles[p] = struct{}{}
	e.mu.Unlock()
	return p, nil
}

// DiscardPage closes a page the Manager declined to adopt — always a popup
// Chromium opened by itself, since every other page is one the Manager
// asked for.
func (e *headlessEngine) DiscardPage(handle string) {
	if p, ok := e.profileForPage(handle); ok {
		p.closeTarget(handle)
	}
}

// bindPage records which profile a page belongs to. Reported by the
// profile's own event stream and by NewPage, so a target that arrived from
// nowhere is never addressable.
func (e *headlessEngine) bindPage(handle string, p *headlessProfile) {
	e.mu.Lock()
	e.pageProfile[handle] = p
	e.mu.Unlock()
}

// unbindPage drops a page and reports whether this engine had it, which is
// what stops a destroyed target nobody owns from being reported as a page
// the Manager lost.
func (e *headlessEngine) unbindPage(handle string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, known := e.pageProfile[handle]
	delete(e.pageProfile, handle)
	return known
}

func (e *headlessEngine) profileForPage(handle string) (*headlessProfile, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.pageProfile[handle]
	return p, ok
}

// pagesOf TAKES every page bound to a profile, unbinding each as it goes.
// Taking rather than reading, because the caller is about to report them
// closed and a second caller must not report the same page a second time.
func (e *headlessEngine) pagesOf(p *headlessProfile) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var handles []string
	for handle, owner := range e.pageProfile {
		if owner == p {
			handles = append(handles, handle)
			delete(e.pageProfile, handle)
		}
	}
	// Sorted, so a profile that lost several pages reports them in one
	// order rather than in map order.
	sort.Strings(handles)
	return handles
}

func (e *headlessEngine) forgetProfile(p *headlessProfile) {
	e.mu.Lock()
	delete(e.profiles, p)
	for handle, owner := range e.pageProfile {
		if owner == p {
			delete(e.pageProfile, handle)
		}
	}
	e.mu.Unlock()
}

// chromiumFlag is one command-line flag in chromedp's own vocabulary: a
// string value becomes --name=value, true becomes --name, and FALSE is a
// flag deliberately WITHHELD — which is not the same as absent, and the
// difference is the sandbox (see chromiumLaunchFlags).
type chromiumFlag struct {
	name  string
	value any
}

// chromiumLaunchFlags is the entire command line this engine builds, kept
// pure so a test can read it without a browser on the machine.
//
// chromedp's DefaultExecAllocatorOptions are deliberately NOT used: they
// are two dozen flags tuned for scraping, and a serve host's browser
// should be exactly what this list says and nothing else. chromedp still
// appends --remote-debugging-port=0 and an about:blank argument of its own.
func chromiumLaunchFlags(userDataDir string) []chromiumFlag {
	return []chromiumFlag{
		// The modern headless mode: a real Chromium with no window, rather
		// than the old separate headless shell that shipped different
		// behavior from the browser users actually run.
		{"headless", "new"},
		// One process per profile is one user-data directory per profile,
		// and that directory is the whole of a workspace's isolation here.
		{"user-data-dir", userDataDir},
		// No compositor on a serve host, and a GPU process that cannot
		// reach a display is a crash loop rather than an optimisation.
		{"disable-gpu", true},
		{"no-first-run", true},
		{"no-default-browser-check", true},
		// NEVER --no-sandbox, and PRESENT-AND-FALSE rather than absent:
		// chromedp adds --no-sandbox by itself when the process runs as
		// root unless the flag is already in its map (allocate.go). So an
		// absent flag would silently drop the renderer sandbox on exactly
		// the deployment most likely to be misconfigured — a serve backend
		// started by a root service unit. docs/architecture/serve-mode.md
		// says to run the service as a non-root user; this line is what
		// makes that instruction load-bearing instead of advisory, because
		// a root install now FAILS to launch and says so.
		{"no-sandbox", false},
	}
}

func (e *headlessEngine) execOptions(userDataDir string) []chromedp.ExecAllocatorOption {
	flags := chromiumLaunchFlags(userDataDir)
	opts := make([]chromedp.ExecAllocatorOption, 0, len(flags)+2)
	opts = append(opts, chromedp.ExecPath(e.binary), chromedp.WSURLReadTimeout(headlessLaunchTimeout))
	for _, flag := range flags {
		opts = append(opts, chromedp.Flag(flag.name, flag.value))
	}
	return opts
}
