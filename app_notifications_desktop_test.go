//go:build !nogui

package main

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
	err := app.notifyOS("Ready", "Body", notify.Target{Kind: "none"})
	var notificationErr *NotificationError
	if !errors.As(err, &notificationErr) || notificationErr.Code != NotificationAuthorizationPending {
		t.Fatalf("pending send error = %v, want authorization_pending", err)
	}

	platform.auth <- notificationAuthorizationResult{authorized: true}
	deadline := time.Now().Add(time.Second)
	for {
		err = app.notifyOS("Ready", "Body", notify.Target{Kind: "none"})
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
	err := app.notifyOS("Ready", "Body", notify.Target{Kind: "none"})
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
				err := app.notifyOS("Ready", "Body", notify.Target{Kind: "none"})
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
