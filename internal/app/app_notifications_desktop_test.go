//go:build !nogui

package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/notify"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

type notificationAuthorizationResult struct {
	authorized bool
	err        error
}

type fakeNotificationPlatform struct {
	startupErr error
	auth       chan notificationAuthorizationResult
	authCalled chan struct{}

	mu        sync.Mutex
	callback  func(notifications.NotificationResult)
	sent      []notifications.NotificationOptions
	removed   []string
	shutdowns int
}

func newFakeNotificationPlatform() *fakeNotificationPlatform {
	return &fakeNotificationPlatform{
		auth:       make(chan notificationAuthorizationResult, 1),
		authCalled: make(chan struct{}),
	}
}

func (f *fakeNotificationPlatform) ServiceName() string { return "fake-notifications" }
func (f *fakeNotificationPlatform) ServiceStartup(context.Context, application.ServiceOptions) error {
	return f.startupErr
}
func (f *fakeNotificationPlatform) ServiceShutdown() error {
	f.shutdowns++
	return nil
}
func (f *fakeNotificationPlatform) RequestNotificationAuthorization() (bool, error) {
	close(f.authCalled)
	result := <-f.auth
	return result.authorized, result.err
}
func (f *fakeNotificationPlatform) SendNotification(options notifications.NotificationOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, options)
	return nil
}
func (f *fakeNotificationPlatform) UpdateNotification(options notifications.NotificationOptions) error {
	return f.SendNotification(options)
}
func (f *fakeNotificationPlatform) RemoveDeliveredNotification(identifier string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, identifier)
	return nil
}
func (f *fakeNotificationPlatform) OnNotificationResponse(callback func(notifications.NotificationResult)) {
	f.callback = callback
}

func TestDesktopNotificationsAuthorizationIsAsynchronous(t *testing.T) {
	app := &App{}
	platform := newFakeNotificationPlatform()
	service := newDesktopNotificationServiceWithPlatform(app, platform)
	if err := service.ServiceStartup(context.Background(), application.ServiceOptions{}); err != nil {
		t.Fatalf("ServiceStartup: %v", err)
	}

	select {
	case <-platform.authCalled:
	case <-time.After(time.Second):
		t.Fatal("authorization request did not start asynchronously")
	}
	err := app.notifyOS(testSend(notify.Target{Kind: "none"}))
	var notificationErr *NotificationError
	if !errors.As(err, &notificationErr) || notificationErr.Code != NotificationAuthorizationPending {
		t.Fatalf("pending send error = %v, want authorization_pending", err)
	}

	platform.auth <- notificationAuthorizationResult{authorized: true}
	deadline := time.Now().Add(time.Second)
	for {
		err = app.notifyOS(testSend(notify.Target{Kind: "none"}))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("authorization never became ready: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDesktopNotificationsStartupFailureDoesNotFailAppStartup(t *testing.T) {
	app := &App{}
	platform := newFakeNotificationPlatform()
	platform.startupErr = errors.New("no notification daemon")
	service := newDesktopNotificationServiceWithPlatform(app, platform)
	if err := service.ServiceStartup(context.Background(), application.ServiceOptions{}); err != nil {
		t.Fatalf("optional notification startup blocked app: %v", err)
	}
	err := app.notifyOS(testSend(notify.Target{Kind: "none"}))
	var notificationErr *NotificationError
	if !errors.As(err, &notificationErr) || notificationErr.Code != NotificationUnavailable {
		t.Fatalf("send error = %v, want unavailable", err)
	}
	if platform.shutdowns != 1 {
		t.Fatalf("startup failure cleanup calls = %d, want 1", platform.shutdowns)
	}
}

func TestDesktopNotificationsAuthorizationFailureStates(t *testing.T) {
	tests := []struct {
		name   string
		result notificationAuthorizationResult
		code   NotificationErrorCode
	}{
		{name: "denied", result: notificationAuthorizationResult{authorized: false}, code: NotificationAuthorizationDenied},
		{name: "request error", result: notificationAuthorizationResult{err: errors.New("authorization service failed")}, code: NotificationUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := &App{}
			platform := newFakeNotificationPlatform()
			service := newDesktopNotificationServiceWithPlatform(app, platform)
			if err := service.ServiceStartup(context.Background(), application.ServiceOptions{}); err != nil {
				t.Fatalf("ServiceStartup: %v", err)
			}
			<-platform.authCalled
			platform.auth <- test.result

			deadline := time.Now().Add(time.Second)
			for {
				err := app.notifyOS(testSend(notify.Target{Kind: "none"}))
				var notificationErr *NotificationError
				if errors.As(err, &notificationErr) && notificationErr.Code == test.code {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("authorization failure state = %v, want %s", err, test.code)
				}
				time.Sleep(time.Millisecond)
			}
			platform.mu.Lock()
			defer platform.mu.Unlock()
			if len(platform.sent) != 0 {
				t.Fatalf("platform sends = %d, want 0", len(platform.sent))
			}
		})
	}
}

func TestDesktopNotificationResponseIgnoresDismissal(t *testing.T) {
	var emitted int
	app := &App{testEmitHook: func(name string, _ any) {
		if name == notify.ActivatedChannel {
			emitted++
		}
	}}
	platform := newFakeNotificationPlatform()
	newDesktopNotificationServiceWithPlatform(app, platform)
	targetData, err := notify.TargetToMap(notify.Target{Kind: "none"})
	if err != nil {
		t.Fatalf("encode target: %v", err)
	}

	platform.callback(notifications.NotificationResult{Response: notifications.NotificationResponse{
		ActionIdentifier: "com.apple.UNNotificationDismissActionIdentifier",
		UserInfo:         targetData,
	}})
	if emitted != 0 {
		t.Fatalf("dismissal emitted %d activation events, want 0", emitted)
	}

	platform.callback(notifications.NotificationResult{Response: notifications.NotificationResponse{
		ActionIdentifier: notifications.DefaultActionIdentifier,
		UserInfo:         targetData,
	}})
	if emitted != 1 {
		t.Fatalf("default action emitted %d activation events, want 1", emitted)
	}
}

// newAuthorizedDesktopNotifications is the fake platform wired through the
// real service and driven to the authorized state, which is the only state
// in which sends reach the platform at all.
func newAuthorizedDesktopNotifications(t *testing.T) (*App, *fakeNotificationPlatform) {
	t.Helper()
	app := &App{}
	platform := newFakeNotificationPlatform()
	service := newDesktopNotificationServiceWithPlatform(app, platform)
	if err := service.ServiceStartup(context.Background(), application.ServiceOptions{}); err != nil {
		t.Fatalf("ServiceStartup: %v", err)
	}
	<-platform.authCalled
	platform.auth <- notificationAuthorizationResult{authorized: true}
	deadline := time.Now().Add(time.Second)
	for {
		if err := app.notifyOS(testSend(notify.Target{Kind: "none"})); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("authorization never became ready")
		}
		time.Sleep(time.Millisecond)
	}
	platform.mu.Lock()
	platform.sent = nil
	platform.mu.Unlock()
	return app, platform
}

