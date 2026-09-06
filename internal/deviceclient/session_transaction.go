package deviceclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// sessionTransaction reloads under the OS lock before changing a profile. The
// in-memory owner and the file must still name the same pairing and endpoint.
// Never perform network work inside change.
func (c *Client) sessionTransaction(ctx context.Context, change func(string, *Session) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.retired {
		return ErrSessionEnded
	}
	path, err := sessionPath(c.dir, c.session.BackendID)
	if err != nil {
		return err
	}
	release, err := lockProfile(ctx, c.dir, filepath.Base(path))
	if err != nil {
		return err
	}
	defer release()
	latest, err := readSession(path)
	if errors.Is(err, ErrNoSession) {
		c.retired = true
		return ErrSessionEnded
	}
	if err != nil {
		return err
	}
	if latest.SessionID != c.session.SessionID || latest.Endpoint != c.session.Endpoint || latest.CertFingerprint != c.session.CertFingerprint {
		c.retired = true
		return ErrSessionEnded
	}
	if err := change(path, &latest); err != nil {
		return err
	}
	c.session = latest
	return nil
}

func removeSessionFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func sameRenewal(a, b Session) bool {
	return a.SessionID == b.SessionID && a.RefreshSecret == b.RefreshSecret && a.PendingNextSecret == b.PendingNextSecret
}
