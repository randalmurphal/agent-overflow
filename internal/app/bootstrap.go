package app

import (
	"log"
	"maps"
	"net"
	"strconv"
	"time"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/windowgeom"
)

// IsolationConfig is the complete mocked-provider boot contract shared by the
// harness and soak modes. Keeping the safety pins in one value prevents a new
// boot mode from applying only part of the isolation boundary.
type IsolationConfig struct {
	ProviderBinary         string
	CredentialHome         string
	UseFileKeychain        bool
	DisableBackgroundFetch bool
	// MockBrowserEngine pins the browser Manager to the fake engine. A mocked
	// boot has no display and must never open a real one, yet its pane chrome
	// and host rect still have to render (spec §10).
	MockBrowserEngine bool
}

// ConfigureIsolation applies every mocked-provider safety pin before Start.
func ConfigureIsolation(a *App, config IsolationConfig) {
	// Isolated boots still exercise the production notification pipe. With no
	// subscriber a headless harness presents nothing; a windowed soak reaches
	// the real launcher bridge.
	a.osNotifications = newTransportNotificationSender(a)
	a.providerBinaryOverride = config.ProviderBinary
	a.credentialHomeOverride = config.CredentialHome
	a.fileKeychainOverride = config.UseFileKeychain
	a.backgroundFetchDisabled = config.DisableBackgroundFetch
	a.browser.mockEngine = config.MockBrowserEngine
}

// SetDataDirOverride installs the executable's --data-dir boot input.
func SetDataDirOverride(a *App, dataDir string) { a.dataDirOverride = dataDir }

// SetBrowserCDPRelay installs the backend end of the Windows launcher's CDP
// tunnel, which the executable creates before the transport so the same
// object can serve the /browser-cdp route.
//
// Non-nil only on the WSL deployment, and that is the whole engine
// selection: the browser Manager takes the hosted (embedded-pane) engine
// exactly when a relay exists, so "which engine" and "is there a launcher
// to host windows" can never disagree. Must be called before Start.
func SetBrowserCDPRelay(a *App, relay appbrowser.CDPRelay) { a.browser.cdpRelay = relay }

// EnsurePrivateDir applies the same ownership and mode rules used by App
// startup to a bootstrap-owned data directory.
func EnsurePrivateDir(path string) error { return ensureAppPrivateDir(path) }

// SetProviderExtraEnv installs mock-control credentials before Start. Copying
// makes the write-once boot input independent of the control server's map.
func SetProviderExtraEnv(a *App, env map[string]string) {
	a.providerExtraEnv = maps.Clone(env)
}

// ConfigureTransportNotifications installs the headless launcher bridge.
func ConfigureTransportNotifications(a *App) {
	a.osNotifications = newTransportNotificationSender(a)
}

// BackendIdentity exposes the store identity to the transport manifest without
// exporting App's persistence internals.
func BackendIdentity(a *App) (backendID, replicaGeneration string) {
	return a.backendIdentity()
}

// PersistWindowGeometry is the desktop tracker's narrow settings sink.
func PersistWindowGeometry(a *App, geometry windowgeom.Geometry) {
	a.persistWindowGeometry(geometry)
}

// PortFromAddr extracts the numeric port from a host:port address. Invalid
// addresses return zero so launchers can surface one consistent boot failure.
func PortFromAddr(addr string) int {
	_, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return 0
	}
	return port
}

func portFromAddr(addr string) int { return PortFromAddr(addr) }

// LogBootPhase keeps root bootstrap and service startup on one diagnostic
// format after the service moved into this importable package.
func LogBootPhase(phase string, started time.Time) {
	log.Printf("boot: phase=%s duration=%s", phase, time.Since(started).Round(time.Millisecond))
}

func logBootPhase(phase string, started time.Time) { LogBootPhase(phase, started) }
