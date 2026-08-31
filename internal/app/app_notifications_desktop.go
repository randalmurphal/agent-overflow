//go:build !nogui

package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"agent-overflow/internal/notify"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

type notificationPlatformService interface {
	ServiceName() string
	ServiceStartup(context.Context, application.ServiceOptions) error
	ServiceShutdown() error
	RequestNotificationAuthorization() (bool, error)
	SendNotification(notifications.NotificationOptions) error
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
	nextID         atomic.Uint64
}

func NewDesktopNotificationService(app *App, window func() *application.WebviewWindow) *desktopNotificationService {
	n := newDesktopNotificationServiceWithPlatform(app, notifications.New())
	n.window = window
	return n
}

func newDesktopNotificationServiceWithPlatform(app *App, service notificationPlatformService) *desktopNotificationService {
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

func (n *desktopNotificationService) send(title, body string, target notify.Target) error {
	n.mu.RLock()
	unavailableErr := n.unavailableErr
	n.mu.RUnlock()
	if unavailableErr != nil {
		return unavailableErr
	}

	data, err := notify.TargetToMap(target)
	if err != nil {
		return err
	}
	return n.service.SendNotification(notifications.NotificationOptions{
		ID:    notify.NewID(n.nextID.Add(1)),
		Title: title,
		Body:  body,
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
