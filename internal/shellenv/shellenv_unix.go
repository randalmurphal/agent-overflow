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

	"agent-overflow/internal/appimage"
	"agent-overflow/internal/procutil"
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
	varsStartSentinel = "__AO_SHELLENV_VARS_START__"
	varsEndSentinel   = "__AO_SHELLENV_VARS_END__"
)

// importedVars are the variables besides PATH that the probe carries into
// the process environment: where TLS clients find trusted certificates and
// which proxy they use. On a network that inspects TLS, child processes
// such as uv, Python requests and Node fail without them, and nothing else
// would hand them to a backend started outside a shell. Credentials and
// provider settings stay out: provider accounts own those.
var importedVars = []string{
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
}

// loginShell is what the probe read from one shell: its PATH and the
// importedVars it set to a non-empty value.
type loginShell struct {
	path string
	vars map[string]string
}

// probeTimeout caps the shell probe. Bash startup with nvm / asdf
// sourced is typically 100-300 ms; 5 s leaves room for slow disks and
// pathological rc files without making startup feel hung when the
// shell never returns.
const probeTimeout = 5 * time.Second

// doSync is the platform implementation of Sync. Unix path: probe the
// user's login shell, merge its PATH and importedVars into the process
// env, then keep loopback off any proxy, whether or not the probe worked.
func doSync(ctx context.Context) error {
	err := syncFromLoginShell(ctx)
	if bypassErr := ensureLoopbackBypassesProxy(); bypassErr != nil {
		err = errors.Join(err, bypassErr)
	}
	return err
}

func syncFromLoginShell(ctx context.Context) error {
	candidates := candidateShells()
	var lastErr error
	for _, shell := range candidates {
		login, err := probe(ctx, shell)
		if err != nil {
			lastErr = err
			continue
		}
		merged := mergePath(login.path, os.Getenv("PATH"))
		if merged == "" {
			lastErr = errors.New("shellenv: probe returned empty PATH")
			continue
		}
		if merged != os.Getenv("PATH") {
			if err := os.Setenv("PATH", merged); err != nil {
				return fmt.Errorf("shellenv: set PATH: %w", err)
			}
		}
		return importVars(login.vars)
	}
	if lastErr == nil {
		return errors.New("shellenv: no shell candidates")
	}
	return lastErr
}

// importVars sets each probed variable the process does not already have.
// A value the app was launched with, even an empty one, is the launcher's
// choice and wins over the shell's.
func importVars(vars map[string]string) error {
	for _, name := range importedVars {
		value, ok := vars[name]
		if !ok {
			continue
		}
		if _, inherited := os.LookupEnv(name); inherited {
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			return fmt.Errorf("shellenv: set %s: %w", name, err)
		}
	}
	return nil
}

// loopbackHosts are the names a client may compare a loopback URL against.
// Bracketed ::1 is how some clients spell the host of http://[::1]:port.
var loopbackHosts = []string{"localhost", "127.0.0.1", "::1", "[::1]"}

// ensureLoopbackBypassesProxy adds loopbackHosts to NO_PROXY and no_proxy
// when a proxy is configured. Child processes reach this app at
// http://[::1]:port (MCP servers, the ao CLI, the Claude gateway); Node and
// Rust clients follow proxy variables literally, so a NO_PROXY written for
// 127.0.0.1 alone would send those calls to a proxy that cannot reach this
// machine. Go never proxies loopback, so the backend's own clients are
// unaffected either way. Each spelling that is set is extended; when
// neither is, both are set.
func ensureLoopbackBypassesProxy() error {
	proxied := false
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		if os.Getenv(name) != "" {
			proxied = true
			break
		}
	}
	if !proxied {
		return nil
	}
	names := []string{}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		if _, ok := os.LookupEnv(name); ok {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		names = []string{"NO_PROXY", "no_proxy"}
	}
	for _, name := range names {
		current := os.Getenv(name)
		if extended := withLoopbackHosts(current); extended != current {
			if err := os.Setenv(name, extended); err != nil {
				return fmt.Errorf("shellenv: set %s: %w", name, err)
			}
		}
	}
	return nil
}

