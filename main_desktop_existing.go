//go:build !nogui

package main

import (
	"context"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/localcontrol"
	"agent-overflow/internal/pagehost"
	"agent-overflow/internal/windowgeom"
)

// A desktop window is a frontend to an already-running local service. It
// starts no second App, owns no provider process, and cannot stop the service
// when the window closes. Re-read rendezvous on reload to follow a restart.
func runExistingDesktop() bool {
	root := bootSettingsDir()
	readPage := func() (pagehost.Answer, error) {
		endpoint, err := localcontrol.Read(root)
		if err != nil {
			return pagehost.Answer{}, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return localcontrol.Page(ctx, endpoint)
	}
	first, err := readPage()
	if err != nil {
		return false
	}
	var mu sync.Mutex
	page := first
	shell := webviewShell{
		title:          appidentity.AppTitle(nativeSingleInstanceMode()),
		singleInstance: true,
		pageURL: func() string {
			mu.Lock()
			defer mu.Unlock()
			if next, err := readPage(); err == nil {
				page = next
			} else {
				log.Printf("local desktop reconnect: %v", err)
			}
			return page.URL
		},
		mintTicket: func() (string, error) {
			next, err := readPage()
			if err != nil {
				return "", err
			}
			return next.Ticket, nil
		},
		loadGeometry: loadPersistedWindowGeometry,
		persistGeometry: func(geometry windowgeom.Geometry) {
			endpoint, err := localcontrol.Read(root)
			if err != nil {
				log.Printf("save window placement: %v", err)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			client, err := localcontrol.Dial(ctx, endpoint)
			if err == nil {
				defer client.Close()
				err = client.Call(ctx, "UpdateSettings", nil, map[string]any{"window": geometry})
			}
			if err != nil {
				log.Printf("save window placement: %v", err)
			}
		},
	}
	if err := shell.run(); err != nil {
		fatalf("local desktop window: %v", err)
	}
	return true
}
