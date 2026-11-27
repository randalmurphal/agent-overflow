package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/dictation"
	"agent-overflow/internal/network"
	"agent-overflow/internal/openai/openaifake"
	"agent-overflow/internal/provider/claude/claudefake"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/providerupdate/updatefake"
	"agent-overflow/internal/trackerapp"
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
	// DisableRetentionSweep keeps the retention TTL sweep from starting.
	// Fixture rows can carry fixed past timestamps, and a sweep firing once
	// a long-lived harness passes its settle gate would delete them under a
	// running test or its reset.
	DisableRetentionSweep bool
	// MockBrowserEngine pins the browser Manager to the fake engine. A mocked
	// boot has no display and must never open a real one, yet its pane chrome
	// and host rect still have to render (spec §10).
	MockBrowserEngine bool
	// WorkspaceRoot bounds every project path and session working directory
	// (app_isolated_workspace.go). Empty disables the check, which only unit
	// tests do.
	WorkspaceRoot string
	// ForgeCLI is the fake forge CLI (ao-mockforge) run in place of gh and
	// glab. Isolation applies whether or not it is set: empty means every
	// forge CLI invocation fails with a named error instead of reaching
	// the real CLI on PATH.
	ForgeCLI string
	// TrackerOrigin says where an isolated boot's tracker clients connect:
	// a function from a connection's kind and site to a loopback fake.
	// Isolation applies whether or not it is set: nil refuses every
	// tracker connection before anything is sent.
	TrackerOrigin trackerapp.Origin
	// ScanScopePIDs are further processes whose trees dev-server discovery
	// may look at (devscan.NewScoped). The scope always includes this
	// backend's own tree, which holds the providers and terminals it
	// spawns, so an isolated boot never dials a listener outside the
	// processes it was given.
	ScanScopePIDs []int
	// ThreadRequestPoll, TransferPendingRetry, PRUpdateInterval,
	// PRUpdateRetryBase, PRCILiveInterval, PRCIFollowInterval,
	// PRCILogWaitInterval, MergedPRPollInterval and PRLookupRetryBase
	// replace the agent thread request poll cadence,
	// threadtransfer.PendingRetry, the pull request poll cadence and its
	// first retry delay, the CI phase's three cadences, the merged-PR
	// worktree cleanup's per-repository poll (which also bounds its tick
	// and first poll), and how long a first failed open-PR lookup is
	// cached (git.WithPRLookupErrorBase), so an end-to-end test
	// does not wait out a production interval it is not about. Zero keeps
	// the product value; only a harness boot sets them
	// (diagenv.HarnessTiming).
	ThreadRequestPoll    time.Duration
	TransferPendingRetry time.Duration
	PRUpdateInterval     time.Duration
	PRUpdateRetryBase    time.Duration
	PRCILiveInterval     time.Duration
	PRCIFollowInterval   time.Duration
	PRCILogWaitInterval  time.Duration
	MergedPRPollInterval time.Duration
	PRLookupRetryBase    time.Duration
	// IdleReapThreshold, IdleReapInterval and WakeupReapGrace replace the
	// idle-session reaper's inactivity window, sweep cadence and
	// scheduled-wakeup grace (app_session_reaper.go). Zero keeps the
	// product value.
	IdleReapThreshold time.Duration
	IdleReapInterval  time.Duration
	WakeupReapGrace   time.Duration
	// AgentWakeFloor replaces the shortest interval a loop or watch may
	// register (agentWakeFloor), so a test can watch fires without waiting
	// out a minute each. Zero keeps the product floor.
	AgentWakeFloor time.Duration
	// AgentWatchFailureWindow replaces how long a watch keeps failing before
	// the person is told (agentWatchFailureWindow). Zero keeps the product
	// window.
	AgentWatchFailureWindow time.Duration
	// RemoteWatchPoll replaces how often the remote-job watcher asks the
	// destination about a job still running, so a test can act between two
	// of its checks. Zero keeps the product cadence.
	RemoteWatchPoll time.Duration
	// ProviderEventDrainStall replaces providerEventDrainStall, how long a
	// session stop waits for the thread's event worker to handle its next
	// event, so a test can stall a worker past it without waiting seconds.
	// Zero keeps the product bound.
	ProviderEventDrainStall time.Duration
	// UsageProbeGap replaces the shortest gap between two usage probes of
	// one provider, so a spec that sets a usage answer is not held behind a
	// probe the boot just made. Zero keeps the product gap.
	UsageProbeGap time.Duration
	// UsagePollInterval replaces the usage poll's tick, so a spec that
	// asserts what the readings it sets did can keep the poll from taking
	// one of its own in between. Zero keeps the product tick.
	UsagePollInterval time.Duration
	// ThreadWakeRetryBase replaces threadWakeRetryBase, the first wait
	// before an undelivered wake is tried again; the ceiling keeps its
	// product ratio (threadWakeRetryDelay). Zero keeps the product wait.
	ThreadWakeRetryBase time.Duration
	// AgentSettledWindow replaces threadAgentSettledWindow, how long a
	// settled request stays in an Agents listing. Zero keeps the product
	// window.
	AgentSettledWindow time.Duration
	// TrackerPollInterval replaces a tracker connection's poll interval
	// (trackerapp.JiraPollEvery, LinearPollEvery): a shown board's delta
	// and the open tickets' round, the two cadences that interval sets, so
	// a spec sees a change made in the fake tracker, or a hidden board's
	// absence of reads, without waiting out ten seconds. Reconcile and
	// structure ride the delta's tick at their own times, and the linked
	// issues loop and the retry base are not the poll. Zero keeps the
	// product interval.
	TrackerPollInterval time.Duration
	// TrackerApprovalWait replaces trackerApprovalWait, how long a tracker
	// write or time post waits for the person, so a spec sees a wait end
	// without waiting out fifteen minutes. Zero keeps the product wait.
	TrackerApprovalWait time.Duration
	// OmitOwnerOps makes this computer answer agent_owner_op as a build
	// before owner operations does, an unknown write call, so a paired
	// spawner's archive or delete names it as running an older version.
	// Only a harness boot sets it (diagenv.HarnessOlderComputer).
	OmitOwnerOps bool
	// OmitAgentMessageRunMembers makes this computer group every history
	// read by store.AgentMessagesAbsorbed whatever the request asks, as a
	// build before transport.CapabilityAgentMessageRunMembers does; its
	// transport leaves that capability out of the hello
	// (transport.Config.OmitAgentMessageRunMembers). Only a harness boot
	// sets it (diagenv.HarnessOlderComputer).
	OmitAgentMessageRunMembers bool
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
	a.retentionSweepDisabled = config.DisableRetentionSweep
	a.browser.mockEngine = config.MockBrowserEngine
	a.isolatedWorkspaceRoot = config.WorkspaceRoot
	a.forgeCLIs = isolatedForgeCLIs{isolated: true, fake: config.ForgeCLI}
	a.trackerOriginOverride = config.TrackerOrigin
	if a.trackerOriginOverride == nil {
		a.trackerOriginOverride = refuseTrackerOrigin
	}
	a.prLookupRetryBase = config.PRLookupRetryBase
	a.trackerCadence.Delta, a.trackerCadence.Ticket = config.TrackerPollInterval, config.TrackerPollInterval
	a.preview.scanScope = isolatedScanScope(config.ScanScopePIDs)
	a.downloadsIsolated = true
	a.threadPollOverride = config.ThreadRequestPoll
	a.ownerOpsOmitted = config.OmitOwnerOps
	a.agentMessageRunMembersOmitted = config.OmitAgentMessageRunMembers
	a.usageProbeGap = config.UsageProbeGap
	a.usagePollInterval = config.UsagePollInterval
	a.transferPendingRetry = config.TransferPendingRetry
	a.prUpdates.interval = config.PRUpdateInterval
	a.prUpdates.retryBase = config.PRUpdateRetryBase
	a.prUpdates.ciLiveInterval = config.PRCILiveInterval
	a.prUpdates.ciFollowInterval = config.PRCIFollowInterval
	a.prUpdates.ciLogWaitInterval = config.PRCILogWaitInterval
	a.idleReap = idleReapTiming{
		threshold:   config.IdleReapThreshold,
		interval:    config.IdleReapInterval,
		wakeupGrace: config.WakeupReapGrace,
	}
	a.agentWakes.floorOverride = config.AgentWakeFloor
	a.agentWakes.failures.windowOverride = config.AgentWatchFailureWindow
	a.remoteWatchPoll = config.RemoteWatchPoll
	a.wakeRetryBaseOverride = config.ThreadWakeRetryBase
	a.agentSettledWindowOverride = config.AgentSettledWindow
	a.trackerMCP.approvals.wait = config.TrackerApprovalWait
	// Unset keeps what the App has: a test fixture's own budget.
	if config.ProviderEventDrainStall > 0 {
		a.providerEvents.drainStall = config.ProviderEventDrainStall
	}
	a.mergedPRs.interval = config.MergedPRPollInterval
	// The mock provider's credential carries a mock bearer. The Claude usage
	// probe would send it to the real API: on hosts without network
	// isolation (macOS, Windows) that is an outbound call from a test run.
	// Usage is answered only for an identity a harness sets a reading for.
	a.claudeUsageFake = claudefake.NewUsage()
	a.rateLimitProbeClientOverride = &http.Client{
		Transport: isolatedProviderTransport{claudeUsage: a.claudeUsageFake},
	}
	// Provider update checks and release notes read the release hosts,
	// answered in process by a feed that advertises the mock provider's own
	// version until a harness seeds a later one.
	a.providerUpdateFeed = updatefake.New()
	a.providerUpdateClientOverride = isolatedProviderUpdateClient(a.providerUpdateFeed)
	// The user's OpenAI key file lives under the test's own data root, and
	// what it would send to OpenAI is answered in process.
	a.openAIClientOverride = isolatedHTTPClient
	// A sign-in or link opened from a loopback page would otherwise start the
	// developer's own browser.
	a.externalURLsIsolated = true
	// Outside the test network namespace the LAN is the developer's: a LAN
	// bind stays on loopback and no multicast is sent (network.Reach).
	a.netReach = network.IsolatedReach()
}

