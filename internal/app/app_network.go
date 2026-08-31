package app

import (
	"fmt"

	"agent-overflow/internal/network"
	"agent-overflow/internal/transport"
)

// GetNetworkSettings returns the current persisted bind-all
// preference plus the server-derived URL and token. The URL and
// token are recomputed on every call so a rebind (e.g. via
// SetNetworkSettings) reflects immediately on the next read.
//
//ao:scope host
func (a *App) GetNetworkSettings() (network.Settings, error) {
	cfg := a.currentSettings()
	return network.FromServer(a.transportServer.Load(), cfg.Network.BindAll), nil
}

// SetNetworkSettings persists the new bind-all preference and
// rebinds the transport server. Going false → true rebinds to
// 0.0.0.0:<port> so LAN clients can reach the app; true → false
// rebinds back to 127.0.0.1:<port>. The port is reused so a
// previously-shared URL stays valid (only the host changes).
//
// On rebind failure the transport state is unchanged (Rebind is
// state-intact on error) and the persisted setting is rolled back
// so a subsequent GetNetworkSettings returns the actual transport
// state. Returns the post-rebind Settings so the UI can update the
// URL display in one round trip.
//
// Origin allow-list: a LAN bind requires an explicit allow-list so
// a stray browser tab on the LAN can't WebSocket-hijack a leaked
// token (CSWSH). On bind-all=true the list contains loopback
// variants plus the discovered LAN IP; on bind-all=false the list
// is empty (loopback has no browser-origin to validate, and
// InsecureSkipVerify is fine).
//
//ao:scope settings:write
//ao:stepup
func (a *App) SetNetworkSettings(s network.Settings) (network.Settings, error) {
	if a.settings == nil {
		return network.Settings{}, fmt.Errorf("settings service unavailable")
	}
	srv := a.transportServer.Load()
	if srv == nil {
		return network.Settings{}, fmt.Errorf("transport server unavailable")
	}

	prevCfg := a.settings.Get()
	if prevCfg.Network.BindAll == s.BindAll {
		// No change — return the current snapshot without touching
		// the transport. Keeps the binding idempotent for UIs that
		// fire SetNetworkSettings on every render.
		return network.FromServer(srv, prevCfg.Network.BindAll), nil
	}

	patch := map[string]any{
		"network": map[string]any{
			"bindAll": s.BindAll,
		},
	}
	if _, err := a.settings.Update(patch); err != nil {
		return network.Settings{}, fmt.Errorf("persist network settings: %w", err)
	}

	port := portFromAddr(srv.Addr())
	host := network.BindHost(s.BindAll)
	addr := fmt.Sprintf("%s:%d", host, port)

	// Compute the LAN IP once so the URL we report and the origin
	// we allow-list use the same value — otherwise the user could
	// see a URL their browser can't reach without an origin
	// failure.
	lanIP := ""
	if s.BindAll {
		lanIP = network.DiscoverLocalLANIP()
	}
	originPatterns := network.OriginPatterns(s.BindAll, lanIP)

	if err := srv.Rebind(addr, &transport.RebindOptions{OriginPatterns: originPatterns}); err != nil {
		// Roll the file back so we don't lie about the transport
		// state. The rollback uses the previously-persisted value,
		// not the patch input, so a partial Update doesn't strand
		// bind-all=true in settings while the transport runs on
		// loopback. Rebind is state-intact on failure (the
		// transport never moved), so the settings rollback is the
		// only state we need to undo.
		rollback := map[string]any{
			"network": map[string]any{
				"bindAll": prevCfg.Network.BindAll,
			},
		}
		if _, rbErr := a.settings.Update(rollback); rbErr != nil {
			return network.Settings{}, fmt.Errorf("rebind failed: %w (rollback also failed: %v)", err, rbErr)
		}
		return network.Settings{}, fmt.Errorf("rebind transport: %w", err)
	}

	return network.FromServerWithLAN(srv, s.BindAll, lanIP), nil
}
