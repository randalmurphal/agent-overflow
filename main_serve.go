package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"time"

	appservice "agent-overflow/internal/app"
	"agent-overflow/internal/network"
	"agent-overflow/internal/supervise"
)

// The `serve` boot: a backend with no window, left running on a machine
// its owner reaches from somewhere else (docs/specs/remote-access.md §7,
// "Headless serve mode and remote update"). Operator-facing walkthrough:
// docs/architecture/serve-mode.md.
//
// Why the verb is a BOOT MODE and not an aocli command is argued at
// serveVerb in main_entry.go. Why it is a separate function from
// runHeadless is argued at runServe.

// checkBackendVerbFlags refuses the boot flags that name a DIFFERENT mode.
//
// `serve` (and `supervise`, which starts one) is the mode the operator
// typed, so a flag that would have selected another one is a
// contradiction rather than a preference. Letting the mode switch quietly
// pick a winner is how somebody ends up debugging a backend they did not
// ask for.
//
// Three flags survive on purpose, because all three configure THIS boot:
// --listen (the bind), --data-dir (the root), and --reset-transport-port
// (drop the pinned port and take a fresh one).
//
// The launcher-identity flags need no row here. parseFlags already ties
// every one of them — --autopilot, --isolated-profile, --launcher-pid and
// the rest of the identity set — to --soak, which this refuses, so they
// are covered transitively and a copy of that rule would be a second
// place to keep it true.
//
// The verb is a parameter rather than the literal "serve" so the two
// modes share one list. A supervisor hands its flags straight to the
// child it spawns, so a flag it accepted and serve refused would be a
// unit that starts, fails, and restarts on a loop.
func checkBackendVerbFlags(verb string, flags cliFlags) error {
	switch {
	case flags.frontend:
		return fmt.Errorf("cannot combine %s with --frontend: a frontend starts no execution backend", verb)
	case flags.connect != "":
		return fmt.Errorf("cannot combine %s with --connect: %s IS a backend, and --connect attaches to somebody else's", verb, verb)
	case flags.headless:
		return fmt.Errorf("cannot combine %s with --print-url-fd: that bootstrap channel belongs to the Windows launcher, and %s prints its endpoints to stdout as text", verb, verb)
	case flags.harness:
		return fmt.Errorf("cannot combine %s with --harness: the harness is its own backend shell over mock providers", verb)
	case flags.soak:
		return fmt.Errorf("cannot combine %s with --soak: the soak shell is its own backend over mock providers", verb)
	case flags.window:
		return fmt.Errorf("cannot combine %s with --window: %s never opens a window, which is what it is for", verb, verb)
	case flags.mockProvider != "":
		return fmt.Errorf("cannot combine %s with --mock-provider: mock providers belong to --harness and --soak", verb)
	}
	return nil
}

