package main

import "runtime/debug"

// buildStamp supplements the semantic version with the VCS revision recorded
// by Go when this binary was built. It is used by harness capabilities so a
// report can identify a locally built binary without changing the wire's
// semantic version field.
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