// errIsolatedNetwork refuses a request an isolated boot would otherwise send
// to a real service.
var errIsolatedNetwork = errors.New("isolated boot: outbound provider requests are disabled")

var isolatedHTTPClient = &http.Client{Transport: isolatedProviderTransport{}}

// isolatedProviderUpdateClient answers the provider release hosts from feed
// and refuses everything else isolatedProviderTransport refuses.
func isolatedProviderUpdateClient(feed *updatefake.Feed) *http.Client {
	return &http.Client{Transport: isolatedProviderTransport{providerUpdates: feed}}
}

// isolatedProviderTransport refuses every request except Claude's OAuth
// profile, the two transcriptions and OpenAI speech, which it answers
// in-process so account identification, dictation and read aloud run end to
// end through the real clients. The profile is claudefake's. Claude usage is
// answered from claudeUsage for an identity it holds a reading for and
// refused otherwise. Codex transcription names the bytes and type it
// received, and an access token of "refused" is answered as an expired
// login; OpenAI live transcription and speech are openaifake's. The
// provider release hosts are answered by providerUpdates when it is set.
type isolatedProviderTransport struct {
	claudeUsage     *claudefake.Usage
	providerUpdates *updatefake.Feed
}

func (t isolatedProviderTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	if req.Body != nil {
		defer func() {
			if closeErr := req.Body.Close(); closeErr != nil && err == nil {
				resp, err = nil, fmt.Errorf("isolated boot: close request body: %w", closeErr)
			}
		}()
	}
	if claudefake.IsProfile(req) {
		return claudefake.ServeProfile(req)
	}
	if resp, ok, err := t.claudeUsage.Serve(req); ok {
		return resp, err
	}
	if resp, ok, err := t.providerUpdates.Serve(req); ok {
		return resp, err
	}
	if req.Method == http.MethodPost && req.URL.String() == codex.TranscribeURL {
		return fakeTranscription(req)
	}
	if openaifake.IsLiveTranscription(req) {
		return openaifake.ServeLiveTranscription(req)
	}
	if openaifake.IsSpeech(req) {
		return openaifake.ServeSpeech(req)
	}
	return nil, errIsolatedNetwork
}

