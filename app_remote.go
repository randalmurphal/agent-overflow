package main

import (
	"fmt"

	"agent-overflow/internal/settings"
)

// RemoteEndpointSummary is the wire shape returned by
// ListRemoteEndpoints. Aliased to the canonical settings type so the
// projection (and its token-redaction guarantee) lives next to the
// stored RemoteEndpoint shape — see settings.RemoteEndpoint.Summary.
type RemoteEndpointSummary = settings.RemoteEndpointSummary

// ListRemoteEndpoints returns every saved `--connect` target with the
// Token field stripped. The local UI fetches a token on-demand via
// GetRemoteEndpointToken when copying a launch command — that pattern
// keeps the bulk read path free of credentials so a remote token-
// holder enumerating saved endpoints can't grab tokens for unrelated
// backends.
func (a *App) ListRemoteEndpoints() ([]RemoteEndpointSummary, error) {
	if a.settings == nil {
		return nil, fmt.Errorf("settings service unavailable")
	}
	cfg := a.settings.Get()
	// Always return a non-nil slice so the JSON encoder emits `[]`
	// rather than `null` — the frontend's TypeScript types model the
	// list as `RemoteEndpointSummary[]` and a null would force every
	// caller to add a defensive coalesce.
	out := make([]RemoteEndpointSummary, 0, len(cfg.RemoteEndpoints))
	for _, ep := range cfg.RemoteEndpoints {
		out = append(out, ep.Summary())
	}
	return out, nil
}

// GetRemoteEndpointToken returns the token for a saved endpoint by ID.
// Split off ListRemoteEndpoints so the bulk-read path doesn't carry
// credentials. Used by the frontend's "Copy launch command" affordance,
// which is an explicit user action; ListRemoteEndpoints fires on every
// settings render and would otherwise leak tokens to any LAN-attached
// token-holder.
func (a *App) GetRemoteEndpointToken(id string) (string, error) {
	if a.settings == nil {
		return "", fmt.Errorf("settings service unavailable")
	}
	cfg := a.settings.Get()
	for _, ep := range cfg.RemoteEndpoints {
		if ep.ID == id {
			return ep.Token, nil
		}
	}
	return "", fmt.Errorf("remote endpoint %q not found", id)
}

// AddRemoteEndpoint persists a new --connect target. The settings
// service mints the ID; the operator-supplied name is optional (a
// blank nickname renders as the URL).
//
// SECURITY: returns the redacted Summary shape, not the raw stored
// record. A LAN-attached token-holder calling AddRemoteEndpoint with
// arbitrary inputs would otherwise see the persisted token echoed back
// (and Update with a no-op patch would harvest the existing token
// without writing). Forcing the return through Summary keeps both
// paths token-free; the local UI fetches the token explicitly via
// GetRemoteEndpointToken when the user copies the launch command.
func (a *App) AddRemoteEndpoint(name, url, token string) (RemoteEndpointSummary, error) {
	if a.settings == nil {
		return RemoteEndpointSummary{}, fmt.Errorf("settings service unavailable")
	}
	stored, err := a.settings.AddRemoteEndpoint(name, url, token)
	if err != nil {
		return RemoteEndpointSummary{}, err
	}
	return stored.Summary(), nil
}

// UpdateRemoteEndpoint mutates the named-by-ID record. Empty fields
// in the patch leave the existing value untouched, matching the
// settings-service semantics, so the UI can update a nickname
// without re-typing the URL or token.
//
// SECURITY: returns RemoteEndpointSummary so a no-op Update from a
// LAN-attached token-holder can't harvest the persisted token. See
// AddRemoteEndpoint for the threat model.
func (a *App) UpdateRemoteEndpoint(id, name, url, token string) (RemoteEndpointSummary, error) {
	if a.settings == nil {
		return RemoteEndpointSummary{}, fmt.Errorf("settings service unavailable")
	}
	stored, err := a.settings.UpdateRemoteEndpoint(id, name, url, token)
	if err != nil {
		return RemoteEndpointSummary{}, err
	}
	return stored.Summary(), nil
}

// DeleteRemoteEndpoint removes the named-by-ID record. Returns an
// error if the ID isn't found so a stale UI gets a clear signal
// rather than silently no-oping.
func (a *App) DeleteRemoteEndpoint(id string) error {
	if a.settings == nil {
		return fmt.Errorf("settings service unavailable")
	}
	return a.settings.DeleteRemoteEndpoint(id)
}

// TouchRemoteEndpoint bumps the LastUsedAt timestamp on the named
// record. Used by the settings UI's "Connect" affordance so the
// list can sort or visually emphasise recently-used endpoints.
func (a *App) TouchRemoteEndpoint(id string) error {
	if a.settings == nil {
		return fmt.Errorf("settings service unavailable")
	}
	return a.settings.TouchRemoteEndpoint(id)
}