// runServe boots the windowless backend and runs until it is signalled.
//
// It is runHeadless's shape — transport, App.Start, signal wait, ordered
// shutdown — with five differences, and each one is the reason this is not
// a parameter on that function:
//
//  1. The persisted network preferences apply. A serve host's bind is
//     configuration somebody saved in Settings → Remote access, not a launcher's
//     per-spawn argument. An explicit --listen still wins, because naming
//     an address on the command line is an override on purpose.
//  2. There is no bootstrap fd channel. Nobody parses this process's
//     stdout — a person reads it — so the endpoints are printed as text
//     through the same formatter Settings → Remote access reads
//     (app.ServeEndpoints), and stdout stays open for the rest of the run.
//  3. Provider credentials live in files, not a keychain. app.UseFileKeychain
//     carries the argument.
//  4. A first boot with nothing paired turns the console into the owner
//     surface (main_serve_enroll.go).
//  5. The browser engine is asked for by name. Every other windowless boot
//     shares this one's absent window and must keep getting no engine, so
//     "launch a headless Chromium" is a request only this mode makes
//     (app.UseHeadlessBrowserEngine).
func runServe(flags cliFlags) {
	// Before the App exists: a supervisor's opening frame decides whether
	// this boot is a TRIAL, and a trial boots with its activation gate
	// already closed. Absent a supervisor this is nil and nothing below it
	// changes. See main_serve_supervisor.go.
	supervisor, err := attachServeSupervisor()
	if err != nil {
		// A marker with a broken channel behind it means the spawn is wrong,
		// not that there is no supervisor. Booting anyway would run the
		// unattended set on what may be a trial.
		fatalf("serve: %v", err)
	}

	appService := newApp()
	// Before Start, which is where initStores decides which credential
	// backend to build.
	appservice.UseFileKeychain(appService.App)
	// Also before Start, which is where the browser Manager picks its engine.
	// This mode opens no window, so the platform engines cannot run here —
	// but the browser TOOLS still can, over a headless Chromium this backend
	// launches itself (docs/specs/embedded-browser.md §2, and the operator
	// walkthrough in docs/architecture/serve-mode.md § Browser tools). It is
	// a request, not a promise: the browser has to already be installed,
	// because nothing is ever downloaded.
	appservice.UseHeadlessBrowserEngine(appService.App)
	configureServeSupervision(appService, supervisor)
	// Same reason runHeadless does it here: the updater RPC handlers must
	// see a fully wired App before the transport can dispatch to them.
	// Runtime-gated on the Windows launcher having spawned us, so on a
	// serve host it is a no-op.
	appservice.InitWSLUpdater(appService.App, bootSettingsDir())

	srv := bootTransport(appService, flags.listenAddr, bootTransportOptions{
		BackendLockHeldBySupervisor: supervisor != nil && supervisor.ownsDataRoot,
		// A browser pointed at a serve host must not load the SPA against a
		// backend whose store is not open yet: /bootstrap.json answers 503
		// until MarkReady, and the page retries. The desktop boot can skip
		// this because the window it opens is under the same process's
		// control; a remote browser is not.
		RequireReadyForBootstrap: true,
		// The whole point of this mode. --listen still overrides the bind
		// inside bootTransport; the canonical domain applies either way.
		LoadPersistedNetwork: true,
	})
	appservice.ConfigureTransportNotifications(appService.App)
	// The bus exists now, so the boot's update check can say its piece to a
	// client that connects immediately.
	appservice.NotifyPendingUpdateApplyFailure(appService.App)
	log.Printf("transport: serve mode")

	bootCtx, bootCancel := context.WithCancel(context.Background())
	defer bootCancel()
	phaseStarted := time.Now()
	if err := appService.Start(bootCtx); err != nil {
		logBootPhase("serve.service_startup", phaseStarted)
		log.Printf("app: service startup: %v", err)
		srv.MarkStartupFailed()
		// Deliberately not exiting: the transport is bound and answers
		// every bootstrap request with a terminal failure that names what
		// happened. Under a service manager, exiting here would restart
		// into the same failure on a loop, and nobody would ever read the
		// reason.
		log.Printf("serve: startup failed; serving terminal bootstrap failure until shutdown")
		if waitForHeadlessShutdownOrRestart(appService, srv, supervisor.restartRequested()) {
			os.Exit(supervise.RestartForUpdateExitCode)
		}
		return
	}
	logBootPhase("serve.service_startup", phaseStarted)
	srv.MarkReady()

	printServeEndpoints(os.Stdout, appservice.ServeEndpoints(appService.App, srv), srv.Addr())

	// The store is open, the migrations ran and the listener answers: this is
	// exactly the state "prepared" names, so report it.
	//
	// On its own goroutine because a trial's report is followed by an
	// unbounded wait for the commit, and the thing that ENDS that wait in the
	// failing case is the SIGTERM the supervisor sends when it rolls back —
	// which only arrives somewhere if the signal wait below is live. A trial
	// that blocked the main goroutine here could not be stopped.
	go finishServeSupervision(appService, supervisor)

	// Enrollment runs on its own goroutine because it needs the server to
	// be SERVING while it waits: the operator loads the pairing link in a
	// browser, and the redemption that produces a verification number is a
	// request this process has to answer. The signal wait stays on the main
	// goroutine, so Ctrl-C during enrollment shuts the backend down the same
	// way it would at any other moment.
	go runServeEnrollment(bootCtx, defaultServeConsole(), appEnrollment{app: appService}, serveEnrollmentPoll)

	if waitForHeadlessShutdownOrRestart(appService, srv, supervisor.restartRequested()) {
		os.Exit(supervise.RestartForUpdateExitCode)
	}
}

