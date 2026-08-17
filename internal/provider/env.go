package provider

// Child-environment assembly: the one env rule every provider subprocess is
// built through, whichever entry point spawns it. Lives apart from the process
// lifecycle in process.go because its callers are not all Spawn — claudetui
// and textgen assemble full []string environments directly.

import (
	"os"
	"strings"

	"agent-overflow/internal/appimage"
)

// BuildEnvironment returns the current process environment, scrubbed of the
// AppImage launch artifacts, with the requested variables removed and explicit
// overrides applied. PATH overrides are additive so provider-specific binary
// directories do not hide the user's normal command search path — and the
// inherited half of that merge is read back off the scrubbed base, so the
// mount's bin directory cannot re-enter through the override path.
//
// This is the ONE env rule every provider process gets, and it is exported for
// that reason: providers whose Config carries an override map reach it through
// Spawn, and the ones whose Config carries a full []string environment
// (claudetui, which launches a real TUI) call it directly. Two different env
// rules across providers is exactly how an injected variable goes missing on
// one of them — which is why an override also *replaces* the inherited value
// rather than being appended after it. Appending happens to work under
// exec.Cmd's last-wins rule and does not under every consumer of a []string
// environment, so the duplicate never gets to exist.
func BuildEnvironment(overrides map[string]string, unset ...string) []string {
	base := appimage.Scrub(os.Environ())
	removed := make([]string, 0, len(unset)+len(overrides))
	removed = append(removed, unset...)
	for key := range overrides {
		removed = append(removed, key)
	}
	env := filterEnvironment(base, removed...)
	for key, value := range overrides {
		if strings.EqualFold(key, "PATH") {
			if existing := envValue(base, "PATH"); existing != "" {
				value += string(os.PathListSeparator) + existing
			}
		}
		env = append(env, key+"="+value)
	}
	return env
}

// envValue returns the value of key in env, last entry wins. Lookup is
// case-insensitive to match the removal rule below: a Windows-style
// environment that spells the variable `Path` must not have it removed as an
// override collision and then re-read as empty here.
func envValue(env []string, key string) string {
	value := ""
	for _, entry := range env {
		name, candidate, found := strings.Cut(entry, "=")
		if found && strings.EqualFold(name, key) {
			value = candidate
		}
	}
	return value
}

// FilterEnvironment removes named variables from env and scrubs the AppImage
// launch artifacts out of what remains. An empty env means inherit the current
// process environment, matching exec.Cmd's default and the previous fetcher
// behavior. Matching is case-insensitive so this helper is also safe for
// Windows-style environments.
//
// The scrub lives here rather than at each call site because every caller is
// assembling a child environment: a provider CLI must resolve its binaries and
// libraries against the user's real system, not against a squashfs mount that
// disappears when Agent Overflow exits. It is marker-gated and idempotent, so
// a non-AppImage launch is untouched and an already-scrubbed env passed back
// in stays as it is.
func FilterEnvironment(env []string, unset ...string) []string {
	if len(env) == 0 {
		env = os.Environ()
	}
	return filterEnvironment(appimage.Scrub(env), unset...)
}

// filterEnvironment is FilterEnvironment's removal half, over an environment
// the caller has already scrubbed. Splitting the two is what lets
// BuildEnvironment scrub exactly once, before it reads PATH back out for the
// additive merge.
func filterEnvironment(env []string, unset ...string) []string {
	if len(unset) == 0 {
		return append([]string(nil), env...)
	}
	removed := make(map[string]struct{}, len(unset))
	for _, key := range unset {
		removed[strings.ToUpper(strings.TrimSpace(key))] = struct{}{}
	}
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, found := strings.Cut(entry, "=")
		if _, shouldRemove := removed[strings.ToUpper(key)]; found && shouldRemove {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}
