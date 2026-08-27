package screenshot

import (
	"agent-overflow/internal/chromium"
	"agent-overflow/internal/eventchan"
)

type Installer = chromium.Installer
type InstallProgress = chromium.InstallProgress
type InstallResult = chromium.InstallResult

const InstallEventName = eventchan.ScreenshotInstallProgress

func NewInstaller(configDir string, emit func(eventchan.Channel, any)) *Installer {
	return chromium.NewInstaller(configDir, chromium.ArtifactHeadlessShell, InstallEventName, emit)
}
