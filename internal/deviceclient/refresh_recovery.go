package deviceclient

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// RefreshRecoveryHeader is additive; absence means a legacy server. Keep its
// spelling in sync with the wire contract without importing the server.
const RefreshRecoveryHeader = "X-AO-Refresh-Recovery"

// refreshRecoverySupport probes only the already trusted endpoint, without
// credentials. An uncertain exchange must never downgrade to legacy rotation.
func (c *Client) refreshRecoverySupport(ctx context.Context, held Session) (bool, error) {
	if held.RefreshRecovery != nil {
		return *held.RefreshRecovery, nil
	}
	req, err := c.request(ctx, http.MethodGet, "/healthz", nil)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("deviceclient: check renewal support: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("deviceclient: check renewal support (HTTP %d)", resp.StatusCode)
	}
	if resp.Header.Get(RefreshRecoveryHeader) != "1" {
		return false, nil
	}
	var health struct {
		BackendID string `json:"backendId"`
	}
	if err := decodeBody(resp.Body, &health); err != nil {
		return false, err
	}
	if health.BackendID != held.BackendID {
		return false, errors.New("deviceclient: renewal endpoint belongs to a different computer")
	}
	return true, nil
}

func (c *Client) rotate(ctx context.Context, observed Session) error {
	// Read first: another process may already have renewed or renamed this file.
	var held Session
	if err := c.sessionTransaction(ctx, func(_ string, latest *Session) error { held = *latest; return nil }); err != nil {
		return err
	}
	if held.RefreshSecret != observed.RefreshSecret {
		return nil
	}
	if held.RefreshSecret == "" {
		return ErrSessionEnded
	}
	support, err := c.refreshRecoverySupport(ctx, held)
	if err != nil {
		return err
	}
	if !support {
		// Old hosts cannot recognize an identical retry. Serialize their exchange
		// across current client processes with a separate lock; profile transactions
		// and forgetting/renaming remain available while the network is slow.
		wait, cancel := context.WithTimeout(ctx, profileWriteTimeout)
		release, err := lockProfile(wait, c.dir, "legacy-renewal-"+held.BackendID)
		cancel()
		if err != nil {
			return err
		}
		defer release()
	}
	ready := false
	if err := c.sessionTransaction(ctx, func(path string, latest *Session) error {
		if latest.RefreshSecret != held.RefreshSecret {
			return nil
		}
		if latest.RefreshRecovery != nil {
			support = *latest.RefreshRecovery
		}
		if latest.PendingNextSecret != "" && !support {
			return errors.New("deviceclient: pending renewal requires a backend with renewal recovery support")
		}
		changed := latest.RefreshRecovery == nil
		latest.RefreshRecovery = &support
		if support && latest.PendingNextSecret == "" {
			secret := make([]byte, 32)
			if _, err := rand.Read(secret); err != nil {
				return err
			}
			latest.PendingNextSecret = base64.RawURLEncoding.EncodeToString(secret)
			changed = true
		}
		if changed {
			if err := writeSession(path, *latest); err != nil {
				return err
			}
		}
		held = *latest
		ready = true
		return nil
	}); err != nil {
		return err
	}
	if !ready {
		return nil
	}
	body, err := json.Marshal(struct {
		RefreshSecret     string `json:"refreshSecret"`
		NextRefreshSecret string `json:"nextRefreshSecret,omitempty"`
	}{held.RefreshSecret, held.PendingNextSecret})
	if err != nil {
		return err
	}
	path := authTokenPath
	if support {
		path = authTokenRecoverPath
	}
	issued, exchangeErr := c.exchange(ctx, path, body, &held)
	// Compare AFTER the network too. A suspended process's late grant/refusal
	// cannot roll back a newer generation, delete a re-pairing, or lose a rename.
	return c.sessionTransaction(ctx, func(path string, latest *Session) error {
		if !sameRenewal(*latest, held) {
			return nil
		}
		if exchangeErr != nil {
			var refusal *Refusal
			if !errors.As(exchangeErr, &refusal) {
				return exchangeErr
			}
			if refusal.Reason == reasonPendingConfirmation {
				return ErrAwaitingConfirmation
			}
			if !renewalRefusalEndsSession(refusal.Reason) {
				return exchangeErr
			}
			if err := removeSessionFile(path); err != nil {
				return err
			}
			c.retired = true
			c.session = Session{BackendID: latest.BackendID, Endpoint: latest.Endpoint}
			return fmt.Errorf("%w (%s)", ErrSessionEnded, refusal.Reason)
		}
		if issued.SessionID != held.SessionID || issued.RefreshSecret == "" || (held.PendingNextSecret != "" && issued.RefreshSecret != held.PendingNextSecret) {
			return errors.New("deviceclient: backend returned a different renewal; saved recovery state retained")
		}
		latest.Credential = issued.Credential
		latest.ExpiresAtMs = issued.ExpiresAtMs
		latest.RefreshSecret = issued.RefreshSecret
		latest.RefreshExpiresAtMs = issued.RefreshExpiresAtMs
		latest.PendingNextSecret = ""
		if issued.refreshRecovery {
			supported := true
			latest.RefreshRecovery = &supported
		}
		if len(issued.Scopes) > 0 {
			latest.Scopes = issued.Scopes
		}
		return writeSession(path, *latest)
	})
}

// Unknown future reasons are not evidence that this pairing ended. Keep the
// profile until a build that understands the verdict can act on it.
func renewalRefusalEndsSession(reason string) bool {
	switch reason {
	case "missing_proof", "malformed_proof", "key_mismatch", "invalid_signature",
		"unknown_session", "revoked_session", "expired_session", "unknown_credential",
		"revoked_device", "proof_not_bound", "proof_downgraded", "refresh_reused",
		"passkey_unavailable", "passkey_challenge_unknown", "passkey_refused":
		return true
	default:
		return false
	}
}
