package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/harnessclient"
	"agent-overflow/internal/headlessshell"
	"agent-overflow/internal/procutil"
)

// The bridge behind `ui`, `perf`, `monitor` and `bench` lives in the SPA
// document, so those commands answer only while some page is open on the
// instance. Until this command existed the only ways to get one were a
// human opening the URL, `make harness-window`, or a Playwright spec —
// none of them available to an agent driving an instance from a shell.
//
// `attach` is deliberately its own verb rather than a flag on `open`.
// `open` answers "where is this instance" and returns; this one owns a
// child process for as long as the page is wanted. Folding a supervisor
// into a command whose contract is "print a line and exit" would make
// `open`'s exit code mean two different things.

const (
	// attachBrowserEnv names a Chromium-family binary explicitly. It is
	// the first link of the resolution chain so a machine with no cached
	// shell (CI, a fresh checkout) has a one-variable answer.
	attachBrowserEnv = "AO_HARNESS_BROWSER"

	// attachDefaultTimeout is the wall-clock budget for "browser spawned"
	// through "the bridge answered". A cold headless-shell start is under
	// two seconds locally; a minute is generous enough that a loaded CI
	// box is not flaky and short enough that a wedged launch is reported
	// while the operator is still watching.
	attachDefaultTimeout = 60 * time.Second

	// attachPollInterval is how often the page registration is re-read.
	// The probe is one small RPC, so this costs nothing next to the
	// browser's own startup.
	attachPollInterval = 250 * time.Millisecond

	// attachProbeTimeout bounds one readiness probe so a wedged backend
	// cannot consume the whole budget in a single call.
	attachProbeTimeout = 5 * time.Second
)

// systemBrowserNames is the last link of the resolution chain, in
// preference order: a headless shell first (smallest, no profile UI),
// then the full browsers, which need --headless=new.
var systemBrowserNames = []string{"chrome-headless-shell", "chromium", "chromium-browser", "google-chrome", "google-chrome-stable"}

// browserChoice is a resolved binary plus WHERE it came from. The source
// is printed because a page that renders differently than expected is
// almost always a different browser than expected.
type browserChoice struct {
	Path   string
	Source string
}

// headlessShell reports whether the binary is a chrome-headless-shell
// build, which is headless by construction and rejects nothing but also
// needs no --headless flag. Full Chrome/Chromium does need one.
func (c browserChoice) headlessShell() bool {
	name := strings.ToLower(filepath.Base(c.Path))
	return strings.HasPrefix(name, "chrome-headless-shell")
}

// browserResolver is the injectable half of the resolution chain: the
// environment lookup and PATH search, so a test can drive every link
// without a browser on the machine.
type browserResolver struct {
	getenv   func(string) string
	lookPath func(string) (string, error)
	// configRoot is the app-managed root whose headless-shell cache the
	// chain consults. Resolved by appdirs in production.
	configRoot func() (string, error)
}

func defaultBrowserResolver() browserResolver {
	return browserResolver{getenv: os.Getenv, lookPath: exec.LookPath, configRoot: appdirs.Root}
}

