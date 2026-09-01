//go:build windows

package main

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

var (
	errLauncherNotificationAuthorizationPending = errors.New("notification authorization is pending")
	errLauncherNotificationAuthorizationDenied  = errors.New("notification authorization was not granted")
)

// launcherNotificationService owns host-side Windows presentation. It keeps
// authorization off Wails' startup path and converts every unavailable state
// into a visible presenter error that the bridge client records in
// launcher.log.
type launcherNotificationService struct {
	service    *notifications.NotificationService
	onActivate func(notify.Target)

	mu             sync.RWMutex
	started        bool
	unavailableErr error
}

func newLauncherNotificationService(onActivate func(notify.Target)) *launcherNotificationService {
	service := notifications.New()
	n := &launcherNotificationService{
		service:        service,
		onActivate:     onActivate,
		unavailableErr: errLauncherNotificationAuthorizationPending,
	}
	service.OnNotificationResponse(n.handleResponse)
	return n
}

func (n *launcherNotificationService) ServiceName() string {
	return n.service.ServiceName()
}

func (n *launcherNotificationService) ServiceStartup(ctx context.Context, options application.ServiceOptions) error {
	if err := n.service.ServiceStartup(ctx, options); err != nil {
		cleanupErr := n.service.ServiceShutdown()
		n.setUnavailable(fmt.Errorf("notification service unavailable: %w", errors.Join(err, cleanupErr)))
		return nil
	}
	n.mu.Lock()
	n.started = true
	n.unavailableErr = errLauncherNotificationAuthorizationPending
	n.mu.Unlock()
	go n.requestAuthorization(ctx)
	return nil
}

func (n *launcherNotificationService) requestAuthorization(ctx context.Context) {
	authorized, err := n.service.RequestNotificationAuthorization()
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		n.setUnavailableIfStarted(fmt.Errorf("notification authorization failed: %w", err))
		return
	}
	if !authorized {
		n.setUnavailableIfStarted(errLauncherNotificationAuthorizationDenied)
		return
	}
	n.mu.Lock()
	if n.started {
		n.unavailableErr = nil
	}
	n.mu.Unlock()
}

func (n *launcherNotificationService) ServiceShutdown() error {
	n.mu.Lock()
	started := n.started
	n.started = false
	n.unavailableErr = errors.New("notification service has shut down")
	n.mu.Unlock()
	if !started {
		return nil
	}
	return n.service.ServiceShutdown()
}

// present raises, replaces or withdraws one bridged notification.
//
// Retraction degrades to nothing on this platform and that is deliberate:
// wintoast exposes no call that pulls a delivered toast back out of the
// Action Center, so Wails' RemoveNotification answers nil without acting.
// The alternative — refusing the retraction, or logging it as a failure —
// would turn a platform's limit into an error the user sees, for an
// operation whose whole purpose is to make things quieter. UpdateNotification
// has the same shape: replace-in-place where the platform has it, a fresh
// toast where it does not.
func (n *launcherNotificationService) present(send notify.Send) error {
	n.mu.RLock()
	unavailableErr := n.unavailableErr
	n.mu.RUnlock()
	if unavailableErr != nil {
		return unavailableErr
	}
	if send.Retract {
		return n.service.RemoveNotification(send.ID)
	}
	data, err := notify.TargetToMap(send.Target)
	if err != nil {
		return err
	}
	return n.service.UpdateNotification(notifications.NotificationOptions{
		ID:    send.ID,
		Title: send.Title,
		Body:  send.Body,
		Data:  data,
	})
}

func (n *launcherNotificationService) handleResponse(result notifications.NotificationResult) {
	if result.Error != nil {
		log.Printf("notifications: launcher activation failed: %v", result.Error)
		return
	}
	if result.Response.ActionIdentifier != notifications.DefaultActionIdentifier {
		return
	}
	target, err := notify.TargetFromMap(result.Response.UserInfo)
	if err != nil {
		log.Printf("notifications: ignore invalid launcher activation target: %v", err)
		return
	}
	if n.onActivate != nil {
		n.onActivate(target)
	}
}

func (n *launcherNotificationService) setUnavailable(err error) {
	n.mu.Lock()
	n.unavailableErr = err
	n.mu.Unlock()
	log.Printf("notifications: %v", err)
}

func (n *launcherNotificationService) setUnavailableIfStarted(err error) {
	n.mu.Lock()
	if !n.started {
		n.mu.Unlock()
		return
	}
	n.unavailableErr = err
	n.mu.Unlock()
	log.Printf("notifications: %v", err)
}
