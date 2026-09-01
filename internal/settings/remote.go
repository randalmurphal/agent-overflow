package settings

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// RemoteEndpoint is one stored `--connect` target. The desktop binary
// takes a URL+token from this list (or from --connect on the command
// line) and points the Wails webview at the remote backend instead of
// booting a local transport.
//
// IDs are opaque strings the settings layer mints; the UI keys list
// rows by ID so a rename/edit doesn't require re-targeting the
// underlying record. LastUsedAt is updated by the settings UI's
// "Connect" affordance — the settings layer doesn't observe runtime
// connection state.
//
// SECURITY: this struct is the on-disk persistence shape — it carries
// the plaintext Token because the launcher needs it when the user
// chooses to --connect. It MUST NOT be returned directly to the wire;
// the bound App methods in app_remote.go project it onto
// RemoteEndpointSummary (no Token field) before crossing the
// transport boundary, with a single explicit GetRemoteEndpointToken
// path for token retrieval. Adding a JSON tag here that hides Token
// would break persistence; the protection lives at the wire shape
// instead.
type RemoteEndpoint struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	Token      string `json:"token"`
	LastUsedAt int64  `json:"lastUsedAt,omitempty"`
}

// RemoteEndpointSummary is the token-redacted wire shape returned by
// the bound `ListRemoteEndpoints` / `AddRemoteEndpoint` /
// `UpdateRemoteEndpoint` App methods. The Token field is structurally
// absent so a LAN-attached token-holder cannot harvest credentials for
// other backends through the bulk list path — token retrieval goes
// through the dedicated server-logged GetRemoteEndpointToken method.
//
// Keeping the projection here (rather than at the App boundary) makes
// it impossible for a future field on RemoteEndpoint that needs
// special handling to slip onto the wire without going through this
// type.
type RemoteEndpointSummary struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	LastUsedAt int64  `json:"lastUsedAt,omitempty"`
}

// Summary projects a stored RemoteEndpoint onto its token-redacted
// wire shape. Centralised so every wire-bound caller goes through the
// same projection — a future RemoteEndpoint field that needs redaction
// gets handled in one place.
func (ep RemoteEndpoint) Summary() RemoteEndpointSummary {
	return RemoteEndpointSummary{
		ID:         ep.ID,
		Name:       ep.Name,
		URL:        ep.URL,
		LastUsedAt: ep.LastUsedAt,
	}
}

// remoteEndpointIDByteLen sizes the random ID for a stored endpoint.
// 8 bytes -> 64 bits is enough to make collisions effectively
// impossible for any sane number of saved endpoints, while keeping
// the base64url string short.
const remoteEndpointIDByteLen = 8