// TestADesktopServiceWithNoPlatformFailsAtConstruction is the fixture
// tripwire: a test that forgets its fake must stop here rather than raise a
// real notification on whoever is running the suite.
func TestADesktopServiceWithNoPlatformFailsAtConstruction(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a nil platform was accepted")
		}
	}()
	newDesktopNotificationServiceWithPlatform(&App{}, nil)
}

// TestTheStableIDReachesThePlatformVerbatim is what makes retraction and
// replace-in-place work at all: the platform recognises a second send about
// the same moment only if it carries the same identifier.
func TestTheStableIDReachesThePlatformVerbatim(t *testing.T) {
	app, platform := newAuthorizedDesktopNotifications(t)
	send := testSend(notify.Target{Kind: "thread", ThreadID: "thread-1", BackendID: "backend-1"})
	send.ID = "thread:thread-1"
	for range 2 {
		if err := app.notifyOS(send); err != nil {
			t.Fatalf("notifyOS: %v", err)
		}
	}

	platform.mu.Lock()
	defer platform.mu.Unlock()
	if len(platform.sent) != 2 {
		t.Fatalf("platform sends = %d, want 2", len(platform.sent))
	}
	for _, options := range platform.sent {
		if options.ID != "thread:thread-1" {
			t.Fatalf("platform id = %q, want the mapped id", options.ID)
		}
	}
	// The route rides in the platform's own payload, so a click can be
	// answered — including the backend it belongs to.
	target, err := notify.TargetFromMap(platform.sent[0].Data)
	if err != nil {
		t.Fatalf("decode target: %v", err)
	}
	if target != send.Target {
		t.Fatalf("target = %#v, want %#v", target, send.Target)
	}
}