func fakeTranscription(req *http.Request) (resp *http.Response, err error) {
	if req.Header.Get("Authorization") == "Bearer refused" {
		return fakeTranscriptionResponse(req, http.StatusUnauthorized, `{"detail":"token expired"}`), nil
	}
	if err := req.ParseMultipartForm(codexFakeTranscribeMemory); err != nil {
		return nil, fmt.Errorf("isolated transcription: %w", err)
	}
	file, header, err := req.FormFile("file")
	if err != nil {
		return nil, fmt.Errorf("isolated transcription: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			resp, err = nil, fmt.Errorf("isolated transcription: close recording: %w", closeErr)
		}
	}()
	body, err := json.Marshal(map[string]string{
		"text": fmt.Sprintf("Heard %d bytes of %s", header.Size, header.Header.Get("Content-Type")),
	})
	if err != nil {
		return nil, err
	}
	return fakeTranscriptionResponse(req, http.StatusOK, string(body)), nil
}

// codexFakeTranscribeMemory holds the largest recording in memory.
const codexFakeTranscribeMemory = dictation.MaxRecordingBytes + 1<<20

func fakeTranscriptionResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     strconv.Itoa(status) + " " + http.StatusText(status),
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
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
	return network.FromServer(srv, a.persistedNetworkSettings(), a.netReach)
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

