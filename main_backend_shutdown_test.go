package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	appservice "agent-overflow/internal/app"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/wsllauncher"
)

// TestArmBackendShutdownDoorWakesTheSignalWait pins the root half of the
// launcher's window-close path: the RPC the launcher calls has to reach
// the wait that owns this process's ordered teardown. Without this seam
// the backend is only ever killed, and the next boot settles its in-flight
// turns as interrupted.
func TestArmBackendShutdownDoorWakesTheSignalWait(t *testing.T) {
	appService := NewApp()
	requested := armBackendShutdownDoor(appService)

	select {
	case <-requested:
		t.Fatal("the wait was woken before anything asked for a shutdown")
	default:
	}

	if err := appService.ShutdownBackend(); err != nil {
		t.Fatalf("ShutdownBackend: %v", err)
	}
	select {
	case <-requested:
	case <-time.After(2 * time.Second):
		t.Fatal("ShutdownBackend did not wake the signal wait")
	}

	// A retry, or a window close racing its own backstop, must stay a
	// no-op rather than blocking the RPC handler on a closed channel.
	if err := appService.ShutdownBackend(); err != nil {
		t.Fatalf("second ShutdownBackend: %v", err)
	}
	select {
	case <-requested:
	case <-time.After(2 * time.Second):
		t.Fatal("the shutdown channel did not stay closed")
	}
}

// TestLauncherShutdownReachesABackendThatIsStillStarting drives the
// launcher's own client against a transport configured the way every boot
// configures it, before MarkReady: the launcher abandons a backend whose
// probe failed by asking it to stop, so ShutdownBackend has to be served
// while App.Start still runs, and has to cancel that Start. Every other
// App method is refused with the retryable code until ready.
func TestLauncherShutdownReachesABackendThatIsStillStarting(t *testing.T) {
	appService := NewApp()
	requested := armBackendShutdownDoor(appService)
	bootCtx, bootCancel := context.WithCancel(context.Background())
	defer bootCancel()
	cancelBootOnShutdownRequest(bootCtx, bootCancel, requested)

	dispatcher := transport.NewDispatcher()
	if _, err := dispatcher.Register(appService, transport.RegisterOptions{
		Package:   "main",
		TypeName:  "App",
		AllowList: transport.NewMethodAllowList(),
	}); err != nil {
		t.Fatalf("register App: %v", err)
	}
	cfg := transport.Config{
		Dispatcher: dispatcher,
		EventBus:   transport.NewEventBus(64),
		Token:      "launcher-token",
		BindAddr:   "127.0.0.1",
		Sessions:   appservice.SessionAuthority(appService.App),
	}
	applyBootReadiness(&cfg)
	srv, err := transport.New(cfg)
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start transport: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	})

	client, err := wsllauncher.NewNotificationClient(wsllauncher.NotificationClientConfig{
		WSURL:      fmt.Sprintf("ws://%s/ws", srv.Addr()),
		Token:      "launcher-token",
		Present:    func(notify.Send) error { return nil },
		MinBackoff: 20 * time.Millisecond,
		MaxBackoff: 100 * time.Millisecond,
		Logf:       t.Logf,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go client.Run(ctx)

	var refused *wsllauncher.RPCRefusedError
	err = client.Activate(ctx, notify.Target{Kind: notify.TargetNone})
	if !errors.As(err, &refused) || refused.Code != transport.ErrCodeTemporarilyUnavailable {
		t.Fatalf("NotificationActivated before ready = %v, want temporarily_unavailable", err)
	}
	select {
	case <-requested:
		t.Fatal("a refused call reached the shutdown door")
	default:
	}

	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("ShutdownBackend before ready: %v", err)
	}
	select {
	case <-bootCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("ShutdownBackend before ready did not cancel the running Start")
	}
}
