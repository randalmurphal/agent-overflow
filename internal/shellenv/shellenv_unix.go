//go:build !windows

package shellenv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// pathStartSentinel / pathEndSentinel bracket the captured PATH value
// in shell stdout. Shells in a login + interactive context can write
// MOTDs, fortune banners, etc. before/after our printenv call; the
// sentinels make extraction unambiguous regardless of that noise.
//
// Sentinels are deliberately namespaced ("__AO_SHELLENV_") so they
// can't collide with anything a user's rc files might emit.
const (
	pathStartSentinel = "__AO_SHELLENV_PATH_START__"
	pathEndSentinel   = "__AO_SHELLENV_PATH_END__"
)

// probeTimeout caps the shell probe. Bash startup with nvm / asdf
// sourced is typically 100-300 ms; 5 s leaves room for slow disks and
// pathological rc files without making startup feel hung when the
// shell never returns.
const probeTimeout = 5 * time.Second

// doSync is the platform implementation of Sync. Unix path: probe the
// user's login shell for PATH, merge into the process env.
func doSync(ctx context.Context) error {
	candidates := candidateShells()
	var lastErr error
	for _, shell := range candidates {
		loginPath, err := probe(ctx, shell)
		if err != nil {
			lastErr = err
			continue
		}
		merged := mergePath(loginPath, os.Getenv("PATH"))
		if merged == "" {
			lastErr = errors.New("shellenv: probe returned empty PATH")
			continue
		}
		if merged == os.Getenv("PATH") {
			return nil
		}
		if err := os.Setenv("PATH", merged); err != nil {
			return fmt.Errorf("shellenv: set PATH: %w", err)
		}
		return nil
	}
	if lastErr == nil {
		return errors.New("shellenv: no shell candidates")
	}
	return lastErr
}

// candidateShells returns the ordered list of shells to try. We start
// with the user's actual shell so their nvm / asdf / etc. config is
// sourced from the real rc files, then fall back to the platform's
// canonical shell so a misconfigured $SHELL doesn't disable the probe
// entirely.
//
// Duplicates are dropped: if $SHELL already names /bin/zsh on macOS we
// don't probe it twice.
func candidateShells() []string {
	seen := map[string]bool{}
	out := make([]string, 0, 3)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(os.Getenv("SHELL"))
	if runtime.GOOS == "darwin" {
		add("/bin/zsh")
	}
	add("/bin/bash")
	return out
}

// probe runs `<shell> -ilc 'echo <START>; printenv PATH; echo <END>'`
// with a deadline-bounded context, captures stdout, and returns the
// PATH value enclosed by our sentinels. Stderr is discarded — bash and
// zsh write benign warnings ("shopt: not in interactive shell", "no
// job control in this shell", etc.) when -i runs without a TTY, and
// surfacing them as errors would defeat the probe for everyone using
// these shells.
//
// The -ilc combination is deliberate: -l sources login files
// (/etc/profile, ~/.bash_profile or ~/.profile), -i sources rc files
// (~/.bashrc, ~/.zshrc), and -c lets us pass the script. nvm in
// particular installs into ~/.bashrc and only adds its bin dir to PATH
// when nvm.sh is sourced — which only happens in the interactive
// branch. Without -i, nvm-managed PATH would not be picked up.
func probe(ctx context.Context, shell string) (string, error) {
	if shell == "" {
		return "", errors.New("shellenv: empty shell")
	}

	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	script := fmt.Sprintf(
		"echo '%s'; printenv PATH; echo '%s'",
		pathStartSentinel, pathEndSentinel,
	)
	cmd := exec.CommandContext(pctx, shell, "-ilc", script)

	// Detach stdin so an `-i` shell that briefly probes for a TTY
	// doesn't hang forever waiting for input. /dev/null is the
	// canonical "this is not a TTY" signal.
	cmd.Stdin = nil

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("shellenv: %s -ilc: %w", shell, err)
	}

	return extractPath(stdout.String())
}

// extractPath finds the PATH value bracketed by our sentinels. The
// shell may print its banner / MOTD / etc. before our START sentinel
// and assorted cleanup output after END; both are ignored.
//
// Returns the trimmed PATH value, or an error if the sentinels aren't
// both present in the expected order.
func extractPath(out string) (string, error) {
	start := strings.Index(out, pathStartSentinel)
	if start < 0 {
		return "", errors.New("shellenv: start sentinel missing")
	}
	bodyStart := start + len(pathStartSentinel)
	end := strings.Index(out[bodyStart:], pathEndSentinel)
	if end < 0 {
		return "", errors.New("shellenv: end sentinel missing")
	}
	body := out[bodyStart : bodyStart+end]
	return strings.TrimSpace(body), nil
}

// mergePath concatenates the login-shell PATH with the inherited PATH,
// preserving order and dropping duplicates. Login-shell entries come
// first because that's the user's authoritative ordering — we don't
// want a system default like /usr/bin to shadow a homebrew or asdf
// shim the user explicitly put earlier in their config.
//
// Empty entries are dropped (some rc files leave a trailing colon, or
// produce a leading colon when prepending to an unset PATH).
func mergePath(loginPath, currentPath string) string {
	sep := string(os.PathListSeparator)
	seen := map[string]bool{}
	merged := make([]string, 0, 16)
	add := func(p string) {
		for _, e := range strings.Split(p, sep) {
			e = strings.TrimSpace(e)
			if e == "" || seen[e] {
				continue
			}
			seen[e] = true
			merged = append(merged, e)
		}
	}
	add(loginPath)
	add(currentPath)
	return strings.Join(merged, sep)
}
