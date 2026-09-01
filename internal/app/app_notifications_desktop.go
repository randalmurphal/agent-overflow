//go:build !nogui

package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"agent-overflow/internal/notify"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

// notificationPlatformService is the desktop presenter's one seam onto the
// operating system, and the only thing in this package that can raise a real
// notification on the developer's machine.
//
// Wails v3 ships this surface for all three desktops
// (`pkg/services/notifications`: XDG D-Bus on Linux, UNUserNotificationCenter
// on macOS, WinRT toasts on Windows), so there is no platform-command split
// to write here — the `*_darwin.go` / `*_windows.go` convention applies where
// we own the mechanism, and here the vendored tree already does.
//
// UpdateNotification and RemoveNotification are on the seam because
// retraction is (docs/specs/remote-access.md §9). Their platform support is
// uneven and the caller must not care: Linux closes the D-Bus notification
// and re-posts with `replaces_id`, macOS removes the delivered notification,
// and Windows answers nil without doing anything (wintoast exposes no
// retract-from-Action-Center call). Degrading silently is the contract.
type notificationPlatformService interface {
	ServiceName() string
	ServiceStartup(context.Context, application.ServiceOptions) error
	ServiceShutdown() error
	RequestNotificationAuthorization() (bool, error)
	SendNotification(notifications.NotificationOptions) error
	UpdateNotification(notifications.NotificationOptions) error
	RemoveNotification(identifier string) error
	OnNotificationResponse(func(notifications.NotificationResult))
}

// desktopNotificationService adapts Wails' fail-fast service lifecycle to an
// optional application feature. Platform startup and authorization failures
// become visible send errors; they never prevent the app from starting.
type desktopNotificationService struct {
	app     *App
	service notificationPlatformService
	window  func() *application.WebviewWindow

	mu             sync.RWMutex
	started        bool
	unavailableErr error
}

func NewDesktopNotificationService(app *App, window func() *application.WebviewWindow) *desktopNotificationService {
	n := newDesktopNotificationServiceWithPlatform(app, notifications.New())
	n.window = window
	return n
}

// newDesktopNotificationServiceWithPlatform is the one constructor, and it
// PANICS on a nil platform rather than degrading.
//
// That is the internal/serviceinstall rule applied to a louder machine: a
// test fixture that forgot its fake must fail at construction, not by
// raising a real notification on the developer's desktop while the suite
// runs. There is exactly one caller that passes the real service, and it is
// the exported constructor above.
func newDesktopNotificationServiceWithPlatform(app *App, service notificationPlatformService) *desktopNotificationService {
	if service == nil {
		panic("app: a notification platform service is required")
	}
	n := &desktopNotificationService{
		app:     app,
		service: service,
		unavailableErr: &NotificationError{
			Code: NotificationAuthorizationPending,
		},
	}
	service.OnNotificationResponse(n.handleResponse)
	app.osNotifications = n
	return n
}

func (n *desktopNotificationService) ServiceName() string {
	return n.service.ServiceName()
}

func (n *desktopNotificationService) ServiceStartup(ctx context.Context, options application.ServiceOptions) error {
	if err := n.service.ServiceStartup(ctx, options); err != nil {
		cleanupErr := n.service.ServiceShutdown()
		n.setUnavailable(&NotificationError{
			Code:  NotificationUnavailable,
			Cause: errors.Join(err, cleanupErr),
		})
		return nil
	}

	n.mu.Lock()
	n.started = true
	n.unavailableErr = &NotificationError{Code: NotificationAuthorizationPending}
	n.mu.Unlock()

	go n.requestAuthorization(ctx)
	return nil
}

func (n *desktopNotificationService) requestAuthorization(ctx context.Context) {
	authorized, err := n.service.RequestNotificationAuthorization()
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		n.setUnavailableIfStarted(&NotificationError{
			Code:  NotificationUnavailable,
			Cause: fmt.Errorf("notification authorization failed: %w", err),
		})
		return
	}
	if !authorized {
		n.setUnavailableIfStarted(&NotificationError{Code: NotificationAuthorizationDenied})
		return
	}

	n.mu.Lock()
	if n.started {
		n.unavailableErr = nil
	}
	n.mu.Unlock()
}

func (n *desktopNotificationService) ServiceShutdown() error {
	n.mu.Lock()
	started := n.started
	n.started = false
	n.unavailableErr = &NotificationError{
		Code:  NotificationUnavailable,
		Cause: errors.New("notification service has shut down"),
	}
	n.mu.Unlock()
	if !started {
		return nil
	}
	return n.service.ServiceShutdown()
}

// send presents, updates or withdraws one notification.
//
// The mapping's stable ID is passed through verbatim rather than
// re-allocated, which is what makes the platform recognise a second send
// about the same moment as the SAME notification: Linux re-posts with
// `replaces_id`, macOS's center replaces by identifier. Allocating a fresh
// id per call — what this did before retraction existed — turned "the turn
// after the one that finished also finished" into two banners.
//
// UpdateNotification is used for a presentation rather than SendNotification
// because it is the same call plus replace-in-place where a platform has it;
// on the two that do not, it forwards to SendNotification itself.
func (n *desktopNotificationService) send(payload notify.Send) error {
	n.mu.RLock()
	unavailableErr := n.unavailableErr
	n.mu.RUnlock()
	if unavailableErr != nil {
		return unavailableErr
	}

	if payload.Retract {
		return n.service.RemoveNotification(payload.ID)
	}
	data, err := notify.TargetToMap(payload.Target)
	if err != nil {
		return err
	}
	return n.service.UpdateNotification(notifications.NotificationOptions{
		ID:    payload.ID,
		Title: payload.Title,
		Body:  payload.Body,
		Data:  data,
	})
}

func (n *desktopNotificationService) setUnavailable(err error) {
	n.mu.Lock()
	n.unavailableErr = err
	n.mu.Unlock()
	log.Printf("notifications: %v", err)
}

func (n *desktopNotificationService) setUnavailableIfStarted(err error) {
	n.mu.Lock()
	if !n.started {
		n.mu.Unlock()
		return
	}
	n.unavailableErr = err
	n.mu.Unlock()
	log.Printf("notifications: %v", err)
}

func (n *desktopNotificationService) handleResponse(result notifications.NotificationResult) {
	if result.Error != nil {
		log.Printf("notifications: activation failed: %v", result.Error)
		return
	}
	if result.Response.ActionIdentifier != notifications.DefaultActionIdentifier {
		return
	}
	target, err := notify.TargetFromMap(result.Response.UserInfo)
	if err != nil {
		log.Printf("notifications: ignore malformed activation payload: %v", err)
		return
	}
	if n.window != nil {
		if window := n.window(); window != nil {
			window.Show()
			window.Restore()
			window.Focus()
		}
	}
	if err := n.app.activateNotificationTarget(target); err != nil {
		log.Printf("notifications: ignore invalid activation target: %v", err)
	}
}

var _ osNotificationSender = (*desktopNotificationService)(nil)