// printServeEndpoints writes the addresses a person needs to reach this
// backend.
//
// Ordered by what the reader does with it: the bound address first (the
// fact), then public addresses and the pairing command. Neither launch
// credentials nor ticket-bearing share URLs belong in service logs.
//
// The tailnet URL is usually absent at this moment even when the node is
// enabled: bring-up is asynchronous and a first sign-in is interactive, so
// the line appears only when the node is already up. Settings → Remote access is
// where a person watches that finish.
func printServeEndpoints(w io.Writer, endpoints network.Settings, addr string) {
	fmt.Fprintf(w, "Agent Overflow is serving on %s\n", addr)
	if endpoints.URL != "" {
		fmt.Fprintf(w, "  Open:    %s\n", publicServeAddress(endpoints.URL))
	}
	if endpoints.Tailnet.URL != "" {
		fmt.Fprintf(w, "  Tailnet: %s\n", publicServeAddress(endpoints.Tailnet.URL))
	}
	fmt.Fprintln(w, "  Pair a device: agent-overflow pair (add --lan to enable LAN access)")
	if endpoints.Insecure {
		fmt.Fprintln(w, "  Browser access over http crosses the network in the clear. Use HTTPS or a tailnet for browser pairing.")
	}
}

func publicServeAddress(raw string) string {
	address, err := url.Parse(raw)
	if err != nil {
		return "address unavailable"
	}
	address.User = nil
	address.RawQuery = ""
	address.Fragment = ""
	return address.String()
}

// serveEnrollmentPoll is how often the console re-reads a pending link
// while it waits for a device to redeem it. Two seconds: the operator is
// switching to another device and back, the read is one indexed row, and a
// tighter loop would only make the log noisier if it ever failed.
const serveEnrollmentPoll = 2 * time.Second

// appEnrollment is the production serveEnrollment, over the very calls the
// settings screen makes. No new App surface and no relaxed rule: an
// in-process call carries no session context, and app_authz.go admits that
// caller by construction — it is the host-present caller these methods'
// step-up requirement already describes. The operator holding a TTY on the
// machine IS standing at it.
type appEnrollment struct{ app *App }

func (e appEnrollment) Enrolled() (bool, error) {
	return appservice.HasEnrolledDevice(e.app.App)
}

// Mint asks for a browser-class device with full access.
//
// BROWSER because that is what a person enrolling the first device on a
// headless host is holding: a phone or a laptop, opening the link. It is
// also the conservative choice of the two plausible classes — identity's
// PolicyFor gives a browser the shorter access and refresh windows than a
// desktop — so a link left unattended on a console costs less.
//
// FULL access because this is the OWNER's first device on a backend with
// no other way in. A view-only first device would leave the machine unable
// to enroll a second one. Narrowing a later device is offered per-device
// in Settings → Access, which is where that choice belongs.
func (e appEnrollment) Mint() (appservice.PairingInvite, error) {
	return e.app.MintDevicePairing(serveEnrollmentDeviceClass, serveEnrollmentAccess)
}

func (e appEnrollment) Status(linkID string) (appservice.PairingStatusView, error) {
	return e.app.DevicePairingStatus(linkID)
}

func (e appEnrollment) Confirm(linkID string) error { return e.app.ConfirmDevicePairing(linkID) }

func (e appEnrollment) Cancel(linkID string) error { return e.app.CancelDevicePairing(linkID) }

var _ serveEnrollment = appEnrollment{}
