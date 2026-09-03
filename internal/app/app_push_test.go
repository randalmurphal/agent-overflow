package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/push"
	"agent-overflow/internal/store"
)

// NOTHING HERE REACHES GOOGLE. Every case installs a fakePushSender, and the
// one case that builds a real FCMSender never sends through it — it asserts
// that a pasted key is accepted or refused on SHAPE, which is the whole of
// what SetPushSenderCredential is allowed to check.

type fakePushSender struct {
	mu   sync.Mutex
	sent []push.Message
	// answer is consulted per message by token, so one table can make one
	// device's registration dead while another's works.
	answer map[string]error
}

func (f *fakePushSender) Send(_ context.Context, message push.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, message)
	return f.answer[message.Token]
}

func (f *fakePushSender) messages() []push.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]push.Message(nil), f.sent...)
}

func (f *fakePushSender) tokens() []string {
	out := []string{}
	for _, message := range f.messages() {
		out = append(out, message.Token)
	}
	return out
}

// pushApp is an identity-wired, residency-wired App with a fake sender
// installed: everything the fan-out reads, and nothing that could reach a
// network.
func pushApp(t *testing.T) (*App, *fakePushSender) {
	t.Helper()
	app := withTierStore(t, identityApp(t))
	// The identity the fan-out stamps on every message, loaded the way
	// Start loads it: without it MessageFor refuses, by design.
	app.storeIdentity.Store(&store.Identity{BackendID: "backend-under-test"})
	sender := &fakePushSender{answer: map[string]error{}}
	app.installPushSender(sender, "project-under-test", "sender@project-under-test.iam")
	return app, sender
}

// pairPhone mints a paired phone session and registers a token through the
// real RPC, so the tests exercise the same door the shell does.
func pairPhone(t *testing.T, app *App, thumbprint, token string) store.Session {
	t.Helper()
	state := app.identityState()
	link, err := state.sessions.MintPairingLink(identity.PairingRequest{
		UserID:       state.owner.ID,
		DeviceClass:  identity.DevicePhone,
		BindingClass: identity.BindingDeviceBound,
		Scopes:       identity.Scopes,
	})
	if err != nil {
		t.Fatalf("MintPairingLink: %v", err)
	}
	redemption, reason := state.sessions.RedeemPairing(identity.RedemptionRequest{
		Token: link.Token, Proof: identity.DeviceProof{Value: thumbprint},
		Label: "a phone", Platform: "android",
	})
	if reason.Refused() {
		t.Fatalf("RedeemPairing: %s", reason.Code())
	}
	if _, err := state.sessions.ConfirmPairing(link.Link.ID); err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}
	session, err := app.store.GetSession(redemption.Tokens.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if token != "" {
		if err := app.RegisterPushToken(callFrom(session.ID, false), "android", token); err != nil {
			t.Fatalf("RegisterPushToken: %v", err)
		}
	}
	return session
}

// firedPush pushes one send through the fan-out and waits for its queue.
// The wait is the queue's own: the fan-out is asynchronous by contract, and
// a sleep would be asserting on a timer.
func firedPush(t *testing.T, app *App, send notify.Send) []push.Message {
	t.Helper()
	app.pushFanout(send)
	app.push.queue.Wait()
	sender := app.currentPushSender()
	fake, ok := sender.(*fakePushSender)
	if !ok {
		t.Fatalf("sender = %T, want the fake", sender)
	}
	return fake.messages()
}

func turnCompleteSend() notify.Send {
	return notify.Send{
		ID:     "thread:" + mappingThreadID,
		Kind:   notify.KindTurnComplete,
		Title:  "Turn complete",
		Body:   "Rewrite the parser",
		Target: notify.Target{Kind: "thread", ThreadID: mappingThreadID},
	}
}

func retractSend() notify.Send {
	return notify.Send{ID: "thread:" + mappingThreadID, Kind: notify.KindTurnComplete, Retract: true}
}

// The headline: one paired phone is woken, and what reaches Google is the
// kind's fixed phrase and the machine's name. NOT the thread title, which
// the desktop's own toast carries and this payload may not.
func TestAWokenPhoneIsToldTheKindAndTheMachineAndNothingElse(t *testing.T) {
	app, _ := pushApp(t)
	pairPhone(t, app, "thumb-phone", "token-phone")

	messages := firedPush(t, app, turnCompleteSend())
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages))
	}
	message := messages[0]
	if message.Token != "token-phone" {
		t.Errorf("token = %q, want the registered one", message.Token)
	}
	if message.Tag != "thread:"+mappingThreadID {
		t.Errorf("tag = %q, want the send id, which is what replace and retract are keyed on", message.Tag)
	}
	if message.Data[push.KeyBackend] != "backend-under-test" {
		t.Errorf("backend = %q, want the store identity every socket frame carries, so both tray paths compose one tag", message.Data[push.KeyBackend])
	}
	if message.Data[push.KeyTitle] != "Turn complete" {
		t.Errorf("title = %q, want the kind's fixed phrase", message.Data[push.KeyTitle])
	}
	for key, value := range message.Data {
		if strings.Contains(value, mappingTitle) {
			t.Errorf("data[%s] carries the thread title, which may not transit Google: %q", key, value)
		}
	}
	if message.Data[push.KeyTarget] == "" {
		t.Error("the message carries no target, so a tap could not open anything")
	}
}

