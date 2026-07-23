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

	"agent-overflow/internal/platform"
)

const (
	// BrowserHelperEnvironment marks an Agent Overflow re-exec requested by
	// a provider's BROWSER hook. The sole argument is the OAuth HTTP(S) URL.
	BrowserHelperEnvironment = "AGENT_OVERFLOW_BROWSER_HELPER"
	BrowserHelperValue       = "1"
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

func open(ctx context.Context, candidates []Command, lookup commandLookup, start commandStarter) error {
	if len(candidates) == 0 {
		return fmt.Errorf("external URL opening is unsupported on %s", runtime.GOOS)
	}

	var errs []error
	for _, candidate := range candidates {
		if candidate.Name == "" {
			continue
		}
		if _, err := lookup(candidate.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", candidate.Name, err))
			continue
		}
		if err := start(ctx, candidate); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", candidate.Name, err))
			continue
		}
		return nil
	}

	if len(errs) == 0 {
		return fmt.Errorf("external URL opening is unsupported on %s", runtime.GOOS)
	}
	return fmt.Errorf("open external URL: %w", errors.Join(errs...))
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
