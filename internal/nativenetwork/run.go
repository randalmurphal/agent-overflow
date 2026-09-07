package nativenetwork

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"time"

	"agent-overflow/internal/nearby"
)

type Bridge interface {
	GetNativeNetworkConfig(context.Context) (Config, error)
	ReportNativeNetworkState(context.Context, State) error
}

// Run binds only while the authenticated backend explicitly enables LAN access.
// Configuration, errors and scan results use the existing owner RPC connection.
// A lost bridge closes the relay before its next bounded retry.
func Run(ctx context.Context, bridge Bridge) {
	host := hostNetwork{}
	defer host.close()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		config, err := bridge.GetNativeNetworkConfig(ctx)
		if err != nil {
			host.close()
		} else {
			state := host.reconcile(ctx, config)
			if err := bridge.ReportNativeNetworkState(ctx, state); err != nil {
				host.close()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type hostNetwork struct {
	relay      *Relay
	advertiser *nearby.Server
	config     Config
	interfaces []string
	state      State
}

func (h *hostNetwork) close() {
	if h.advertiser != nil {
		h.advertiser.Close()
		h.advertiser = nil
	}
	if h.relay != nil {
		h.relay.Close()
		h.relay = nil
	}
	h.interfaces = nil
	h.config = Config{}
	h.state = State{}
}
func (h *hostNetwork) reconcile(ctx context.Context, config Config) State {
	interfaces, err := LANAddresses()
	if err != nil {
		h.close()
		return State{Error: err.Error(), ScanID: config.ScanID, Generation: config.Generation}
	}
	changed := config.Generation != h.config.Generation || config.Enabled != h.config.Enabled || config.Target != h.config.Target || !slices.Equal(interfaces, h.interfaces)
	if changed {
		h.close()
	}
	h.interfaces = interfaces
	var failures []error
	if config.Enabled {
		target, err := url.Parse(config.Target)
		if err != nil {
			failures = append(failures, err)
		} else if _, _, err := relayTarget(config.Target); err != nil {
			failures = append(failures, err)
		} else if slices.Contains(interfaces, target.Hostname()) {
			// WSL mirrored mode already shares this Windows interface. Binding a
			// second listener at the same endpoint would compete with the backend.
			h.state.Addresses = []string{config.Target}
		} else {
			if h.relay == nil {
				h.relay, err = StartRelay(ctx, config.Target, interfaces)
			}
			if err != nil {
				failures = append(failures, err)
			} else {
				h.state.Addresses = h.relay.Addresses()
				if err := h.relay.Err(); err != nil {
					failures = append(failures, err)
				}
			}
		}
	}
	advertisementChanged := changed || config.PairingOpen != h.config.PairingOpen || config.Name != h.config.Name || config.BackendID != h.config.BackendID
	if advertisementChanged && h.advertiser != nil {
		h.advertiser.Close()
		h.advertiser = nil
	}
	if config.Enabled && config.PairingOpen && len(h.state.Addresses) > 0 && h.advertiser == nil {
		addresses := make([]string, 0, len(h.state.Addresses))
		port := 0
		for _, address := range h.state.Addresses {
			u, _ := url.Parse(address)
			addresses = append(addresses, u.Hostname())
			port, _ = strconv.Atoi(u.Port())
		}
		h.advertiser, err = nearby.Start(nearby.Advertisement{BackendID: config.BackendID, Name: func() string { return config.Name }, Port: port, Addresses: addresses})
		if err != nil {
			failures = append(failures, fmt.Errorf("nearby discovery unavailable: %v", err))
		}
	}
	if config.ScanID != 0 && config.ScanID != h.state.ScanID {
		h.state.Nearby, err = nearby.Discover(ctx)
		h.state.ScanID = config.ScanID
		if err != nil {
			failures = append(failures, err)
		}
	}
	h.config = config
	h.state.Generation = config.Generation
	h.state.Error = ""
	if err := errors.Join(failures...); err != nil {
		h.state.Error = err.Error()
	}
	return h.state
}