// The preference gate is per PHONE, read out of that phone's own device-tier
// bucket. The desktop's own toggles decide nothing here.
func TestAPhoneIsWokenOnItsOwnPreferences(t *testing.T) {
	app, sender := pushApp(t)
	quiet := pairPhone(t, app, "thumb-quiet", "token-quiet")
	loud := pairPhone(t, app, "thumb-loud", "token-loud")

	if _, err := app.UpdateSettings(callFrom(quiet.ID, false), map[string]any{
		"notifyTurnComplete": false,
	}); err != nil {
		t.Fatalf("UpdateSettings on the quiet phone: %v", err)
	}

	firedPush(t, app, turnCompleteSend())
	if got := sender.tokens(); len(got) != 1 || got[0] != "token-loud" {
		t.Fatalf("woken = %v, want only the phone that still wants turn notices", got)
	}

	// And the master switch silences the other one, which proves the gate is
	// the same switch the desktop reads rather than a second copy of it.
	if _, err := app.UpdateSettings(callFrom(loud.ID, false), map[string]any{
		"notificationsEnabled": false,
	}); err != nil {
		t.Fatalf("UpdateSettings on the loud phone: %v", err)
	}
	before := len(sender.messages())
	firedPush(t, app, turnCompleteSend())
	if len(sender.messages()) != before {
		t.Fatal("a phone with notifications off was woken")
	}
}

