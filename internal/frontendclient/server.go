// Package frontendclient hosts a desktop frontend with no execution backend.
// Pairings and presentation services are local; every execution computer is an
// ordinary attached transport. Boot never waits for a remote computer.
package frontendclient

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"runtime"
	"sync"

	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/transport"
)

type Config struct {
	Profiles, ConfigDir, ClientID, ComputerID, Label, Version string
	Assets                                                    fs.FS
	Port                                                      int
	EphemeralPortFallback                                     bool
	SetWindowBackground                                       func(uint8, uint8, uint8)
	ConfigureUpdater                                          func(*appupdate.Service)
}

type Server struct {
	transport *transport.Server
	service   *service
	decorate  func(string) string
	stopOnce  sync.Once
}

func Serve(cfg Config) (*Server, error) {
	if cfg.Assets == nil || cfg.ConfigDir == "" {
		return nil, errors.New("frontend client: assets and a local configuration directory are required")
	}
	computers, err := attachedbackends.New(cfg.Profiles, cfg.Label, runtime.GOOS)
	if err != nil {
		return nil, err
	}
	bus := transport.NewEventBus(64)
	ctx, cancel := context.WithCancel(context.Background())
	services, err := newService(ctx, cancel, cfg, computers, bus)
	if err != nil {
		cancel()
		bus.Close()
		return nil, err
	}
	dispatcher := transport.NewDispatcher()
	// These are the existing App wire methods, backed by the same small
	// package services. No App, database, provider process or local project is
	// constructed. Unregistered execution methods fail at the dispatcher.
	if _, err := dispatcher.Register(services, transport.RegisterOptions{
		Package: "main", TypeName: "App", AllowList: transport.NewMethodAllowList(),
	}); err != nil {
		services.close()
		return nil, err
	}
	decorate := func(base string) string {
		u, err := url.Parse(base)
		if err != nil {
			return base
		}
		q := u.Query()
		q.Set("mode", "frontend")
		q.Set("cid", cfg.ClientID)
		if cfg.ComputerID != "" {
			q.Set("computer", cfg.ComputerID)
		}
		u.RawQuery = q.Encode()
		return u.String()
	}
	server, err := transport.New(transport.Config{
		BindAddr: "127.0.0.1", Port: cfg.Port, EphemeralPortFallback: cfg.EphemeralPortFallback,
		Dispatcher: dispatcher, EventBus: bus, Version: cfg.Version,
		AssetHandler: http.FileServerFS(cfg.Assets), AttachedBackends: computers,
		DecoratePageURL: decorate,
	})
	if err != nil {
		services.close()
		return nil, err
	}
	if err := server.Start(); err != nil {
		services.close()
		return nil, err
	}
	return &Server{transport: server, service: services, decorate: decorate}, nil
}

func (s *Server) AppURL() string                  { return s.decorate(s.transport.WebviewPageURL()) }
func (s *Server) Addr() string                    { return s.transport.Addr() }
func (s *Server) MintPageTicket() (string, error) { return s.transport.MintPageTicket() }
func (s *Server) Shutdown(ctx context.Context) error {
	// Cancel owned jobs before joining transport calls that may await them.
	s.stopOnce.Do(s.service.close)
	return s.transport.Shutdown(ctx)
}
