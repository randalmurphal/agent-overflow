package screenshot

import (
	"agent-overflow/internal/chromium"
	"agent-overflow/internal/eventchan"
)

// Installer remains an alias for callers that historically imported the
// screenshot package. Downloading, extraction, caching, and cache recovery are
// shared with the in-app browser through internal/chromium.
type Installer = chromium.Installer
type InstallProgress = chromium.InstallProgress
type InstallResult = chromium.InstallResult

const InstallEventName = eventchan.ScreenshotInstallProgress

func NewInstaller(configDir string, emit func(eventchan.Channel, any)) *Installer {
	return chromium.NewInstaller(configDir, chromium.ArtifactHeadlessShell, InstallEventName, emit)
}
