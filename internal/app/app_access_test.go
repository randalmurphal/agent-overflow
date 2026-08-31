package app

import (
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/store"
)

// accessApp is identityApp plus the two things the device-access surface
// reads outside the session core: a running transport (for the page URL a
// pairing link points at, and the live connection counts) and the store's
// published backend identity (which every minted payload names).
func accessApp(t *testing.T) *App {
	t.Helper()
	app := identityApp(t)
	app.SetTransportServer(startTestTransportServer(t))
	id, err := app.store.Identity()
	if err != nil {
		t.Fatalf("store identity: %v", err)
	}
	app.storeIdentity.Store(&id)
	return app
}

// findDevice picks one device out of an overview by label.
func findDevice(t *testing.T, overview AccessOverview, label string) AccessDevice {
	t.Helper()
	for _, device := range overview.Devices {
		if device.Label == label {
			return device
		}
	}
	t.Fatalf("no device labelled %q in %d rows", label, len(overview.Devices))
	return AccessDevice{}
}

// mintLink mints through the RPC and returns the link id plus the token
// the payload carries, which is what a redeeming device presents.
func mintLink(t *testing.T, app *App, class identity.DeviceClass) (linkID, token string) {
	t.Helper()
	invite, err := app.MintDevicePairing(string(class))
	if err != nil {
		t.Fatalf("MintDevicePairing(%s): %v", class, err)
	}
	_, encoded, found := strings.Cut(invite.URL, pairingFragmentPrefix)
	if !found {
		t.Fatalf("the invite URL carries no pairing fragment: %s", invite.URL)
	}
	payload, err := identity.DecodePairingPayload(encoded)
	if err != nil {
		t.Fatalf("decode the pairing payload: %v", err)
	}
	return invite.LinkID, payload.Token
}

// redeem presents a link the way the device side does.
func redeem(t *testing.T, app *App, token, label, thumbprint string) identity.Redemption {
	t.Helper()
	redemption, reason := app.identityState().sessions.RedeemPairing(identity.RedemptionRequest{
		Token: token, KeyThumbprint: thumbprint, Label: label, Platform: "linux",
	})
	if reason.Refused() {
		t.Fatalf("RedeemPairing: %s", reason.Code())
	}
	return redemption
}

// TestGetAccessOverview_ListsWhatReachesThisBackend is the settings pane
// in one read: which devices hold credentials, the sessions they are
// holding, and the credential log that explains how they got them.
func TestGetAccessOverview_ListsWhatReachesThisBackend(t *testing.T) {
	app := accessApp(t)
	paired, session := pairDevice(t, app, "A browser", "thumb-browser")
	local := localChannelSession(t, app)

	overview, err := app.GetAccessOverview()
	if err != nil {
		t.Fatalf("GetAccessOverview: %v", err)
	}

	device := findDevice(t, overview, "A browser")
	if device.ID != paired.ID {
		t.Fatalf("device id = %q, want %q", device.ID, paired.ID)
	}
	if device.Class != string(identity.DeviceBrowser) {
		t.Errorf("class = %q, want %q", device.Class, identity.DeviceBrowser)
	}
	if device.Channel != "" {
		t.Errorf("a paired device carries channel %q, want empty", device.Channel)
	}
	if device.RevokedAtMs != 0 {
		t.Errorf("a live device reports revokedAtMs = %d", device.RevokedAtMs)
	}
	if len(device.Sessions) != 1 || device.Sessions[0].ID != session.ID {
		t.Fatalf("device sessions = %+v, want just %q", device.Sessions, session.ID)
	}
	if device.Sessions[0].AwaitingConfirmation {
		t.Error("a confirmed session reports awaitingConfirmation")
	}
	if device.Sessions[0].Binding != string(identity.BindingDeviceBound) {
		t.Errorf("binding = %q, want device-bound", device.Sessions[0].Binding)
	}
	if device.Sessions[0].Connections != 0 {
		t.Errorf("connections = %d with nothing attached", device.Sessions[0].Connections)
	}

	// The local page channel is a device row too, and the surface has to be
	// able to tell it apart — it is the one row a revoke control must not
	// offer, because revoking it would sign this window out.
	var localDevice AccessDevice
	for _, candidate := range overview.Devices {
		if candidate.ID == local.DeviceID {
			localDevice = candidate
		}
	}
	if localDevice.Channel != identity.LocalChannel {
		t.Fatalf("the local page channel device reports channel %q", localDevice.Channel)
	}

	if len(overview.Audit) == 0 {
		t.Fatal("the credential log is empty after a whole pairing exchange")
	}
	// Newest first, which is what a log a person reads top-down needs.
	for i := 1; i < len(overview.Audit); i++ {
		if overview.Audit[i-1].AtMs < overview.Audit[i].AtMs {
			t.Fatalf("audit row %d is older than the one before it", i)
		}
	}
}

