package app

import (
	"log"
	"maps"
	"net"
	"strconv"
	"time"

	"agent-overflow/internal/network"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/windowgeom"
)

// IsolationConfig is the complete mocked-provider boot contract shared by the
// harness and soak modes. Keeping the four safety pins in one value prevents a
// new boot mode from applying only part of the isolation boundary.
type IsolationConfig struct {
	ProviderBinary         string
	CredentialHome         string
	UseFileKeychain        bool
	DisableBackgroundFetch bool
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
// and the address shown in Settings → Network are produced by one
// formatter and cannot drift.
func ServeEndpoints(a *App, srv *transport.Server) network.Settings {
	return network.FromServer(srv, a.persistedNetworkSettings())
}

// SetDataDirOverride installs the executable's --data-dir boot input.
func SetDataDirOverride(a *App, dataDir string) { a.dataDirOverride = dataDir }

// SetCertFingerprint installs the fingerprint of the TLS certificate the
// transport terminates with, which every pairing link then carries. The
// boot resolves the certificate and the listener from one value, so the
// string a device is told to pin is the string that listener presents.
// Call before the transport serves.
func SetCertFingerprint(a *App, fingerprint string) { a.certFingerprint = fingerprint }

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
