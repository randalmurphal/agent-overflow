package provider

// Child-environment assembly: the one env rule every provider subprocess is
// built through, whichever entry point spawns it. Lives apart from the process
// lifecycle in process.go because its callers are not all Spawn — claudetui,
// textgen, the provider update runner and the MCP status checks assemble full
// []string environments directly.

import (
	"maps"
	"os"
	"slices"
	"strings"

	"agent-overflow/internal/appimage"
)

// EnvInput is everything a provider child's environment is built from, kept
// apart by kind so the precedence is one rule rather than each caller's merge
// order. Every name AO sets or clears is a ReservedEnv from the table in
// envtable.go; only the user's custom environment is free-form.
type EnvInput struct {
	// Custom is the user's custom environment for the provider
	// (settings.ProviderEnvMap), already refused for every reserved name.
	Custom map[string]string
	// Defaults apply where Custom does not name the variable, over an
	// inherited value.
	Defaults map[ReservedEnv]string
	// Pins apply over Custom and every inherited value.
	Pins map[ReservedEnv]string
	// Clears remove an inherited value.
	Clears []ReservedEnv
	// ClearUnlessCustom remove an inherited value unless Custom names the
	// variable, so the user's own value is the only one the child can see.
	ClearUnlessCustom []ReservedEnv
	// PathPrepend, when set, goes in front of the inherited PATH.
	PathPrepend string
}

// Pin returns a copy of in with env pinned to value.
func (in EnvInput) Pin(env ReservedEnv, value string) EnvInput {
	in.Pins = withEntry(in.Pins, env, value)
	return in
}

// Default returns a copy of in with value as env's default.
func (in EnvInput) Default(env ReservedEnv, value string) EnvInput {
	in.Defaults = withEntry(in.Defaults, env, value)
	return in
}

// Clear returns a copy of in that also clears envs.
func (in EnvInput) Clear(envs ...ReservedEnv) EnvInput {
	in.Clears = append(slices.Clip(in.Clears), envs...)
	return in
}

// Fingerprint identifies what in sets and clears: two inputs with one
// fingerprint build the same environment from the same host environment.
// Pool keys use it so a connection is shared only by sessions whose process
// would have been started with the same environment.
func (in EnvInput) Fingerprint() string {
	parts := make(map[string]string, len(in.Custom)+len(in.Defaults)+len(in.Pins)+len(in.Clears)+len(in.ClearUnlessCustom)+1)
	for name, value := range in.Custom {
		parts["custom:"+name] = value
	}
	for env, value := range in.Defaults {
		parts["default:"+env.Name()] = value
	}
	for env, value := range in.Pins {
		parts["pin:"+env.Name()] = value
	}
	for _, env := range in.Clears {
		parts["clear:"+env.Name()] = ""
	}
	for _, env := range in.ClearUnlessCustom {
		parts["clearUnlessCustom:"+env.Name()] = ""
	}
	if in.PathPrepend != "" {
		parts["pathPrepend"] = in.PathPrepend
	}
	return EnvFingerprint(parts)
}

func withEntry(m map[ReservedEnv]string, env ReservedEnv, value string) map[ReservedEnv]string {
	out := make(map[ReservedEnv]string, len(m)+1)
	maps.Copy(out, m)
	out[env] = value
	return out
}

// BuildEnvironment returns the current process environment, scrubbed of the
// AppImage launch artifacts, with in applied: Clears and ClearUnlessCustom
// remove inherited values, Defaults apply where Custom does not name the
// variable, then Custom, then Pins. Each variable appears once; removal is
// case-insensitive so a Windows-style `Path` collides with `PATH`. The PATH
// prefix is read against the scrubbed base, so the mount's bin directory
// cannot re-enter through it.
//
// This is the ONE env rule every provider process gets: Spawn applies it, and
// the paths that launch a process themselves call it directly. Two env rules
// across providers is how an injected variable goes missing on one of them.
func BuildEnvironment(in EnvInput) []string {
	base := appimage.Scrub(os.Environ())
	set := make([]string, 0, len(in.Defaults)+len(in.Custom)+len(in.Pins)+1)
	named := make(map[string]struct{}, len(in.Custom))
	for name := range in.Custom {
		named[strings.ToUpper(name)] = struct{}{}
	}
	for env, value := range in.Defaults {
		if _, overridden := named[strings.ToUpper(env.Name())]; !overridden {
			if _, pinned := in.Pins[env]; !pinned {
				set = append(set, env.Name()+"="+value)
			}
		}
	}
	for name, value := range in.Custom {
		if !pinsName(in.Pins, name) {
			set = append(set, name+"="+value)
		}
	}
	for env, value := range in.Pins {
		set = append(set, env.Name()+"="+value)
	}
	if in.PathPrepend != "" {
		value := in.PathPrepend
		if existing := envValue(base, PathEnv.Name()); existing != "" {
			value += string(os.PathListSeparator) + existing
		}
		set = append(set, PathEnv.Name()+"="+value)
	}

	removed := make([]string, 0, len(in.Clears)+len(in.ClearUnlessCustom)+len(set))
	for _, env := range in.Clears {
		removed = append(removed, env.Name())
	}
	for _, env := range in.ClearUnlessCustom {
		if _, userSet := named[strings.ToUpper(env.Name())]; !userSet {
			removed = append(removed, env.Name())
		}
	}
	for _, entry := range set {
		name, _, _ := strings.Cut(entry, "=")
		removed = append(removed, name)
	}
	return append(filterEnvironment(base, removed...), set...)
}

func pinsName(pins map[ReservedEnv]string, name string) bool {
	for env := range pins {
		if strings.EqualFold(env.Name(), name) {
			return true
		}
	}
	return false
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

// filterEnvironment removes named variables from an already scrubbed env.
// Matching is case-insensitive so it is also safe for Windows-style
// environments.
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
