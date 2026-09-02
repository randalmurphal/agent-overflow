//go:build !nogui

package main

import "runtime"

// serviceArtifactPlatform is the release-asset token THIS binary is, for the
// GUI build.
//
// The token is what `matchReleaseAsset` builds a file name from, and it is not
// `runtime.GOOS` in general (the WSL backend targets `wsl`); for the ordinary
// Linux binary it is. So a supervised `serve` running the desktop artifact
// updates itself to the next desktop artifact, which is right: the file the
// unit starts is the one the release feed calls
// `agent-overflow-linux-amd64`, and the version staged beside it must be the
// same kind of file.
//
// Two platforms answer "" — no remote update here — and the reasons differ:
//
//   - darwin publishes its artifact as an .app bundle zip. The supervisor
//     stages ONE executable into `versions/<v>/agent-overflow` and spawns it by
//     path; a bundle is a directory tree and an unpack step nothing in
//     `internal/supervise` has. `agent-overflow service update --binary` is
//     the path there, with the operator choosing what to install.
//   - windows is not a serve mode at all. Agent Overflow is a launcher there
//     that supervises its backend inside WSL, and `supervise` refuses to run
//     on it, so there is no supervised host for this to answer for.
//
// An empty token means ConfigureServiceUpdates never runs, and the status RPC
// says so in a sentence rather than offering a button that cannot work.
func serviceArtifactPlatform() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	return runtime.GOOS
}
