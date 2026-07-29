package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Publishing the app binary as a command agent sessions can run (D30).
//
// The CLI is this executable, dispatched by verb (main_entry.go), so what a
// session needs is a PATH entry that resolves the name `agent-overflow` to
// whatever this process happens to be. That name cannot be assumed: a dev build
// is `bin/agent-overflow`, a Wails dev run is a temp binary, and a macOS bundle
// is `Agent Overflow.app/Contents/MacOS/Agent Overflow` — spaces and all. So
// boot writes one symlink under the app's own config directory, and
// sessionProcessEnv puts that directory at the front of the session's PATH.

const (
	// cliBinDirName is the directory, under the app config root, that holds
	// nothing but the canonical-name link. It is its own directory precisely so
	// prepending it to PATH exposes this command and nothing else.
	cliBinDirName = "bin"
	// cliCommandName is the name a session must be able to type. It is the
	// literal the docs, the composer block, and every usage string print.
	cliCommandName = "agent-overflow"
)

// ensureCLIBinDir publishes this executable under its canonical name and
// returns the directory to prepend to a session's PATH. An empty return means
// the command is not reachable — sessions then run without it, which the
// `/workflow` composer block reports rather than leaving the agent to discover
// as "command not found".
//
// Failure is loud but never fatal: an app that cannot write one symlink is
// still an app, and refusing to boot over it would trade a degraded feature
// for no product at all.
func (a *App) ensureCLIBinDir(configDir string) string {
	executable, err := os.Executable()
	if err != nil {
		log.Printf("cli path: locate this executable: %v; agent sessions will not find the `%s` command", err, cliCommandName)
		return ""
	}
	binDir, err := ensureCLISymlink(configDir, executable)
	if err != nil {
		log.Printf("cli path: %v; agent sessions will not find the `%s` command", err, cliCommandName)
		return ""
	}
	return binDir
}

// ensureCLISymlink makes <configDir>/bin/agent-overflow point at executable and
// returns <configDir>/bin. Returns ("", nil) on Windows, which never publishes
// the command: the Windows binary is a launcher for the WSL backend and spawns
// no provider children, so the sessions that would use the command live on the
// Linux side and get it from the Linux backend's own boot.
//
// The replacement is atomic — symlink to a temp name in the same directory,
// then rename over whatever is there — so a session resolving the command
// during an app restart sees either the old target or the new one, never a
// missing file. A target that already matches is left alone.
func ensureCLISymlink(configDir, executable string) (string, error) {
	if runtime.GOOS == "windows" {
		return "", nil
	}
	if strings.TrimSpace(configDir) == "" {
		return "", errors.New("publish CLI command: no config directory")
	}
	if strings.TrimSpace(executable) == "" {
		return "", errors.New("publish CLI command: no executable path")
	}
	target, err := filepath.Abs(executable)
	if err != nil {
		return "", fmt.Errorf("publish CLI command: resolve %q: %w", executable, err)
	}

	binDir := filepath.Join(configDir, cliBinDirName)
	if err := ensureAppPrivateDir(binDir); err != nil {
		return "", fmt.Errorf("publish CLI command: create %s: %w", binDir, err)
	}
	link := filepath.Join(binDir, cliCommandName)
	if current, err := os.Readlink(link); err == nil && current == target {
		return binDir, nil
	}

	// The pid suffix keeps two app processes booting at once from fighting over
	// one staging name; the rename makes whichever lands last the winner. Which
	// build wins does not matter to a caller: the CLI carries no state about the
	// app it belongs to, it POSTs to whatever AO_ENDPOINT its session env names.
	staging := link + ".staging." + strconv.Itoa(os.Getpid())
	if err := os.Remove(staging); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("publish CLI command: clear %s: %w", staging, err)
	}
	if err := os.Symlink(target, staging); err != nil {
		return "", fmt.Errorf("publish CLI command: link %s -> %s: %w", staging, target, err)
	}
	if err := os.Rename(staging, link); err != nil {
		// Leaving the staging link behind would make the next boot's Remove the
		// thing that reports this failure, long after the cause.
		if removeErr := os.Remove(staging); removeErr != nil {
			return "", fmt.Errorf("publish CLI command: install %s: %w (and %s could not be removed: %v)", link, err, staging, removeErr)
		}
		return "", fmt.Errorf("publish CLI command: install %s: %w", link, err)
	}
	return binDir, nil
}

// prependCLIBinDir puts binDir at the front of the environment overrides' PATH.
// The lookup is case-insensitive because provider.BuildEnvironment — which
// applies these overrides — matches PATH that way too; disagreeing would leave
// a `Path` override in place and this one appended to a variable nothing reads.
//
// Setting PATH to just binDir is not a truncation: BuildEnvironment appends the
// inherited PATH to whatever a PATH override says, so the child sees
// <binDir>:<the app's own PATH>.
func prependCLIBinDir(env map[string]string, binDir string) {
	for key, value := range env {
		if !strings.EqualFold(key, "PATH") {
			continue
		}
		if strings.TrimSpace(value) == "" {
			env[key] = binDir
			return
		}
		env[key] = binDir + string(os.PathListSeparator) + value
		return
	}
	env["PATH"] = binDir
}
