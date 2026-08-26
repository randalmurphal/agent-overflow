package main

import (
	"fmt"
	"runtime/debug"
	"strings"

	"agent-overflow/internal/harnessclient"
)

// version is stamped at link time by the Makefile
// (-X main.version=$(VERSION)) alongside the backend binary it ships
// with. "dev" is what a bare `go build ./cmd/ao-harness` produces.
var version = "dev"

// buildStamp is what `ao-harness version` prints: the linked version
// plus the VCS revision Go records for a module build, which is what
// actually identifies a binary somebody built by hand.
func buildStamp() string {
	stamp := version
	if info, ok := debug.ReadBuildInfo(); ok {
		revision, modified := "", false
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = setting.Value
			case "vcs.modified":
				modified = setting.Value == "true"
			}
		}
		if revision != "" {
			if len(revision) > 12 {
				revision = revision[:12]
			}
			if modified {
				revision += "-dirty"
			}
			stamp += " (" + revision + ")"
		}
	}
	return stamp
}

func runVersion(e *env, args []string) error {
	flags := e.newFlagSet("version")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("version takes no positional arguments (got %v)", rest)
	}
	if e.jsonOutput() {
		return e.writeJSON(map[string]any{"version": version, "build": buildStamp()})
	}
	e.printf("ao-harness %s\n", buildStamp())
	return nil
}

// warnVersionSkew says so when the CLI and the instance it just attached
// to were built from different trees. A stale bin/ao-harness against a
// freshly rebuilt backend is the quiet failure mode this exists for: the
// RPC surface moved, the CLI still speaks the old one, and every error
// reads like a bug in the backend.
//
// A warning, never a refusal: the two are independently useful across a
// version bump, and the whole point of a debugging tool is that it still
// runs when things do not line up.
func (e *env) warnVersionSkew(bs harnessclient.Bootstrap) {
	backend := strings.TrimSpace(bs.Version)
	if backend == "" || version == "dev" || backend == version {
		return
	}
	fmt.Fprintf(e.stderr,
		"ao-harness: this CLI is %s but the instance is %s; rebuild with `make harness-build` if a command behaves oddly\n",
		version, backend)
}