// TestARetractionWithdrawsByIDAndPresentsNothing.
func TestARetractionWithdrawsByIDAndPresentsNothing(t *testing.T) {
	app, platform := newAuthorizedDesktopNotifications(t)
	err := app.notifyOS(notify.Send{
		ID: "thread:thread-1", Kind: notify.KindTurnComplete, Retract: true,
	})
	if err != nil {
		t.Fatalf("notifyOS: %v", err)
	}
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if len(platform.sent) != 0 {
		t.Fatalf("a retraction presented %d notifications", len(platform.sent))
	}
	if len(platform.removed) != 1 || platform.removed[0] != "thread:thread-1" {
		t.Fatalf("removed = %#v, want the presented id", platform.removed)
	}
}

// TestAnUnavailablePlatformIsNotReachedByARetraction either: the
// availability state is checked before the branch, so a shut-down service
// does not call into the platform for a withdrawal.
func TestAnUnavailablePlatformIsNotReachedByARetraction(t *testing.T) {
	app, platform := newAuthorizedDesktopNotifications(t)
	service, ok := app.osNotifications.(*desktopNotificationService)
	if !ok {
		t.Fatalf("osNotifications = %T", app.osNotifications)
	}
	if err := service.ServiceShutdown(); err != nil {
		t.Fatalf("ServiceShutdown: %v", err)
	}
	err := app.notifyOS(notify.Send{
		ID: "thread:thread-1", Kind: notify.KindTurnComplete, Retract: true,
	})
	var notificationErr *NotificationError
	if !errors.As(err, &notificationErr) || notificationErr.Code != NotificationUnavailable {
		t.Fatalf("retraction after shutdown = %v, want unavailable", err)
	}
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if len(platform.removed) != 0 {
		t.Fatalf("removed = %#v, want none", platform.removed)
	}
}

// EXACTLY ONE SOUND reaches the platform. The vendored contract is that a nil
// Sound means the platform's DEFAULT notification sound and
// `&NotificationSound{Silent: true}` means none, so leaving the field alone —
// what this presenter did before — added the OS sound on top of the app's own
// cue on macOS and Windows. These pin the mapping in both directions.
func TestTheHostsSilentAnswerReachesThePlatform(t *testing.T) {
	tests := []struct {
		name   string
		silent bool
		want   *notifications.NotificationSound
	}{
		{
			name:   "a cue is playing in the app",
			silent: true,
			want:   &notifications.NotificationSound{Silent: true},
		},
		{
			// nil, not a named sound: the user chose "the system sound",
			// and naming one here would pick a sound they did not choose.
			name:   "the system sound is the chosen cue",
			silent: false,
			want:   nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, platform := newAuthorizedDesktopNotifications(t)
			send := testSend(notify.Target{Kind: "none"})
			send.Silent = test.silent
			if err := app.notifyOSUngated(send); err != nil {
				t.Fatalf("notifyOSUngated: %v", err)
			}

			platform.mu.Lock()
			defer platform.mu.Unlock()
			if len(platform.sent) != 1 {
				t.Fatalf("platform sends = %d, want 1", len(platform.sent))
			}
			got := platform.sent[0].Sound
			if test.want == nil {
				if got != nil {
					t.Fatalf("Sound = %#v, want nil (the platform's default sound)", got)
				}
				return
			}
			if got == nil || *got != *test.want {
				t.Fatalf("Sound = %#v, want %#v", got, test.want)
			}
		})
	}
}

// A retraction is a withdrawal by id. It never reaches the options shape at
// all, so there is nothing for a sound to be attached to.
func TestARetractionCarriesNoPlatformSound(t *testing.T) {
	app, platform := newAuthorizedDesktopNotifications(t)
	err := app.notifyOS(notify.Send{
		ID: "thread:thread-1", Kind: notify.KindTurnComplete, Retract: true,
	})
	if err != nil {
		t.Fatalf("notifyOS: %v", err)
	}
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if len(platform.sent) != 0 {
		t.Fatalf("a retraction presented %#v", platform.sent)
	}
	if len(platform.removed) != 1 {
		t.Fatalf("removed = %#v, want the withdrawn id", platform.removed)
	}
}

// The whole point, end to end on the in-process presenter: the shipped
// default is a built-in cue, so a real notifyOS send arrives at the platform
// asking for silence rather than a second sound.
func TestTheDefaultCueReachesThePlatformSilent(t *testing.T) {
	app, platform := newAuthorizedDesktopNotifications(t)
	if err := app.notifyOS(testSend(notify.Target{Kind: "none"})); err != nil {
		t.Fatalf("notifyOS: %v", err)
	}
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if len(platform.sent) != 1 {
		t.Fatalf("platform sends = %d, want 1", len(platform.sent))
	}
	sound := platform.sent[0].Sound
	if sound == nil || !sound.Silent {
		t.Fatalf("Sound = %#v, want silent: the app is already playing the cue", sound)
	}
}
