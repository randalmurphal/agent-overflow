package externalurl

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"agent-overflow/internal/appimage"
	"agent-overflow/internal/platform"
)

// ErrNoOpener reports that this host has no program able to hand a URL to a
// browser: every platform candidate was absent from PATH, or the platform has
// no candidate at all. It is a fact about the MACHINE rather than about the
// URL, which is why it is a sentinel and not prose — a caller with somewhere
// else to put the link (the provider login flow, which can show it to a remote
// device instead) branches on errors.Is and keeps its flow alive, while every
// caller that only wanted a browser opened still gets an error with the same
// remedy in it.
//
// A candidate that WAS on PATH and failed to start is deliberately not this:
// an opener exists, it just did not work, and retrying or fixing the desktop
// session is the answer there.
var ErrNoOpener = errors.New(
	"no browser opener is installed on this host; open the link on the device you are reading this on",
)

// Command describes the platform command used to hand a URL to the OS.
type Command struct {
	Name string
	Args []string
}

// Open validates rawURL and opens it in the system browser for the host
// environment. WSL is special: the visible desktop is Windows, so route
// through Windows interop instead of Linux xdg-open / WSLg.
func Open(ctx context.Context, rawURL string) error {
	safeURL, err := Validate(rawURL)
	if err != nil {
		return err
	}
	return open(ctx, commandCandidates(runtime.GOOS, platform.IsWSL(), safeURL), exec.LookPath, startCommand)
}

// Validate accepts only absolute HTTP(S) URLs. The UI also validates before
// calling this binding, but the RPC boundary gets no trust.
func Validate(rawURL string) (string, error) {
	value := strings.TrimSpace(rawURL)
	if value == "" {
		return "", errors.New("external URL is required")
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return "", errors.New("invalid external URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("external URL must use http or https")
	}
	if parsed.Host == "" {
		return "", errors.New("external URL must include a host")
	}
	return parsed.String(), nil
}

type commandStarter func(context.Context, Command) error
type commandLookup func(string) (string, error)

// open walks the platform's opener candidates in order. The two failure
// shapes are kept apart on purpose: nothing was INSTALLED (ErrNoOpener, a
// host property a caller can degrade around) versus an installed opener
// REFUSED to start (an ordinary failure). Lookup failures are still reported
// alongside a start failure, because which candidates were missing is what
// explains why the one that ran was the one that ran.
func open(ctx context.Context, candidates []Command, lookup commandLookup, start commandStarter) error {
	var lookupErrs, startErrs []error
	for _, candidate := range candidates {
		if candidate.Name == "" {
			continue
		}
		if _, err := lookup(candidate.Name); err != nil {
			lookupErrs = append(lookupErrs, fmt.Errorf("%s: %w", candidate.Name, err))
			continue
		}
		if err := start(ctx, candidate); err != nil {
			startErrs = append(startErrs, fmt.Errorf("%s: %w", candidate.Name, err))
			continue
		}
		return nil
	}

	if len(startErrs) == 0 {
		if len(lookupErrs) == 0 {
			return fmt.Errorf("%w (%s names no opener command)", ErrNoOpener, runtime.GOOS)
		}
		return fmt.Errorf("%w (%w)", ErrNoOpener, errors.Join(lookupErrs...))
	}
	return fmt.Errorf("open external URL: %w", errors.Join(append(lookupErrs, startErrs...)...))
}

func commandCandidates(goos string, isWSL bool, safeURL string) []Command {
	if goos == "windows" || isWSL {
		return []Command{{
			Name: "rundll32.exe",
			Args: []string{"url.dll,FileProtocolHandler", safeURL},
		}}
	}

	switch goos {
	case "darwin":
		return []Command{{Name: "open", Args: []string{safeURL}}}
	case "linux":
		return []Command{
			{Name: "xdg-open", Args: []string{safeURL}},
			{Name: "x-www-browser", Args: []string{safeURL}},
			{Name: "www-browser", Args: []string{safeURL}},
		}
	default:
		return nil
	}
}

func startCommand(ctx context.Context, command Command) error {
	cmd := exec.CommandContext(ctx, command.Name, command.Args...)
	// The opener — and the browser it hands the URL to — inherits our
	// environment, minus the AppImage launch artifacts. A browser started
	// with the mount's LD_LIBRARY_PATH / XDG_DATA_DIRS resolves libraries and
	// .desktop entries against a squashfs that disappears when Agent Overflow
	// exits, and it outlives us by design. nil on every other launch shape,
	// which keeps exec.Cmd on its own inherit path.
	cmd.Env = appimage.ScrubInherited()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open dev null: %w", err)
	}
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	applyDetachAttrs(cmd)
	if err := cmd.Start(); err != nil {
		_ = devNull.Close()
		return err
	}
	_ = devNull.Close()
	go func() { _ = cmd.Wait() }()
	return nil
}