// A RETRACTION IS NEVER GATED — the same rule notifyOS holds, for the same
// reason: a toggle flipped between a send and its withdrawal must not strand
// the notification it was flipped to stop.
func TestARetractionReachesAPhoneThatTurnedTheKindOff(t *testing.T) {
	app, _ := pushApp(t)
	phone := pairPhone(t, app, "thumb-phone", "token-phone")
	if _, err := app.UpdateSettings(callFrom(phone.ID, false), map[string]any{
		"notificationsEnabled": false,
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	messages := firedPush(t, app, retractSend())
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want the retraction to have gone out anyway", len(messages))
	}
	if messages[0].Data[push.KeyRetract] != push.RetractValue {
		t.Errorf("data = %v, want a retraction", messages[0].Data)
	}
	if _, ok := messages[0].Data[push.KeyTitle]; ok {
		t.Error("a retraction carried a title; withdrawing something says nothing")
	}
}

// Revoking a device is what makes its phone stop being woken. The fan-out
// reads the join, so this needs no second check anywhere.
func TestARevokedDeviceIsNotWoken(t *testing.T) {
	app, sender := pushApp(t)
	revoked := pairPhone(t, app, "thumb-gone", "token-gone")
	pairPhone(t, app, "thumb-here", "token-here")

	if _, err := app.store.RevokeDevice(revoked.DeviceID, 1); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	firedPush(t, app, turnCompleteSend())
	if got := sender.tokens(); len(got) != 1 || got[0] != "token-here" {
		t.Fatalf("woken = %v, want only the device that is still admitted", got)
	}
}

// A backend with no credential records registrations and sends nothing.
// This is every friend's backend, and the owner's until they paste a key.
func TestABackendWithNoCredentialRecordsAndSendsNothing(t *testing.T) {
	app, sender := pushApp(t)
	pairPhone(t, app, "thumb-phone", "token-phone")
	app.installPushSender(nil, "", "")

	app.pushFanout(turnCompleteSend())
	app.push.queue.Wait()
	if len(sender.messages()) != 0 {
		t.Fatal("a backend with no sender sent something")
	}
	live, err := app.store.LivePushTokens()
	if err != nil {
		t.Fatalf("LivePushTokens: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("live registrations = %d, want the token still recorded for the later relay", len(live))
	}
}

// The one actionable failure. A dead registration is dropped, and only that
// device's — the others in the same fan-out still go out.
func TestADeadRegistrationIsDroppedAndTheRestStillGo(t *testing.T) {
	app, sender := pushApp(t)
	dead := pairPhone(t, app, "thumb-dead", "token-dead")
	pairPhone(t, app, "thumb-live", "token-live")
	sender.answer["token-dead"] = push.ErrTokenGone

	firedPush(t, app, turnCompleteSend())
	if got := sender.tokens(); len(got) != 2 {
		t.Fatalf("attempted = %v, want both devices tried", got)
	}
	live, err := app.store.LivePushTokens()
	if err != nil {
		t.Fatalf("LivePushTokens: %v", err)
	}
	if len(live) != 1 || live[0].DeviceID == dead.DeviceID {
		t.Fatalf("live registrations = %+v, want the dead one dropped and no other", live)
	}
	// A dead token is routine, not a fault the owner should be shown.
	status, err := app.GetPushSenderStatus()
	if err != nil {
		t.Fatalf("GetPushSenderStatus: %v", err)
	}
	if status.LastError != "" {
		t.Errorf("lastError = %q, want silence: a re-registering phone is not a broken credential", status.LastError)
	}
}

// Everything else is a standing fault the owner reads, and a success is what
// clears it — otherwise a credential that was fixed would look broken
// forever.
func TestAFailedSendIsVisibleUntilOneSucceeds(t *testing.T) {
	app, sender := pushApp(t)
	pairPhone(t, app, "thumb-phone", "token-phone")
	sender.answer["token-phone"] = errors.New("googleapis said no")

	firedPush(t, app, turnCompleteSend())
	status, err := app.GetPushSenderStatus()
	if err != nil {
		t.Fatalf("GetPushSenderStatus: %v", err)
	}
	if !strings.Contains(status.LastError, "googleapis said no") {
		t.Fatalf("lastError = %q, want the refusal that caused it", status.LastError)
	}
	if !status.Configured || status.RegisteredDevices != 1 {
		t.Fatalf("status = %+v, want one configured sender and one registered phone", status)
	}

	delete(sender.answer, "token-phone")
	firedPush(t, app, turnCompleteSend())
	status, err = app.GetPushSenderStatus()
	if err != nil {
		t.Fatalf("GetPushSenderStatus: %v", err)
	}
	if status.LastError != "" {
		t.Errorf("lastError = %q, want it cleared by the send that worked", status.LastError)
	}
}

// The device is the CALLER's, never a parameter, so there is no shape of
// this call that registers somebody else's phone.
func TestRegistrationBelongsToTheCallingSession(t *testing.T) {
	app, _ := pushApp(t)
	phone := pairPhone(t, app, "thumb-phone", "token-phone")

	live, err := app.store.LivePushTokens()
	if err != nil {
		t.Fatalf("LivePushTokens: %v", err)
	}
	if len(live) != 1 || live[0].DeviceID != phone.DeviceID {
		t.Fatalf("live = %+v, want the calling session's own device", live)
	}

	if err := app.RegisterPushToken(context.Background(), "android", "token-anon"); err == nil {
		t.Error("a call with no session behind it registered a token; there is no device to attribute it to")
	}
	if err := app.UnregisterPushToken(callFrom(phone.ID, false)); err != nil {
		t.Fatalf("UnregisterPushToken: %v", err)
	}
	live, err = app.store.LivePushTokens()
	if err != nil {
		t.Fatalf("LivePushTokens: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("live = %+v, want the caller's own registration gone", live)
	}
}

// serviceAccountJSON is a syntactically real service-account key with a
// freshly minted RSA key. It names no real project and is never sent
// anywhere; it exists so the shape check has something to accept.
var serviceAccountJSON = sync.OnceValue(func() string {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		panic(err)
	}
	raw, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"project_id":   "ao-test-project",
		"client_email": "sender@ao-test-project.iam.gserviceaccount.com",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri":    "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
})

// The credential round trip, shape check included. No send happens here and
// none may: whether Google accepts the key is a question only a real send
// asks, and the answer arrives through lastError.
func TestTheSenderCredentialIsAcceptedOnShapeAndSurvivesABoot(t *testing.T) {
	app, _ := pushApp(t)
	app.installPushSender(nil, "", "")

	if err := app.SetPushSenderCredential(`{"type":"user","project_id":"p"}`); err == nil {
		t.Error("a key file that is not a service account was accepted")
	}
	if err := app.SetPushSenderCredential(`{"type":"service_account","project_id":"p","client_email":"a@b","private_key":"not a pem"}`); err == nil {
		t.Error("a key file whose private key does not parse was accepted")
	}

	if err := app.SetPushSenderCredential(serviceAccountJSON()); err != nil {
		t.Fatalf("SetPushSenderCredential: %v", err)
	}
	status, err := app.GetPushSenderStatus()
	if err != nil {
		t.Fatalf("GetPushSenderStatus: %v", err)
	}
	if !status.Configured || status.ProjectID != "ao-test-project" {
		t.Fatalf("status = %+v, want the pasted project", status)
	}

	// A boot reads it back out of the store and sends with it again.
	app.installPushSender(nil, "", "")
	app.loadPushSender()
	if app.currentPushSender() == nil {
		t.Fatal("the stored credential did not survive a boot")
	}

	if err := app.ClearPushSenderCredential(); err != nil {
		t.Fatalf("ClearPushSenderCredential: %v", err)
	}
	if app.currentPushSender() != nil {
		t.Error("clearing the credential left a sender behind")
	}
}

// Clearing the credential leaves the REGISTRATIONS alone: the phones have
// not changed their minds, and re-pasting a key should not cost each of them
// a launch to come back.
func TestClearingTheCredentialKeepsTheRegistrations(t *testing.T) {
	app, _ := pushApp(t)
	pairPhone(t, app, "thumb-phone", "token-phone")
	if err := app.ClearPushSenderCredential(); err != nil {
		t.Fatalf("ClearPushSenderCredential: %v", err)
	}
	live, err := app.store.LivePushTokens()
	if err != nil {
		t.Fatalf("LivePushTokens: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("live registrations = %d, want the phones left registered", len(live))
	}
}

// The notification queue FEEDS the push queue, so a shutdown that drained
// only the first would cut a send that had not been enqueued yet. Both
// drains are in app_shutdown.go in that order; this pins that the push one
// actually waits.
func TestDrainPushWaitsForAnInFlightSend(t *testing.T) {
	app, sender := pushApp(t)
	pairPhone(t, app, "thumb-phone", "token-phone")

	release := make(chan struct{})
	blocking := &blockingPushSender{inner: sender, gate: release}
	app.installPushSender(blocking, "p", "e")

	app.pushFanout(turnCompleteSend())
	drained := make(chan error, 1)
	go func() { drained <- app.drainPush(context.Background(), pushDrainTimeout) }()

	select {
	case err := <-drained:
		t.Fatalf("drainPush returned %v while a send was still in flight", err)
	default:
	}
	close(release)
	if err := <-drained; err != nil {
		t.Fatalf("drainPush: %v", err)
	}
	if len(sender.messages()) != 1 {
		t.Fatal("the in-flight send did not finish before the drain returned")
	}
}

type blockingPushSender struct {
	inner *fakePushSender
	gate  chan struct{}
}

func (b *blockingPushSender) Send(ctx context.Context, message push.Message) error {
	<-b.gate
	return b.inner.Send(ctx, message)
}

// A registration is an address a send is made to, so the shapes that could
// never be one are refused before a row exists for them.
//
// The empty one is what an unregistered shell sends when it means "I have
// nothing yet", and storing it puts the backend in the state where it believes
// it can wake a device and every send to it fails. Before this, the register
// call took whatever it was handed.
func TestARegistrationThatCouldNeverWakeAPhoneIsRefused(t *testing.T) {
	app, _ := pushApp(t)
	phone := pairPhone(t, app, "thumb-phone", "token-phone")
	ctx := callFrom(phone.ID, false)

	for _, tc := range []struct{ name, token string }{
		{"empty", ""},
		{"whitespace only", " \t\n "},
		{"past the ceiling", strings.Repeat("a", maxPushTokenBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := app.RegisterPushToken(ctx, "android", tc.token); err == nil {
				t.Fatal("a token that cannot be sent to was registered")
			}
		})
	}

	// And the registration this device already had is untouched: a refused
	// call is not a way to unregister.
	live, err := app.store.LivePushTokens()
	if err != nil {
		t.Fatalf("LivePushTokens: %v", err)
	}
	if len(live) != 1 || live[0].Token != "token-phone" {
		t.Fatalf("live = %+v, want the existing registration unchanged", live)
	}
}

// A token with room around it is the same token. Trimming happens before the
// ceiling and before the row, so two calls a shell makes with and without a
// trailing newline are one registration.
func TestASurroundedTokenIsStoredTrimmed(t *testing.T) {
	app, _ := pushApp(t)
	phone := pairPhone(t, app, "thumb-phone", "token-phone")

	if err := app.RegisterPushToken(callFrom(phone.ID, false), "android", "  token-fresh\n"); err != nil {
		t.Fatalf("RegisterPushToken: %v", err)
	}
	live, err := app.store.LivePushTokens()
	if err != nil {
		t.Fatalf("LivePushTokens: %v", err)
	}
	if len(live) != 1 || live[0].Token != "token-fresh" {
		t.Fatalf("live = %+v, want one registration holding the trimmed token", live)
	}
}
