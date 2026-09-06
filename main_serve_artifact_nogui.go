//go:build nogui

package main

import "runtime"

// serviceArtifactPlatform is the release-asset token THIS binary is, for the
// windowless build.
//
// `nogui` produces two different artifacts and they are not interchangeable:
// the headless serve binary (`agent-overflow-headless-linux-amd64`, what a
// server runs) and the WSL backend payload the Windows launcher carries. Only
// the first can be a supervised serve host — `supervise` refuses to run on
// Windows, and the launcher owns the WSL payload's update through
// `internal/appupdate`'s WSL path — so the token here is the headless one, and
// it is `headless-<GOOS>` rather than `<GOOS>` for exactly the reason
// `matchReleaseAsset` matches exact names: a headless host that took
// `agent-overflow-linux-amd64` would stage a binary linked against GTK and
// WebKit that a server has no libraries for, and the failure would arrive as a
// trial that will not start rather than as a wrong choice anyone could see.
//
// No standalone nogui release is published for macOS or Windows. A Mac
// serve host installs the ordinary app bundle; Windows uses its WSL launcher.
func serviceArtifactPlatform() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	return "headless-" + runtime.GOOS
}
