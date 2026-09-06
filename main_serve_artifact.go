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
// macOS downloads its complete signed .app bundle. supervise.PrepareArtifact
// retains that bundle and publishes the same flat launcher older supervisors
// understand. Windows itself is a WSL launcher, never a supervised serve host.
func serviceArtifactPlatform() string {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return ""
	}
	return runtime.GOOS
}