// RefusePendingMigrations makes Start fail with a store.MigrationsPendingError,
// before anything writes the database, when an existing database has
// migrations to apply. The Windows launcher's backend runs with it: a
// database is migrated only by a trial that snapshots it first
// (docs/specs/app-update.md, the no-live-migration rule). Call before Start.
func RefusePendingMigrations(a *App) { a.refusePendingMigrations = true }

// SetCertFingerprint installs the fingerprint of the TLS certificate the
// transport terminates with, which every pairing link then carries. The
// boot resolves the certificate and the listener from one value, so the
// string a device is told to pin is the string that listener presents.
// Call before the transport serves.
func SetCertFingerprint(a *App, fingerprint string) { a.certFingerprint = fingerprint }

// ComputerRoutes observes this computer's reachable listeners for its trusted
// bootstrap manifest. It is boot wiring, not a separately callable RPC.
func ComputerRoutes(a *App) []computerroute.Route {
	srv := a.transportServer.Load()
	if srv == nil {
		return nil
	}
	return network.ComputerRoutes(srv, a.persistedNetworkSettings(), a.netReach)
}

// NetworkReach is where this boot's LAN bind listens and which address it
// publishes; the executable applies it to the initial bind.
func NetworkReach(a *App) network.Reach { return a.netReach }

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

// ForgeAPI is the fake forge an isolated boot's forge API transport talks
// to: the base URL of the harness's forge listener and the fixed token it
// accepts (forgeapi.Isolated).
type ForgeAPI struct {
	BaseURL string
	Token   string
}

// SetForgeAPI installs the fake forge listener beside the ForgeCLI pin
// before Start. The listener starts after ConfigureIsolation, so it
// arrives separately; an isolated boot without one refuses every forge
// API request. A desktop boot ignores it.
func SetForgeAPI(a *App, api ForgeAPI) { a.forgeCLIs.api = api }

// SetProviderExtraEnv installs mock-control credentials before Start. Copying
// makes the write-once boot input independent of the control server's map.
func SetProviderExtraEnv(a *App, env map[string]string) {
	a.providerExtraEnv = maps.Clone(env)
}

// ConfigureBackendShutdown installs the process-exit path ShutdownBackend
// asks for. Call before Start.
//
// Only a boot whose shell IS a signal wait installs one, and it installs
// its own signal: the ordered teardown a graceful exit needs (App.Shutdown,
// the transport drain, the boot's own deferred cleanup) already hangs off
// that wait, and a second path into it would be a second teardown order to
// keep correct. Root owns process exit; this package only asks for it.
func ConfigureBackendShutdown(a *App, request func() error) { a.backendShutdown = request }

// SetBootProgress installs the sink App.Start reports its phases to. Call
// before Start.
func SetBootProgress(a *App, progress BootProgress) { a.bootProgress = progress }

// SetStartDone installs what the desktop boot does when the App.Start that
// ServiceStartup runs on its own goroutine returns: done(nil) after a
// successful start, done(err) after a failed one. A Start that
// ServiceShutdown canceled calls nothing. Call before the Wails
// application runs.
func SetStartDone(a *App, done func(error)) { a.startDone = done }

// UpdateBootFollows is the install the WSL boot's update marker named and
// whether it applied (appupdate.Service.UpdateThisBootFollows); "" before
// InitWSLUpdater or without one.
func UpdateBootFollows(a *App) (to string, applied bool) {
	return a.updater.UpdateThisBootFollows()
}

// EndUpdateRestart ends the timing of a restart to update that took the
// framework's swap (appupdate.Service.EndRestart): call once the process
// has shut down, with the error that ends it, if any.
func EndUpdateRestart(a *App, err error) {
	a.updater.EndRestart(err)
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