// resolve walks a three-link chain and says which link answered:
//
//  1. --browser, else $AO_HARNESS_BROWSER. An explicit path that is not
//     executable is an error, never a silent fall-through to link 2:
//     a caller who named a binary wants THAT binary.
//  2. a chrome-headless-shell already installed by the design-mode
//     screenshot installer, under the app-managed config root. This is
//     a read of one executable path, never a download — `ao-harness`
//     has no network story and must not grow one.
//  3. a Chromium-family browser on PATH.
func (r browserResolver) resolve(explicit string) (browserChoice, error) {
	if strings.TrimSpace(explicit) == "" {
		explicit = strings.TrimSpace(r.getenv(attachBrowserEnv))
	}
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		path := explicit
		if !filepath.IsAbs(path) {
			found, err := r.lookPath(path)
			if err != nil {
				return browserChoice{}, fmt.Errorf("browser %q: %w", explicit, err)
			}
			path = found
		}
		if !headlessshell.Executable(path) {
			return browserChoice{}, fmt.Errorf("browser %q is not an executable file", path)
		}
		return browserChoice{Path: path, Source: "explicit"}, nil
	}
	if root, err := r.configRoot(); err == nil {
		if path, version, ok := headlessshell.Installed(root); ok {
			return browserChoice{Path: path, Source: "cached chrome-headless-shell " + version}, nil
		}
	}
	for _, name := range systemBrowserNames {
		path, err := r.lookPath(name)
		if err != nil {
			continue
		}
		return browserChoice{Path: path, Source: "PATH"}, nil
	}
	return browserChoice{}, fmt.Errorf(
		"no headless browser found: set $%s to a Chromium-family binary (or pass --browser), "+
			"install one of %s on PATH, or let the app download chrome-headless-shell once "+
			"(design mode's read_screenshot does it)",
		attachBrowserEnv, strings.Join(systemBrowserNames, ", "))
}

// attachSpec is everything the argv builder needs. Split out so the
// assembly is a pure function a test can read.
type attachSpec struct {
	Browser      browserChoice
	URL          string
	ProfileDir   string
	WindowWidth  int
	WindowHeight int
	// DevToolsPort, when non-zero, also serves the Chromium DevTools
	// protocol, which is what `profile` and `bench --trace` need. Zero
	// opens no socket.
	DevToolsPort int
}

// browserArgs assembles the command line.
//
// The flag set is deliberately small. This page is what `perf` and
// `bench` measure, so every non-default rendering flag is a distortion
// of the numbers; only flags that make an unattended launch possible at
// all are here. In particular the memory-shaving flags
// `internal/screenshot` uses (--no-zygote, --in-process-gpu) are NOT
// used: a screenshot is a one-shot capture, this is a long-lived page
// under measurement, and --in-process-gpu turns a GPU fault into a whole
// browser death.
func browserArgs(spec attachSpec) []string {
	args := make([]string, 0, 10)
	if !spec.Browser.headlessShell() {
		// chrome-headless-shell is headless by construction and warns on
		// the flag; full Chrome needs it or it opens a window.
		args = append(args, "--headless=new")
	}
	args = append(args,
		// The SUID sandbox helper is unavailable in most WSL and
		// container environments, and the page is a loopback URL this
		// same operator just started.
		"--no-sandbox",
		// /dev/shm is tiny in containers; without this the renderer
		// dies on its first large allocation.
		"--disable-dev-shm-usage",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-gpu",
		"--user-data-dir="+spec.ProfileDir,
		fmt.Sprintf("--window-size=%d,%d", spec.WindowWidth, spec.WindowHeight),
	)
	if spec.DevToolsPort > 0 {
		args = append(args, "--remote-debugging-port="+strconv.Itoa(spec.DevToolsPort))
	}
	return append(args, spec.URL)
}

