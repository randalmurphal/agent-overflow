//go:build !nogui

package main

import (
	"context"
	"errors"
	"path/filepath"

	appservice "agent-overflow/internal/app"
	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/clientmode"
	"agent-overflow/internal/frontendclient"
	"agent-overflow/internal/transport"
)

type clientWindowServer interface {
	AppURL() string
	Addr() string
	MintPageTicket() (string, error)
	Shutdown(context.Context) error
}

type clientWindowHooks struct {
	setBackground    func(uint8, uint8, uint8)
	configureUpdater func(*appupdate.Service)
}

func serveClientWindow(cfg clientmode.Config, hooks clientWindowHooks) (clientWindowServer, error) {
	if cfg.Paired == nil && cfg.WSURL != "" {
		return clientmode.Serve(cfg)
	}
	root := bootSettingsDir()
	if root == "" {
		return nil, errors.New("cannot resolve this frontend's configuration directory")
	}
	profiles, err := deviceProfileDir()
	if err != nil {
		return nil, err
	}
	// A frontend-only desktop has its own origin and presentation files; it
	// may run beside this installation's ordinary execution-host window.
	dir := filepath.Join(root, "frontend")
	portConfig := transport.Config{}
	pin := pinTransportPort(&portConfig, dir, 0, resetTransportPortPin)
	server, err := frontendclient.Serve(frontendclient.Config{
		Profiles: profiles, ConfigDir: dir, ClientID: appservice.EnsureClientIDIn(dir),
		ComputerID: cfg.BackendID, Label: deviceLabel(), Version: version,
		Assets: cfg.Assets, Port: portConfig.Port, EphemeralPortFallback: portConfig.EphemeralPortFallback,
		SetWindowBackground: hooks.setBackground, ConfigureUpdater: hooks.configureUpdater,
	})
	if err != nil {
		pin.clearOnFailedBind(err)
		return nil, err
	}
	pin.adopt(server.Addr())
	return server, nil
}

// Pairing invitations are one-use, and the initially named computer may have
// been removed. Restart the local catalog instead of replaying that argument.
func frontendRelaunchArgs(dataDir string) []string {
	args := []string{"--frontend"}
	if dataDir != "" {
		args = append(args, "--data-dir", dataDir)
	}
	return args
}