// NewRemoteEndpointID returns a fresh opaque ID. Exposed so call sites
// (App methods that mint records) and tests can mint IDs through the
// same path; manual ID assignment is intentionally not supported on
// AddRemoteEndpoint.
func NewRemoteEndpointID() (string, error) {
	buf := make([]byte, remoteEndpointIDByteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("settings: generate remote endpoint id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ValidateRemoteEndpointURL trims and verifies that url is a ws:// or
// wss:// URL with a non-empty host. Used by both AddRemoteEndpoint and
// the `--connect` URL parser so the validation rule lives in one
// place.
func ValidateRemoteEndpointURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("remote endpoint url cannot be empty")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("remote endpoint url invalid: %w", err)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return "", fmt.Errorf("remote endpoint url scheme must be ws:// or wss://, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", errors.New("remote endpoint url missing host")
	}
	return trimmed, nil
}

// ValidateRemoteEndpointToken trims and verifies the token. Empty
// tokens are rejected — every transport boot mints a non-empty token,
// and a record without one would never authenticate.
func ValidateRemoteEndpointToken(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("remote endpoint token cannot be empty")
	}
	return trimmed, nil
}

// AddRemoteEndpoint appends a new endpoint to the persisted list. The
// service mints the ID and timestamp so callers don't have to. Returns
// the new record so the UI can render it without re-fetching the full
// list.
//
// Concurrent AddRemoteEndpoint / UpdateRemoteEndpoint / DeleteRemoteEndpoint
// calls are serialised under the same mutex Update uses; the underlying
// loadFromFile / writeSparse pair is the canonical write path.
func (s *Service) AddRemoteEndpoint(name, rawURL, token string) (RemoteEndpoint, error) {
	cleanURL, err := ValidateRemoteEndpointURL(rawURL)
	if err != nil {
		return RemoteEndpoint{}, err
	}
	cleanToken, err := ValidateRemoteEndpointToken(token)
	if err != nil {
		return RemoteEndpoint{}, err
	}
	id, err := NewRemoteEndpointID()
	if err != nil {
		return RemoteEndpoint{}, err
	}

	next := RemoteEndpoint{
		ID:    id,
		Name:  strings.TrimSpace(name),
		URL:   cleanURL,
		Token: cleanToken,
	}
	if _, err := s.mutate("", DeviceDesktop, func(current Settings) (Settings, error) {
		current.RemoteEndpoints = append(current.RemoteEndpoints, next)
		return current, nil
	}); err != nil {
		return RemoteEndpoint{}, err
	}
	return next, nil
}

// UpdateRemoteEndpoint mutates the named-by-ID record. Empty fields on
// the patch leave the existing value unchanged so the UI can update
// only the nickname without re-typing the URL/token; pass an explicit
// non-empty string to overwrite.
//
// Validation runs against the post-merge values so a patch that only
// updates the nickname doesn't have to re-validate the existing URL.
// Returns the updated record on success.
func (s *Service) UpdateRemoteEndpoint(id, name, rawURL, token string) (RemoteEndpoint, error) {
	if strings.TrimSpace(id) == "" {
		return RemoteEndpoint{}, errors.New("remote endpoint id required")
	}

	var updated RemoteEndpoint
	if _, err := s.mutate("", DeviceDesktop, func(current Settings) (Settings, error) {
		idx := indexRemoteEndpointByID(current.RemoteEndpoints, id)
		if idx < 0 {
			return Settings{}, fmt.Errorf("remote endpoint %q not found", id)
		}

		updated = current.RemoteEndpoints[idx]
		if strings.TrimSpace(name) != "" {
			updated.Name = strings.TrimSpace(name)
		}
		if strings.TrimSpace(rawURL) != "" {
			clean, err := ValidateRemoteEndpointURL(rawURL)
			if err != nil {
				return Settings{}, err
			}
			updated.URL = clean
		}
		if strings.TrimSpace(token) != "" {
			clean, err := ValidateRemoteEndpointToken(token)
			if err != nil {
				return Settings{}, err
			}
			updated.Token = clean
		}

		current.RemoteEndpoints[idx] = updated
		return current, nil
	}); err != nil {
		return RemoteEndpoint{}, err
	}
	return updated, nil
}

// DeleteRemoteEndpoint removes the named-by-ID record. Returns an
// error if the ID isn't found so a UI that was looking at a stale list
// gets a clear signal rather than silently no-oping.
func (s *Service) DeleteRemoteEndpoint(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("remote endpoint id required")
	}

	_, err := s.mutate("", DeviceDesktop, func(current Settings) (Settings, error) {
		idx := indexRemoteEndpointByID(current.RemoteEndpoints, id)
		if idx < 0 {
			return Settings{}, fmt.Errorf("remote endpoint %q not found", id)
		}

		current.RemoteEndpoints = append(current.RemoteEndpoints[:idx], current.RemoteEndpoints[idx+1:]...)
		if len(current.RemoteEndpoints) == 0 {
			// Drop the slice entirely so writeSparse omits the key from JSON.
			current.RemoteEndpoints = nil
		}
		return current, nil
	})
	return err
}

// TouchRemoteEndpoint updates the LastUsedAt timestamp on the named
// record. Best-effort: a missing ID is a no-op error rather than a
// panic so a UI that fires this opportunistically doesn't have to
// pre-check membership. Used by the "Connect" affordance in the
// settings UI to bubble recently-used endpoints to the top of the list.
func (s *Service) TouchRemoteEndpoint(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("remote endpoint id required")
	}

	_, err := s.mutate("", DeviceDesktop, func(current Settings) (Settings, error) {
		idx := indexRemoteEndpointByID(current.RemoteEndpoints, id)
		if idx < 0 {
			return Settings{}, fmt.Errorf("remote endpoint %q not found", id)
		}
		current.RemoteEndpoints[idx].LastUsedAt = time.Now().Unix()
		return current, nil
	})
	return err
}

// indexRemoteEndpointByID returns the slice index of the record with
// the matching ID, or -1 when not found. Linear scan — the list is
// small (a handful of saved endpoints in practice) so a map index
// would cost more than it saved.
func indexRemoteEndpointByID(list []RemoteEndpoint, id string) int {
	for i, ep := range list {
		if ep.ID == id {
			return i
		}
	}
	return -1
}