func runAttach(e *env, args []string) error {
	flags := e.newFlagSet("attach")
	detach := flags.Bool("detach", false, "leave the browser running in the background and print its pid instead of holding the terminal")
	timeout := flags.Duration("timeout", attachDefaultTimeout, "wall-clock budget for the page to load and the bridge to answer")
	browser := flags.String("browser", "", "path to a Chromium-family binary (default: $"+attachBrowserEnv+", then the cached chrome-headless-shell, then PATH)")
	width := flags.Int("width", 1600, "page width in CSS pixels")
	height := flags.Int("height", 1000, "page height in CSS pixels")
	devtools := flags.Int("devtools-port", 0, "also serve the Chromium DevTools protocol on this `port`, so profile and bench --trace can attach")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("attach takes no positional arguments (got %v)", rest)
	}
	if *timeout <= 0 {
		return usagef("--timeout must be positive")
	}
	if *width <= 0 || *height <= 0 {
		return usagef("--width and --height must be positive")
	}

	choice, err := defaultBrowserResolver().resolve(*browser)
	if err != nil {
		return err
	}

	// SIGINT/SIGTERM is the documented way to end a foreground attach, so
	// the notifier is installed before anything is spawned: a Ctrl-C
	// during the readiness wait must tear the browser down too.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	client, t, bs, err := e.attach(ctx)
	if err != nil {
		return err
	}
	defer client.Close()
	if strings.TrimSpace(bs.PageMarker) == "" {
		return fmt.Errorf("instance %s publishes no page marker; this build cannot prove which page attached", t.ID)
	}

	// Which pages are already registered decides what counts as OURS.
	// Without this an attach that spawns a browser which dies on the
	// spot still "succeeds" — against somebody else's window, or a
	// previous detached attach — because the page marker names the
	// BACKEND, not one document. Found live during verification.
	preexisting, err := registeredPageIDs(ctx, client)
	if err != nil {
		return fmt.Errorf("read the instance's attached pages: %w", err)
	}

	profileDir, err := os.MkdirTemp("", "ao-harness-page-"+t.ID+"-")
	if err != nil {
		return fmt.Errorf("create browser profile directory: %w", err)
	}
	logPath := filepath.Join(profileDir, "browser.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		_ = os.RemoveAll(profileDir)
		return fmt.Errorf("create browser log: %w", err)
	}

	spec := attachSpec{Browser: choice, URL: bs.URL, ProfileDir: profileDir, WindowWidth: *width, WindowHeight: *height, DevToolsPort: *devtools}
	// The foreground browser is a child of THIS context, so signal
	// delivery tears it down through the same group kill a failed
	// readiness wait uses. A detached one is deliberately tied to a
	// context that never ends; it still gets ConfigureGroup, so the
	// error paths below can reach its group through the one shared
	// primitive rather than a second kill of their own.
	runCtx := ctx
	if *detach {
		runCtx = context.Background()
	}
	cmd := exec.CommandContext(runCtx, choice.Path, browserArgs(spec)...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	procutil.ConfigureGroup(cmd)
	if *detach {
		applyAttachDetachAttrs(cmd)
	}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		_ = os.RemoveAll(profileDir)
		return fmt.Errorf("start %s: %w", choice.Path, err)
	}
	// The child holds its own descriptor for the log; ours is done.
	_ = logFile.Close()

	// exited is CLOSED rather than fed, so the readiness wait, the
	// teardown and the foreground hold can all observe it. waitErr is
	// written before the close and read only after it.
	var waitErr error
	exited := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(exited) }()

	teardown := func() {
		if err := procutil.KillConfiguredGroup(cmd); err != nil && !errors.Is(err, os.ErrProcessDone) {
			e.warnf("stop browser pid %d: %v", cmd.Process.Pid, err)
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
		if err := os.RemoveAll(profileDir); err != nil {
			e.warnf("remove browser profile %s: %v", profileDir, err)
		}
	}

	pageID, err := awaitAttachedPage(ctx, e, client, bs.PageMarker, preexisting, exited, &waitErr, *timeout)
	if err != nil {
		teardown()
		return fmt.Errorf("%w\n  browser: %s (%s)\n  browser log: %s", err, choice.Path, choice.Source, logPath)
	}

	if *detach {
		// Nothing else here owns the process now. The profile directory
		// stays with it: it is the browser's live state, and removing it
		// under a running Chromium corrupts the page.
		if e.jsonOutput() {
			return e.writeJSON(map[string]any{
				"pid": cmd.Process.Pid, "pageId": pageID, "detached": true,
				"browser": choice.Path, "browserSource": choice.Source,
				"url": bs.URL, "log": logPath, "profileDir": profileDir,
			})
		}
		e.printf("attached headless page %s to instance %s\n", pageID, t.ID)
		e.printf("  browser: %s (%s), pid %d\n", choice.Path, choice.Source, cmd.Process.Pid)
		e.printf("  log:     %s\n", logPath)
		e.printf("  stop it: kill %d && rm -rf %s\n", cmd.Process.Pid, profileDir)
		return nil
	}

	if e.jsonOutput() {
		if err := e.writeJSON(map[string]any{
			"pid": cmd.Process.Pid, "pageId": pageID, "detached": false,
			"browser": choice.Path, "browserSource": choice.Source,
			"url": bs.URL, "log": logPath, "profileDir": profileDir,
		}); err != nil {
			teardown()
			return err
		}
	} else {
		e.printf("attached headless page %s to instance %s\n", pageID, t.ID)
		e.printf("  browser: %s (%s), pid %d\n", choice.Path, choice.Source, cmd.Process.Pid)
		e.printf("  log:     %s\n", logPath)
		e.printf("  holding the page open; Ctrl-C (or SIGTERM) closes it\n")
	}

	select {
	case <-ctx.Done():
		teardown()
		return nil
	case <-exited:
		// The browser going away on its own is a failure: the caller
		// asked for a page and no longer has one.
		if err := os.RemoveAll(profileDir); err != nil {
			e.warnf("remove browser profile %s: %v", profileDir, err)
		}
		return fmt.Errorf("the headless browser exited while attached: %v (log: %s)", waitErr, logPath)
	}
}

