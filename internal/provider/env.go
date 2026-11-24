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
//
// A name is its exact spelling, for precedence, removal and the entry the
// child receives alike. That is what the child's operating system does:
// provider processes run only on Linux (WSL included) and macOS, where
// `bash_max_timeout_ms` and `BASH_MAX_TIMEOUT_MS` are two variables.
type EnvInput struct {
	// Custom is the user's custom environment for the provider
	// (settings.ProviderEnvMap), already refused for every reserved name.
	Custom map[string]string
	// Defaults apply over an inherited value; Custom and Pins outrank them.
	Defaults map[ReservedEnv]string
	// Pins apply over Custom and every inherited value.
	Pins map[ReservedEnv]string
	// Clears remove an inherited value; a value Custom, Defaults or Pins set
	// still applies.
	Clears []ReservedEnv
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
	parts := make(map[string]string, len(in.Custom)+len(in.Defaults)+len(in.Pins)+len(in.Clears)+1)
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
// AppImage launch artifacts, with in applied: Clears remove inherited values,
// then Defaults, Custom and Pins set theirs, a later kind replacing an earlier
// one's value, and PathPrepend goes in front of the inherited PATH. Each name
// appears once. The PATH prefix is read against the scrubbed base, so the
// mount's bin directory cannot re-enter through it.
//
// This is the ONE env rule every provider process gets: Spawn applies it, and
// the paths that launch a process themselves call it directly. Two env rules
// across providers is how an injected variable goes missing on one of them.
func BuildEnvironment(in EnvInput) []string {
	base := appimage.Scrub(os.Environ())
	values := make(map[string]string, len(in.Defaults)+len(in.Custom)+len(in.Pins)+1)
	order := make([]string, 0, len(in.Defaults)+len(in.Custom)+len(in.Pins)+1)
	set := func(name, value string) {
		if _, exists := values[name]; !exists {
			order = append(order, name)
		}
		values[name] = value
	}
	for env, value := range in.Defaults {
		set(env.Name(), value)
	}
	for name, value := range in.Custom {
		set(name, value)
	}
	for env, value := range in.Pins {
		set(env.Name(), value)
	}
	if in.PathPrepend != "" {
		value := in.PathPrepend
		if existing := envValue(base, pathName); existing != "" {
			value += string(os.PathListSeparator) + existing
		}
		set(pathName, value)
	}

	removed := make(map[string]struct{}, len(in.Clears)+len(values))
	for _, env := range in.Clears {
		removed[env.Name()] = struct{}{}
	}
	for name := range values {
		removed[name] = struct{}{}
	}
	out := make([]string, 0, len(base)+len(order))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if _, drop := removed[name]; !drop {
			out = append(out, entry)
		}
	}
	for _, name := range order {
		out = append(out, name+"="+values[name])
	}
	return out
}

// envValue returns the value of key in env, last entry wins.
func envValue(env []string, key string) string {
	value := ""
	for _, entry := range env {
		if name, candidate, found := strings.Cut(entry, "="); found && name == key {
			value = candidate
		}
	}
	return value
}