// TestGetAccessOverview_CarriesNoDeadSessions — a device that has
// reconnected for months holds a long tail of expired rows, and the audit
// log is where that history already lives. The list is what a person can
// act on.
func TestGetAccessOverview_CarriesNoDeadSessions(t *testing.T) {
	app := accessApp(t)
	device, session := pairDevice(t, app, "A browser", "thumb-browser")

	if _, err := app.identityState().sessions.RevokeSession(session.ID); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	overview, err := app.GetAccessOverview()
	if err != nil {
		t.Fatalf("GetAccessOverview: %v", err)
	}
	listed := findDevice(t, overview, "A browser")
	if listed.ID != device.ID {
		t.Fatalf("device id = %q, want %q", listed.ID, device.ID)
	}
	if len(listed.Sessions) != 0 {
		t.Fatalf("a revoked session is still listed: %+v", listed.Sessions)
	}
}

// TestMintDevicePairing_HandsTheDeviceALoadablePageAndAFragmentPayload
// pins the whole link shape. The page ticket is what lets a device holding
// no credential load the page at all; the payload rides the FRAGMENT,
// which is never sent to a server, never written to an access log, and
// never lands in a Referer header.
func TestMintDevicePairing_HandsTheDeviceALoadablePageAndAFragmentPayload(t *testing.T) {
	app := accessApp(t)

	invite, err := app.MintDevicePairing(string(identity.DevicePhone))
	if err != nil {
		t.Fatalf("MintDevicePairing: %v", err)
	}
	if invite.LinkID == "" {
		t.Fatal("the invite names no link")
	}
	if invite.ExpiresAtMs <= app.identityState().sessions.Now() {
		t.Fatalf("a freshly minted link expires at %d, which is not in the future", invite.ExpiresAtMs)
	}

	base, encoded, found := strings.Cut(invite.URL, pairingFragmentPrefix)
	if !found {
		t.Fatalf("the invite URL carries no pairing fragment: %s", invite.URL)
	}
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse the page URL: %v", err)
	}
	if parsed.Query().Get("t") == "" {
		t.Errorf("the page URL carries no one-time ticket: %s", base)
	}
	// No `cid`, deliberately: that is this install's durable UI-state
	// identity, and stamping it on a link for somebody else's phone would
	// point that phone at this machine's bucket.
	if parsed.Query().Has("cid") {
		t.Errorf("the pairing URL carries this install's client id: %s", base)
	}

	payload, err := identity.DecodePairingPayload(encoded)
	if err != nil {
		t.Fatalf("decode the pairing payload: %v", err)
	}
	backendID, _ := app.backendIdentity()
	if payload.BackendID != backendID {
		t.Errorf("payload backendId = %q, want %q", payload.BackendID, backendID)
	}
	if payload.Endpoint != parsed.Scheme+"://"+parsed.Host {
		t.Errorf("payload endpoint = %q, want the page URL's own origin %q://%q",
			payload.Endpoint, parsed.Scheme, parsed.Host)
	}
	if payload.Token == "" {
		t.Fatal("the payload carries no token")
	}
	// The token is the whole point: it has to redeem, and the class the
	// minting surface chose is the class the device gets.
	redemption := redeem(t, app, payload.Token, "A phone", "thumb-phone")
	device, err := app.store.GetDevice(redemption.DeviceID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if device.Class != string(identity.DevicePhone) {
		t.Errorf("redeemed device class = %q, want the minted %q", device.Class, identity.DevicePhone)
	}
}

