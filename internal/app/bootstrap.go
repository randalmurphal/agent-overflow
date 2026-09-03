package app

import (
	"log"
	"maps"
	"net"
	"strconv"
	"time"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/network"
	"agent-overflow/internal/transport"
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

// UseFileKeychain moves provider credentials and the browser companion's
// state key out of the OS keychain and into 0600 files under the config
// root. Call before Start.
//
// This is the SAME pin ConfigureIsolation sets, reached for an unrelated
// reason, and the two callers must not be confused. A mocked boot sets it
// so a test can never touch the developer's real keychain. The `serve`
// boot sets it because an unattended host has no login session: on Linux
// the Secret Service lives in the desktop session's D-Bus, and on macOS
// the login keychain is unlocked by a person logging in. A backend started
// by systemd at boot, or by launchd before anyone signs in, would either
// block on a prompt nobody can answer or silently fall back — and "the
// operator's provider logins disappeared after a reboot" is the shape that
// second outcome takes. Files under a root this process already owns are
// the honest posture for a machine nobody is sitting at
// (docs/architecture/serve-mode.md).
//
// It is a separate function from ConfigureIsolation rather than a fifth
// field on it because serve is not an isolated boot: it spawns the REAL
// provider CLIs against the operator's real provider homes, which is the
// entire point of leaving it running.
func UseFileKeychain(a *App) { a.fileKeychainOverride = true }

// UseHeadlessBrowserEngine asks for the headless Chromium browser engine.
// Call before Start, like the two above: the Manager is built during startup
// and its engine is chosen once, for the life of the process.
//
// It is a separate function from ConfigureIsolation for the same reason
// UseFileKeychain is — serve is not an isolated boot — and it is a REQUEST
// rather than an inference for a much sharper reason. Every other windowless
// deployment (a `--connect` backend, the harness, `go test` itself) also has
// no window, and inferring "no window, therefore launch a browser" would make
// the suite start browsers on the developer's machine. So the serve boot,
// which is the one deployment that wants one, says so by name
// (docs/specs/remote-access.md §7).
//
// Asking does not guarantee an engine: the browser has to already be on the
// machine, because nothing is ever downloaded. A serve host without one keeps
// the windowless answer — no engine, no MCP server, one line in the log.
func UseHeadlessBrowserEngine(a *App) { a.browser.headlessChromium = true }

// BrowserToolsAvailable reports whether this backend has a browser engine
// at all, which is what the transport advertises as CapabilityBrowser.
//
// Deployment, not settings and not authorization: the engine is chosen once
// during Start, so this answers "could browser tools work on this machine"
// and never "are they switched on" or "may this caller use one". A serve
// host with no Chromium installed answers false, and a client reading that
// can say why instead of offering a surface with nothing behind it.
//
// False before Start, and false on a bare fixture with no Manager, which is
// the same answer both deserve.
func BrowserToolsAvailable(a *App) bool {
	return a.browser.manager != nil && a.browser.manager.Available()
}

// HasEnrolledDevice reports whether any device could still sign in to this
// backend from somewhere else.
//
// Two exclusions, and each is what makes the answer useful to the `serve`
// console. The LOCAL PAGE CHANNEL is not a paired device: it is this
// backend's own row, resolved on every boot for the window this mode never
// opens, so counting it would mean a serve host was never fresh. A REVOKED
// row is not access either — the owner ended it deliberately — and an
// owner who revoked their last device on a machine with no screen has no
// remaining way in, which is the one moment the console most needs to
// offer enrollment.
//
// So this answers "is there a device that could reach this backend", not
// "has this backend ever been paired". The second question would leave a
// headless host locked out of itself after a revocation.
func HasEnrolledDevice(a *App) (bool, error) { return a.hasEnrolledDevice() }

// ServeEndpoints is the addressing summary a windowless boot prints for
// the person who started it: the share URL, this launch's token, whether
// that URL is cleartext, and the tailnet URL when the node is up.
//
// It is GetNetworkSettings' own composition — the persisted preferences
// through network.FromServer — so the address printed on a serve console
// and the address shown in Settings → Remote access are produced by one
// formatter and cannot drift.
//
// The FULL variant deliberately, and it needs no host-present pick: this
// writes to the console of the process itself, which is the machine. The
// pick GetNetworkSettings applies is about a caller across a network, and
// there is no caller here.
func ServeEndpoints(a *App, srv *transport.Server) network.Settings {
	return network.FromServer(srv, a.persistedNetworkSettings())
}

// SetBoundPortRecorder installs the sink that persists the port this
// listener is on, so the executable's transport-port cache keeps naming
// the previous bind after Settings → Remote access moves it. Call before the
// transport serves.
//
// A callback rather than a path, because the cache's format, location and
// failure policy are the executable's (main_transport_port.go) and this
// package has no business restating any of them.
func SetBoundPortRecorder(a *App, record func(port int)) { a.boundPortRecorder = record }

// SetDataDirOverride installs the executable's --data-dir boot input.
func SetDataDirOverride(a *App, dataDir string) { a.dataDirOverride = dataDir }

// SetCertFingerprint installs the fingerprint of the TLS certificate the
// transport terminates with, which every pairing link then carries. The
// boot resolves the certificate and the listener from one value, so the
// string a device is told to pin is the string that listener presents.
// Call before the transport serves.
func SetCertFingerprint(a *App, fingerprint string) { a.certFingerprint = fingerprint }

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
