package acmecert

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"agent-overflow/internal/appimage"
	"agent-overflow/internal/procutil"
)

// ChallengePrefix is the label a DNS-01 TXT record is published under.
// Fixed by RFC 8555: the record for `example.com` is at
// `_acme-challenge.example.com`, and a hook is handed the whole name
// rather than being asked to build it, so a provider API that wants an
// absolute name and one that wants a relative one both get something
// unambiguous to work from.
const ChallengePrefix = "_acme-challenge."

// The two actions a hook is asked for. They are positional argument one;
// see the package doc for the whole invocation shape.
const (
	hookSet   = "set"
	hookClear = "clear"
)

// DefaultHookTimeout bounds one hook invocation. Generous, because a DNS
// API call plus a propagation wait the hook chose to do itself is a
// legitimate couple of minutes — and because the alternative to waiting
// is a validation the CA refuses, which costs an issuance against a rate
// limit rather than a retry.
const DefaultHookTimeout = 2 * time.Minute

// hookOutputTailBytes bounds the stdout+stderr a failing hook contributes
// to the error. A DNS tool prints its diagnosis last, so the tail is the
// useful end of an arbitrarily long stream.
const hookOutputTailBytes = 8 * 1024

// hookRunner is the seam the tests replace. Production is runHook.
type hookRunner func(ctx context.Context, argv []string, timeout time.Duration, action, fqdn, value string) error

// runHook invokes the user's DNS hook once.
//
// It runs in its own process group with a timeout that kills the group
// (internal/procutil): a hook that backgrounded its real work must not
// outlive the bound, and this runs unattended on the host with nobody
// watching — the same posture as a worktree setup command, and the same
// machinery.
//
// A failure carries the exit condition AND the output, because the two
// answer different questions: "the command was not found" is a settings
// problem and "NXDOMAIN for zone example.com" is a DNS one, and only the
// second is in the output.
func runHook(ctx context.Context, argv []string, timeout time.Duration, action, fqdn, value string) error {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return errors.New("the DNS hook has no command to run")
	}
	if timeout <= 0 {
		timeout = DefaultHookTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := append(append([]string(nil), argv[1:]...), action, fqdn, value)
	command := exec.CommandContext(runCtx, argv[0], full...)
	// The user's own tooling, PATH-resolving and long-lived, so the
	// inherited environment is scrubbed of AppImage launch artifacts like
	// every other child this app spawns (internal/appimage).
	command.Env = appimage.Scrub(os.Environ())
	tail := procutil.NewTailBuffer(hookOutputTailBytes)
	command.Stdout = tail
	command.Stderr = tail
	procutil.ConfigureGroup(command)

	err := command.Run()
	if err == nil {
		return nil
	}
	output := strings.TrimSpace(tail.String())
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("the DNS hook %q %s %s timed out after %s; output: %s",
			argv[0], action, fqdn, timeout, output)
	}
	return fmt.Errorf("the DNS hook %q %s %s failed: %w; output: %s",
		argv[0], action, fqdn, err, output)
}