// TestMintDevicePairing_RefusesAClassItDoesNotPair — a peer backend is
// enrolled through the federation flow with its own trust decisions;
// admitting one here would give it the posture of an owner's own device.
func TestMintDevicePairing_RefusesAClassItDoesNotPair(t *testing.T) {
	app := accessApp(t)

	for name, class := range map[string]string{
		"undeclared":   "watch",
		"empty":        "",
		"backend peer": string(identity.DeviceBackendPeer),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := app.MintDevicePairing(class); err == nil {
				t.Fatalf("MintDevicePairing(%q) minted a link", class)
			}
		})
	}
}

// TestDevicePairingStatus_WalksTheExchange pins the state machine the
// polling surface renders, including the ordering rule that matters: a
// redeemed link outlives its own five-minute window, because redemption
// starts the owner's ten-minute confirmation window and reporting
// "expired" would take away the step the exchange is waiting on.
func TestDevicePairingStatus_WalksTheExchange(t *testing.T) {
	app := accessApp(t)
	linkID, token := mintLink(t, app, identity.DevicePhone)

	pending, err := app.DevicePairingStatus(linkID)
	if err != nil {
		t.Fatalf("DevicePairingStatus: %v", err)
	}
	if pending.State != pairingStatePending {
		t.Fatalf("state = %q, want pending", pending.State)
	}
	if pending.VerificationNumber != "" || pending.DeviceLabel != "" {
		t.Fatalf("an unredeemed link named a device: %+v", pending)
	}

	redemption := redeem(t, app, token, "A phone", "thumb-phone")
	redeemed, err := app.DevicePairingStatus(linkID)
	if err != nil {
		t.Fatalf("DevicePairingStatus after redemption: %v", err)
	}
	if redeemed.State != pairingStateRedeemed {
		t.Fatalf("state = %q, want redeemed", redeemed.State)
	}
	if redeemed.DeviceLabel != "A phone" {
		t.Errorf("deviceLabel = %q, want the redeeming device's", redeemed.DeviceLabel)
	}
	// The number the owner compares is the one the device was handed, and
	// it is derived from THAT device's key: a different device redeeming
	// first produces a different number, which is what closes the race.
	if redeemed.VerificationNumber != redemption.VerificationNumber {
		t.Fatalf("the surface shows %q and the device was told %q",
			redeemed.VerificationNumber, redemption.VerificationNumber)
	}
	// The redeemed link's deadline is the confirmation window, not the
	// link row's own expiry, so the confirm affordance survives.
	link, err := app.store.GetPairingLink(linkID)
	if err != nil {
		t.Fatalf("GetPairingLink: %v", err)
	}
	if redeemed.ExpiresAtMs != link.RedeemedAt+identity.PairingConfirmWindow.Milliseconds() {
		t.Fatalf("a redeemed link reports deadline %d, want the confirmation window", redeemed.ExpiresAtMs)
	}
	if redeemed.ExpiresAtMs <= link.ExpiresAt {
		t.Fatal("the confirmation window does not outlive the link's own expiry")
	}

	// It is also still in the pending list, which is where the confirm
	// control lives.
	overview, err := app.GetAccessOverview()
	if err != nil {
		t.Fatalf("GetAccessOverview: %v", err)
	}
	if len(overview.PendingPairings) != 1 {
		t.Fatalf("pending pairings = %+v, want the redeemed link", overview.PendingPairings)
	}
	if !overview.PendingPairings[0].Redeemed {
		t.Error("the pending row does not report the redemption")
	}
	if overview.PendingPairings[0].VerificationNumber != redemption.VerificationNumber {
		t.Errorf("the pending row shows number %q, want %q",
			overview.PendingPairings[0].VerificationNumber, redemption.VerificationNumber)
	}

	if err := app.ConfirmDevicePairing(linkID); err != nil {
		t.Fatalf("ConfirmDevicePairing: %v", err)
	}
	confirmed, err := app.DevicePairingStatus(linkID)
	if err != nil {
		t.Fatalf("DevicePairingStatus after confirm: %v", err)
	}
	if confirmed.State != pairingStateConfirmed {
		t.Fatalf("state = %q, want confirmed", confirmed.State)
	}
	// A settled link leaves the list nothing to act on.
	overview, err = app.GetAccessOverview()
	if err != nil {
		t.Fatalf("GetAccessOverview: %v", err)
	}
	if len(overview.PendingPairings) != 0 {
		t.Fatalf("a confirmed link is still pending: %+v", overview.PendingPairings)
	}
	// And the session it minted is live.
	session, err := app.store.GetSession(redemption.Tokens.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !session.Live(app.identityState().sessions.Now()) {
		t.Fatal("confirming the pairing left the session unusable")
	}
}

// TestCancelDevicePairing_SettlesTheLinkAndTakesTheSessionWithIt — the
// number did not match, or the link was minted by mistake. Whatever the
// redemption already created goes with it.
func TestCancelDevicePairing_SettlesTheLinkAndTakesTheSessionWithIt(t *testing.T) {
	app := accessApp(t)
	linkID, token := mintLink(t, app, identity.DevicePhone)
	redemption := redeem(t, app, token, "A phone", "thumb-phone")

	if err := app.CancelDevicePairing(linkID); err != nil {
		t.Fatalf("CancelDevicePairing: %v", err)
	}
	status, err := app.DevicePairingStatus(linkID)
	if err != nil {
		t.Fatalf("DevicePairingStatus: %v", err)
	}
	if status.State != pairingStateCanceled {
		t.Fatalf("state = %q, want canceled", status.State)
	}
	session, err := app.store.GetSession(redemption.Tokens.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.RevokedAt == 0 {
		t.Fatal("cancelling the pairing left the session it minted alive")
	}
	if _, reason := app.identityState().sessions.Live(session.ID); !reason.Refused() {
		t.Fatal("the session core still admits the cancelled session")
	}
}

// TestConfirmAndCancelRefuseALinkTheyDoNotKnow — a caller cannot tell a
// link that never existed from one already settled, and neither should
// it: the difference is a fact about this backend's records.
func TestConfirmAndCancelRefuseALinkTheyDoNotKnow(t *testing.T) {
	app := accessApp(t)

	if err := app.ConfirmDevicePairing("no-such-link"); err == nil {
		t.Error("ConfirmDevicePairing confirmed a link that does not exist")
	}
	if err := app.CancelDevicePairing("no-such-link"); err == nil {
		t.Error("CancelDevicePairing cancelled a link that does not exist")
	}
	if _, err := app.DevicePairingStatus("no-such-link"); err == nil {
		t.Error("DevicePairingStatus answered for a link that does not exist")
	}
}

// TestRevokeAccessDevice_EndsEveryCredentialAndDropsTheDevicesUIState —
// revoking a device drops its state (docs/specs/remote-access.md §6). A
// device that came back paired must not inherit the view it had before
// somebody took its access away.
func TestRevokeAccessDevice_EndsEveryCredentialAndDropsTheDevicesUIState(t *testing.T) {
	app := accessApp(t)
	gone, goneSession := pairDevice(t, app, "The removed one", "thumb-gone")
	kept, keptSession := pairDevice(t, app, "The other one", "thumb-kept")

	for _, session := range []store.Session{goneSession, keptSession} {
		if err := app.SetUIState(sessionCtx(session.ID, ""),
			map[string]string{"sidebar:width": "312"}); err != nil {
			t.Fatalf("SetUIState: %v", err)
		}
	}

	if err := app.RevokeAccessDevice(gone.ID); err != nil {
		t.Fatalf("RevokeAccessDevice: %v", err)
	}

	revoked, err := app.store.GetDevice(gone.ID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if revoked.RevokedAt == 0 {
		t.Fatal("the device row carries no revocation")
	}
	if _, reason := app.identityState().sessions.Live(goneSession.ID); !reason.Refused() {
		t.Fatal("the revoked device's session still admits a presentation")
	}
	bucket, err := app.store.GetUIState("device:" + gone.ID)
	if err != nil {
		t.Fatalf("read the revoked device's bucket: %v", err)
	}
	if len(bucket) != 0 {
		t.Fatalf("the revoked device's ui state survived: %v", bucket)
	}

	// Nothing else moved.
	neighbour, err := app.store.GetUIState("device:" + kept.ID)
	if err != nil {
		t.Fatalf("read the neighbouring bucket: %v", err)
	}
	if neighbour["sidebar:width"] != "312" {
		t.Fatalf("a neighbouring device's ui state was disturbed: %v", neighbour)
	}
	if _, reason := app.identityState().sessions.Live(keptSession.ID); reason.Refused() {
		t.Fatalf("a neighbouring device's session was refused: %s", reason.Code())
	}

	// Idempotent: a second revocation of a device already gone is not an
	// error, because nothing about the outcome differs.
	if err := app.RevokeAccessDevice(gone.ID); err != nil {
		t.Fatalf("second RevokeAccessDevice: %v", err)
	}
}

// recordingConns is the transport's live-connection registry as the
// session core sees it, recording which sessions were force-closed.
type recordingConns struct {
	mu     sync.Mutex
	closed []string
}

func (r *recordingConns) CloseSession(sessionID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = append(r.closed, sessionID)
	return 1
}

func (r *recordingConns) sawClose(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range r.closed {
		if id == sessionID {
			return true
		}
	}
	return false
}

// TestRevokeReachesTheLiveSockets — "revoked" that has not reached a live
// socket is not revoked. A connection already upgraded holds its stream
// open on a credential it presented once, so the revocation has to close
// it rather than wait for a next presentation that never comes.
func TestRevokeReachesTheLiveSockets(t *testing.T) {
	app := accessApp(t)
	conns := &recordingConns{}
	AttachSessionConns(app, conns)

	device, session := pairDevice(t, app, "A browser", "thumb-browser")
	if err := app.RevokeAccessSession(session.ID); err != nil {
		t.Fatalf("RevokeAccessSession: %v", err)
	}
	if !conns.sawClose(session.ID) {
		t.Fatal("revoking a session left its live sockets open")
	}

	// A device-wide revocation reaches every session the device holds,
	// including one minted after the first was revoked — which is what a
	// device that reconnects on a fresh credential is.
	second := mintSessionFor(t, app, device)
	if err := app.RevokeAccessDevice(device.ID); err != nil {
		t.Fatalf("RevokeAccessDevice: %v", err)
	}
	if !conns.sawClose(second.ID) {
		t.Fatal("revoking a device left one of its sessions' sockets open")
	}
}

// TestRevokeReachesSocketsWhenTheTransportBootsFirst pins the attach
// against the PRODUCTION boot order: main.go hands the registry over at
// transport-construct time, before Start has run initIdentity. An attach
// that only completes through an already-booted core wires nothing on
// that order — which shipped, and which the live pairing exercise caught
// as a revocation that left the revoked device's socket streaming.
func TestRevokeReachesSocketsWhenTheTransportBootsFirst(t *testing.T) {
	app := newTestAppWithStore(t)
	conns := &recordingConns{}
	AttachSessionConns(app, conns) // transport first,
	app.initIdentity("backend-under-test") // identity second
	app.SetTransportServer(startTestTransportServer(t))
	id, err := app.store.Identity()
	if err != nil {
		t.Fatalf("store identity: %v", err)
	}
	app.storeIdentity.Store(&id)

	_, session := pairDevice(t, app, "A browser", "thumb-order")
	if err := app.RevokeAccessSession(session.ID); err != nil {
		t.Fatalf("RevokeAccessSession: %v", err)
	}
	if !conns.sawClose(session.ID) {
		t.Fatal("registry attached before the session core booted never reached it")
	}
}

// mintSessionFor issues a second session on an EXISTING device row.
func mintSessionFor(t *testing.T, app *App, device store.Device) store.Session {
	t.Helper()
	session, _, err := app.identityState().sessions.Mint(identity.MintRequest{
		UserID:       device.UserID,
		DeviceID:     device.ID,
		BindingClass: identity.BindingDeviceBound,
		Scopes:       identity.Scopes,
		TTL:          time.Hour,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return session
}

// TestRevokeAccessDevice_RefusesTheLocalPageChannel — that row is not a
// device somebody paired: it is this backend's own page channel, which the
// embedded webview, the WSL relay, and the `--connect` stub all present.
// Revoking it would sign the host's own window out.
func TestRevokeAccessDevice_RefusesTheLocalPageChannel(t *testing.T) {
	app := accessApp(t)
	local := localChannelSession(t, app)

	if err := app.RevokeAccessDevice(local.DeviceID); err == nil {
		t.Fatal("RevokeAccessDevice revoked this app's own page channel")
	}
	if err := app.RevokeAccessSession(local.ID); err == nil {
		t.Fatal("RevokeAccessSession revoked this app's own page channel session")
	}

	device, err := app.store.GetDevice(local.DeviceID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if device.RevokedAt != 0 {
		t.Fatal("the refusal still wrote a revocation")
	}
	if _, reason := app.identityState().sessions.Live(local.ID); reason.Refused() {
		t.Fatalf("the local channel session was refused after the refusal: %s", reason.Code())
	}
}

// TestRevokeAccessSession_EndsOneCredentialAndLeavesTheDevicePaired — the
// narrower control: sign one session out without un-pairing the device
// that holds it.
func TestRevokeAccessSession_EndsOneCredentialAndLeavesTheDevicePaired(t *testing.T) {
	app := accessApp(t)
	device, session := pairDevice(t, app, "A browser", "thumb-browser")

	if err := app.RevokeAccessSession(session.ID); err != nil {
		t.Fatalf("RevokeAccessSession: %v", err)
	}
	if _, reason := app.identityState().sessions.Live(session.ID); !reason.Refused() {
		t.Fatal("the revoked session still admits a presentation")
	}
	still, err := app.store.GetDevice(device.ID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if still.RevokedAt != 0 {
		t.Fatal("revoking one session un-paired the device that held it")
	}
	// And the device's own state stays: it is still a device this owner
	// paired, and it is expected back.
	if _, err := app.store.GetUIState("device:" + device.ID); err != nil {
		t.Fatalf("read the device bucket: %v", err)
	}
}

// TestRevokeRefusesRowsItCannotResolve — a revocation naming nothing must
// say so rather than reporting success over a no-op, because the surface
// would otherwise show a device as removed that is still connected.
func TestRevokeRefusesRowsItCannotResolve(t *testing.T) {
	app := accessApp(t)

	if err := app.RevokeAccessDevice("no-such-device"); err == nil {
		t.Error("RevokeAccessDevice reported success for a device that does not exist")
	}
	if err := app.RevokeAccessSession("no-such-session"); err == nil {
		t.Error("RevokeAccessSession reported success for a session that does not exist")
	}
}

// TestAccessSurfaceWithoutIdentity — identity not being wired is a state,
// not a fault, and every method has to say so rather than panicking on a
// nil session core.
func TestAccessSurfaceWithoutIdentity(t *testing.T) {
	app := newTestAppWithStore(t)
	if app.identityState() != nil {
		t.Fatal("this fixture booted a session core")
	}

	if _, err := app.GetAccessOverview(); err == nil {
		t.Error("GetAccessOverview answered with no session core")
	}
	if _, err := app.MintDevicePairing(string(identity.DevicePhone)); err == nil {
		t.Error("MintDevicePairing minted with no session core")
	}
	if _, err := app.DevicePairingStatus("any"); err == nil {
		t.Error("DevicePairingStatus answered with no session core")
	}
	if err := app.ConfirmDevicePairing("any"); err == nil {
		t.Error("ConfirmDevicePairing answered with no session core")
	}
	if err := app.CancelDevicePairing("any"); err == nil {
		t.Error("CancelDevicePairing answered with no session core")
	}
	if err := app.RevokeAccessDevice("any"); err == nil {
		t.Error("RevokeAccessDevice answered with no session core")
	}
	if err := app.RevokeAccessSession("any"); err == nil {
		t.Error("RevokeAccessSession answered with no session core")
	}
}

// TestMintDevicePairing_NeedsATransportToPointAt — a link naming no
// address is a link nothing can redeem, and minting one would spend a
// single-use token on a URL that goes nowhere.
func TestMintDevicePairing_NeedsATransportToPointAt(t *testing.T) {
	app := identityApp(t)
	if _, err := app.MintDevicePairing(string(identity.DevicePhone)); err == nil {
		t.Fatal("MintDevicePairing minted a link with no transport running")
	}
}
