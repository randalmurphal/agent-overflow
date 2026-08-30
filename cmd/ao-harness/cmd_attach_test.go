package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agent-overflow/internal/harnessclient"
	"agent-overflow/internal/headlessshell"
)

// fakeBrowser writes an executable file so the resolver's own
// executable check passes. Nothing in these tests ever RUNS it: a unit
// test must never spawn a browser any more than it may spawn a provider.
func fakeBrowser(t *testing.T, dir, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func testResolver(env map[string]string, path map[string]string, configRoot string) browserResolver {
	return browserResolver{
		getenv: func(key string) string { return env[key] },
		lookPath: func(name string) (string, error) {
			if found, ok := path[name]; ok {
				return found, nil
			}
			return "", errors.New("not found in $PATH")
		},
		configRoot: func() (string, error) { return configRoot, nil },
	}
}

func TestBrowserResolutionPrefersTheExplicitFlag(t *testing.T) {
	dir := t.TempDir()
	explicit := fakeBrowser(t, dir, "my-chrome")
	cached := filepath.Join(dir, "cache")
	installFakeShell(t, cached, "150.0.1.2")

	choice, err := testResolver(nil, nil, cached).resolve(explicit)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if choice.Path != explicit || choice.Source != "explicit" {
		t.Fatalf("resolve = %+v, want the explicit path", choice)
	}
}

func TestBrowserResolutionReadsTheEnvOverrideBeforeTheCache(t *testing.T) {
	dir := t.TempDir()
	fromEnv := fakeBrowser(t, dir, "env-chrome")
	cached := filepath.Join(dir, "cache")
	installFakeShell(t, cached, "150.0.1.2")

	choice, err := testResolver(map[string]string{attachBrowserEnv: fromEnv}, nil, cached).resolve("")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if choice.Path != fromEnv {
		t.Fatalf("resolve = %q, want the $%s binary", choice.Path, attachBrowserEnv)
	}
}

// An explicitly named binary that does not exist must FAIL rather than
// quietly resolving to some other browser: a caller who typed a path is
// asserting which engine renders the page they are about to measure.
func TestBrowserResolutionRefusesAMissingExplicitBinary(t *testing.T) {
	dir := t.TempDir()
	cached := filepath.Join(dir, "cache")
	installFakeShell(t, cached, "150.0.1.2")
	onPath := map[string]string{"chromium": fakeBrowser(t, dir, "chromium")}

	_, err := testResolver(nil, onPath, cached).resolve(filepath.Join(dir, "absent"))
	if err == nil {
		t.Fatal("resolve accepted a nonexistent --browser path")
	}
}

func TestBrowserResolutionFallsBackToTheCachedShellThenPath(t *testing.T) {
	dir := t.TempDir()
	cached := filepath.Join(dir, "cache")
	shell := installFakeShell(t, cached, "150.0.1.2")
	onPath := map[string]string{"chromium": fakeBrowser(t, dir, "chromium")}

	choice, err := testResolver(nil, onPath, cached).resolve("")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if choice.Path != shell {
		t.Fatalf("resolve = %q, want the cached shell %q", choice.Path, shell)
	}
	if !strings.Contains(choice.Source, "150.0.1.2") {
		t.Fatalf("source %q does not name the cached version", choice.Source)
	}

	empty := filepath.Join(dir, "empty-cache")
	choice, err = testResolver(nil, onPath, empty).resolve("")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if choice.Path != onPath["chromium"] || choice.Source != "PATH" {
		t.Fatalf("resolve = %+v, want the PATH chromium", choice)
	}
}

func TestBrowserResolutionFailsLoudlyWithNoBrowserAnywhere(t *testing.T) {
	dir := t.TempDir()
	_, err := testResolver(nil, nil, filepath.Join(dir, "empty")).resolve("")
	if err == nil {
		t.Fatal("resolve succeeded with no browser available")
	}
	if !strings.Contains(err.Error(), attachBrowserEnv) {
		t.Fatalf("error %q does not name the $%s escape hatch", err, attachBrowserEnv)
	}
}

// installFakeShell lays out one version under a cache root the way the
// screenshot installer does, and returns the binary path.
func installFakeShell(t *testing.T, configRoot, version string) string {
	t.Helper()
	platform, err := headlessshell.Platform()
	if err != nil {
		t.Skipf("no Chrome-for-Testing platform here: %v", err)
	}
	path := headlessshell.BinaryPath(filepath.Join(configRoot, headlessshell.CacheDirName, version), platform)
	return fakeBrowser(t, filepath.Dir(path), strings.TrimSuffix(filepath.Base(path), ".exe"))
}

func TestBrowserArgsOmitHeadlessFlagForTheHeadlessShell(t *testing.T) {
	spec := attachSpec{
		Browser:      browserChoice{Path: "/x/chrome-headless-shell"},
		URL:          "http://127.0.0.1:1/?token=t",
		ProfileDir:   "/tmp/p",
		WindowWidth:  1600,
		WindowHeight: 1000,
	}
	args := browserArgs(spec)
	for _, arg := range args {
		if strings.HasPrefix(arg, "--headless") {
			t.Fatalf("headless shell was given %q", arg)
		}
	}
	if args[len(args)-1] != spec.URL {
		t.Fatalf("URL is not the last argument: %v", args)
	}
	if !hasArg(args, "--user-data-dir=/tmp/p") || !hasArg(args, "--window-size=1600,1000") {
		t.Fatalf("args missing profile or window size: %v", args)
	}
	if hasArgPrefix(args, "--remote-debugging-port") {
		t.Fatalf("a devtools port was opened without being asked for: %v", args)
	}
}

func TestBrowserArgsAddHeadlessFlagForFullChrome(t *testing.T) {
	args := browserArgs(attachSpec{
		Browser:      browserChoice{Path: "/usr/bin/google-chrome"},
		URL:          "http://127.0.0.1:1/",
		ProfileDir:   "/tmp/p",
		WindowWidth:  800,
		WindowHeight: 600,
		DevToolsPort: 9333,
	})
	if !hasArg(args, "--headless=new") {
		t.Fatalf("full chrome was not put in headless mode: %v", args)
	}
	if !hasArg(args, "--remote-debugging-port=9333") {
		t.Fatalf("--devtools-port did not reach the browser: %v", args)
	}
}

// The measured surface must not be reshaped by convenience flags. The
// screenshot package's memory-shaving options are deliberately absent.
func TestBrowserArgsCarryNoRenderingDistortions(t *testing.T) {
	args := browserArgs(attachSpec{Browser: browserChoice{Path: "/x/chrome-headless-shell"}, URL: "u", ProfileDir: "p", WindowWidth: 10, WindowHeight: 10})
	for _, banned := range []string{"--no-zygote", "--in-process-gpu", "--disable-frame-rate-limit"} {
		if hasArg(args, banned) {
			t.Fatalf("%s distorts what perf/bench measure: %v", banned, args)
		}
	}
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func hasArgPrefix(args []string, prefix string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	return false
}

func TestAttachedPageIDMatchesTheBackendMarkerOnly(t *testing.T) {
	pages := []harnessclient.HarnessPageIdentity{
		{PageID: "old", Marker: "other"},
		{PageID: "", Marker: "mine"},
		{PageID: "ours", Marker: "mine"},
	}
	if got := attachedPageID(pages, "mine", nil); got != "ours" {
		t.Fatalf("attachedPageID = %q, want ours", got)
	}
	if got := attachedPageID(pages, "absent", nil); got != "" {
		t.Fatalf("attachedPageID = %q, want empty for an unknown marker", got)
	}
}

// A page that was already open when the browser was spawned is not
// evidence that OUR browser attached. Without this filter an attach
// whose browser died on the spot reported success against somebody
// else's window (found during live verification, 2026-08-30).
func TestAttachedPageIDIgnoresPagesThatWereAlreadyOpen(t *testing.T) {
	pages := []harnessclient.HarnessPageIdentity{
		{PageID: "somebody-elses-window", Marker: "mine"},
	}
	preexisting := map[string]bool{"somebody-elses-window": true}
	if got := attachedPageID(pages, "mine", preexisting); got != "" {
		t.Fatalf("attachedPageID = %q, want empty: that page predates this attach", got)
	}
	pages = append(pages, harnessclient.HarnessPageIdentity{PageID: "fresh", Marker: "mine"})
	if got := attachedPageID(pages, "mine", preexisting); got != "fresh" {
		t.Fatalf("attachedPageID = %q, want the page this attach opened", got)
	}
}

func TestAttachRefusesPositionalArguments(t *testing.T) {
	code, _, stderr := run(t, "attach", "http://example.test")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d (%s)", code, exitUsage, stderr)
	}
	if !strings.Contains(stderr, "no positional arguments") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestAttachRefusesANonPositiveTimeout(t *testing.T) {
	code, _, stderr := run(t, "attach", "--timeout", "0")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d (%s)", code, exitUsage, stderr)
	}
	if !strings.Contains(stderr, "--timeout") {
		t.Fatalf("stderr = %q", stderr)
	}
}
