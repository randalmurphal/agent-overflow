package browser

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/platform"
)

// revealCommand is one fully-formed OS command that opens the system file
// manager with a file selected. Keeping the argv a value rather than an
// exec.Cmd is what lets the construction rules be unit-tested without
// running anything.
type revealCommand struct {
	Name string
	Args []string
}

// RevealPageFile opens the OS file manager with the local file a page is
// displaying selected, so it can be dragged into apps that accept file drops
// but not file paste (Teams refuses a pasted file object outright — verified
// live 2026-09-01 against both this app's clipboard copy and Explorer's own).
// Only file:// pages qualify: there is nothing to reveal for a remote URL.
func (m *Manager) RevealPageFile(ctx context.Context, access Access, pageID PageID) error {
	if err := m.bound(access); err != nil {
		return err
	}
	p, _, err := m.lookupOrSelectPage(ctx, access, pageID)
	if err != nil {
		return err
	}
	pageURL := p.cachedInfo().URL
	path, err := localFilePathForPageURL(pageURL)
	if err != nil {
		return err
	}
	if engine, ok := m.engine.(engineFileURL); ok {
		// The page's address is in the RENDERER's form (file:///C:/...,
		// file://wsl.localhost/...); resolve the backend path behind it
		// before touching the filesystem.
		if path, err = engine.BackendFilePath(ctx, pageURL); err != nil {
			return err
		}
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("browser: resolve %s to reveal: %w", path, err)
	}
	return m.revealFile(ctx, resolved)
}

// errNoFileManager refuses a reveal in a boot that shows nothing on the
// user's desktop.
var errNoFileManager = errors.New("browser: this run does not open the file manager")

// revealFile hands path to the file manager this Manager was built with.
func (m *Manager) revealFile(ctx context.Context, path string) error {
	if m.revealFileInFileManager == nil {
		return errNoFileManager
	}
	return m.revealFileInFileManager(ctx, path)
}

// revealsFiles answers whether a boot may start the OS file manager: never
// one on the fake engine, which is an isolated boot (the harness), and never
// a test binary. Either would open a window on the desktop of whoever runs
// it.
func revealsFiles(fakeEngine, underTest bool) bool {
	return !fakeEngine && !underTest
}

// fileRevealer is the hand-off a Manager is built with: the OS file manager
// where revealsFiles allows it, else a refusal.
func fileRevealer(fakeEngine, underTest bool) func(context.Context, string) error {
	if !revealsFiles(fakeEngine, underTest) {
		return func(context.Context, string) error { return errNoFileManager }
	}
	return revealFileInFileManager
}

// localFilePathForPageURL turns a page's current address into the local path it
// is rendering, and rejects everything that is not a local file.
func localFilePathForPageURL(rawURL string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", fmt.Errorf("browser: page has no address")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || !strings.EqualFold(parsed.Scheme, "file") {
		return "", fmt.Errorf("browser: only local files can be shown in the file manager, this page is at %s", trimmed)
	}
	if parsed.Path == "" {
		return "", fmt.Errorf("browser: page address %s names no file", trimmed)
	}
	return filepath.FromSlash(parsed.Path), nil
}

// revealFileInFileManager is the production hand-off. Every branch ends in one
// bounded subprocess. A Manager reaches it only through fileRevealer, and it
// refuses in a test binary itself, so no test can start a file manager.
func revealFileInFileManager(ctx context.Context, path string) error {
	if testing.Testing() {
		return errNoFileManager
	}
	switch {
	case platform.IsWSL():
		// Explorer opens \\wsl.localhost paths directly, so the ORIGINAL
		// file is revealed — no staging copy that could go stale.
		windowsPath, err := windowsPathFor(ctx, path)
		if err != nil {
			return err
		}
		command, err := windowsRevealCommand(windowsPath)
		if err != nil {
			return err
		}
		return runRevealCommand(ctx, command, true)
	case runtime.GOOS == "darwin":
		return runRevealCommand(ctx, revealCommand{Name: "open", Args: []string{"-R", path}}, false)
	default:
		return runLinuxRevealCommands(ctx, path)
	}
}

// windowsRevealCommand names the file for Explorer's select verb. The comma
// spelling is Explorer's own argument grammar; the path needs no quoting
// because it crosses as one argv entry, never through a shell.
func windowsRevealCommand(windowsPath string) (revealCommand, error) {
	if strings.TrimSpace(windowsPath) == "" {
		return revealCommand{}, fmt.Errorf("browser: empty Windows path to reveal")
	}
	return revealCommand{Name: "explorer.exe", Args: []string{"/select," + windowsPath}}, nil
}

// linuxRevealCommands is the ordered candidate list for a native Linux
// desktop: the freedesktop file-manager interface selects the file itself,
// and a plain open of the parent directory is the fallback every desktop has.
func linuxRevealCommands(path string) []revealCommand {
	uri := (&url.URL{Scheme: "file", Path: path}).String()
	return []revealCommand{
		{Name: "dbus-send", Args: []string{
			"--session", "--print-reply", "--dest=org.freedesktop.FileManager1",
			"/org/freedesktop/FileManager1", "org.freedesktop.FileManager1.ShowItems",
			"array:string:" + uri, "string:",
		}},
		{Name: "xdg-open", Args: []string{filepath.Dir(path)}},
	}
}

func runLinuxRevealCommands(ctx context.Context, path string) error {
	var lastErr error
	for _, command := range linuxRevealCommands(path) {
		if _, err := exec.LookPath(command.Name); err != nil {
			lastErr = err
			continue
		}
		if err := runRevealCommand(ctx, command, false); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no file manager found")
	}
	return fmt.Errorf("browser: show in file manager needs a org.freedesktop.FileManager1 service or xdg-open: %w", lastErr)
}

// runRevealCommand runs one bounded reveal subprocess. ignoreExitStatus is
// for explorer.exe, which reports exit code 1 on success by long-standing
// Windows behavior — only a failure to launch at all is an error there.
func runRevealCommand(ctx context.Context, command revealCommand, ignoreExitStatus bool) error {
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, command.Name, command.Args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if ignoreExitStatus && errors.As(err, &exitErr) && runCtx.Err() == nil {
			return nil
		}
		return fmt.Errorf("browser: %s reveal failed: %w: %s", command.Name, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func runWSLPath(ctx context.Context, args ...string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(runCtx, "wslpath", args...).Output()
	if err != nil {
		return "", fmt.Errorf("wslpath: %w", err)
	}
	converted := strings.TrimSpace(string(output))
	if converted == "" {
		return "", fmt.Errorf("wslpath returned nothing")
	}
	return converted, nil
}

func windowsPathFor(ctx context.Context, linuxPath string) (string, error) {
	converted, err := runWSLPath(ctx, "-w", linuxPath)
	if err != nil {
		return "", fmt.Errorf("browser: convert %s to a Windows path: %w", linuxPath, err)
	}
	return converted, nil
}