// withLoopbackHosts appends the loopbackHosts a NO_PROXY list lacks. "*"
// already bypasses every host.
func withLoopbackHosts(list string) string {
	present := map[string]bool{}
	for _, entry := range strings.Split(list, ",") {
		present[strings.ToLower(strings.TrimSpace(entry))] = true
	}
	if present["*"] {
		return list
	}
	extended := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(list), ","))
	for _, host := range loopbackHosts {
		if present[host] {
			continue
		}
		if extended != "" {
			extended += ","
		}
		extended += host
	}
	return extended
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

// probe runs `<shell> -ilc 'echo <START>; printenv PATH; echo <END>; ...'`
// with a deadline-bounded context, captures stdout, and returns the
// PATH value enclosed by our sentinels plus the importedVars the shell
// set, one NAME=value line each inside a second sentinel pair. The
// script uses only echo, printf and printenv, so fish runs it too. Stderr is discarded — bash and
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
func probe(ctx context.Context, shell string) (loginShell, error) {
	if shell == "" {
		return loginShell{}, errors.New("shellenv: empty shell")
	}

	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	var script strings.Builder
	fmt.Fprintf(&script, "echo '%s'; printenv PATH; echo '%s'; echo '%s'", pathStartSentinel, pathEndSentinel, varsStartSentinel)
	for _, name := range importedVars {
		fmt.Fprintf(&script, "; printf '%%s=' %s; printenv %s; echo", name, name)
	}
	fmt.Fprintf(&script, "; echo '%s'", varsEndSentinel)
	cmd := exec.CommandContext(pctx, shell, "-ilc", script.String())
	// An interactive bash or zsh whose process group is not its terminal's
	// foreground group stops that group with SIGTTIN, which would freeze
	// this process too when it runs in a background group (an update trial,
	// or an app backgrounded from a shell). A new session has no terminal.
	// Its group is also killed at the timeout, with anything the rc files
	// started, and a descendant holding stdout cannot outlast the timeout.
	procutil.ConfigureGroup(cmd)

	// The probe's whole job is to report the user's REAL PATH. Started
	// from an AppImage, the inherited PATH carries the squashfs mount's
	// bin dirs, and profiles that extend rather than assign
	// (`PATH=$PATH:…`) would fold those into the answer — which outlives
	// the mount. Scrub the launch artifacts so the shell rebuilds from a
	// clean base; nil on every other launch shape.
	cmd.Env = appimage.ScrubInherited()

	// Detach stdin so an `-i` shell that briefly probes for a TTY
	// doesn't hang forever waiting for input. /dev/null is the
	// canonical "this is not a TTY" signal.
	cmd.Stdin = nil

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard

	// ErrWaitDelay means the shell exited successfully while something its
	// rc files started still holds stdout; the output is complete.
	if err := cmd.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return loginShell{}, fmt.Errorf("shellenv: %s -ilc: %w", shell, err)
	}

	out := stdout.String()
	path, err := extractPath(out)
	if err != nil {
		return loginShell{}, err
	}
	vars, err := extractVars(out)
	if err != nil {
		return loginShell{}, err
	}
	return loginShell{path: path, vars: vars}, nil
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

// extractVars reads the NAME=value lines between the vars sentinels and
// keeps the importedVars with a non-empty value. An unset variable prints
// as "NAME=" and is left out.
func extractVars(out string) (map[string]string, error) {
	start := strings.Index(out, varsStartSentinel)
	if start < 0 {
		return nil, errors.New("shellenv: vars start sentinel missing")
	}
	bodyStart := start + len(varsStartSentinel)
	end := strings.Index(out[bodyStart:], varsEndSentinel)
	if end < 0 {
		return nil, errors.New("shellenv: vars end sentinel missing")
	}
	wanted := map[string]bool{}
	for _, name := range importedVars {
		wanted[name] = true
	}
	vars := map[string]string{}
	for _, line := range strings.Split(out[bodyStart:bodyStart+end], "\n") {
		name, value, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok || !wanted[name] || strings.TrimSpace(value) == "" {
			continue
		}
		vars[name] = value
	}
	return vars, nil
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