// awaitAttachedPage waits for the spawned page to register with the
// backend and its bridge to answer a query. The budget is wall-clock,
// not a poll count, and running out is a FAILURE — the whole point of
// this command is that a caller can rely on the page being there when it
// returns.
func awaitAttachedPage(ctx context.Context, e *env, client *harnessclient.Client, marker string, preexisting map[string]bool, exited <-chan struct{}, waitErr *error, budget time.Duration) (string, error) {
	deadline := time.Now().Add(budget)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return "", fmt.Errorf("the headless browser exited before its page attached: %v", *waitErr)
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		if err := sleepCtx(ctx, attachPollInterval); err != nil {
			return "", err
		}
		infoCtx, cancel := context.WithTimeout(ctx, attachProbeTimeout)
		info, err := client.Info(infoCtx)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		pageID := attachedPageID(info.FrontendPages, marker, preexisting)
		if pageID == "" {
			lastErr = fmt.Errorf("no NEW frontend page with marker %q is registered yet", marker)
			continue
		}
		probeCtx, probeCancel := context.WithTimeout(ctx, attachProbeTimeout)
		_, err = e.queryUI(probeCtx, client, map[string]any{"kind": "element", "selector": "body", "pageId": pageID})
		probeCancel()
		if err != nil {
			lastErr = err
			continue
		}
		e.pageID = pageID
		return pageID, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no probe completed inside the budget")
	}
	return "", fmt.Errorf("the headless page did not answer the harness bridge within %s: %w", budget, lastErr)
}

// attachedPageID picks the page THIS attach put on the instance. Two
// filters, and both are load-bearing. Marker is the backend's own
// per-boot value, so a page carrying it cannot be a document left over
// from a previous instance on a recycled port. Preexisting excludes
// every page that was already registered when the browser was spawned,
// because the marker identifies the backend rather than one document:
// without it an attach whose browser died instantly reports success
// against the window somebody else already had open.
func attachedPageID(pages []harnessclient.HarnessPageIdentity, marker string, preexisting map[string]bool) string {
	for i := len(pages) - 1; i >= 0; i-- {
		page := pages[i]
		id := strings.TrimSpace(page.PageID)
		if page.Marker == marker && id != "" && !preexisting[id] {
			return id
		}
	}
	return ""
}

// registeredPageIDs is the set of documents attached right now.
func registeredPageIDs(ctx context.Context, client *harnessclient.Client) (map[string]bool, error) {
	infoCtx, cancel := context.WithTimeout(ctx, attachProbeTimeout)
	defer cancel()
	info, err := client.Info(infoCtx)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(info.FrontendPages))
	for _, page := range info.FrontendPages {
		if id := strings.TrimSpace(page.PageID); id != "" {
			ids[id] = true
		}
	}
	return ids, nil
}
