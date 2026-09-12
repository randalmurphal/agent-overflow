package deviceclient

import (
	"context"
	"errors"
	"time"
)

// renewWakeInterval bounds one wait of KeepRenewed. Timers count running
// time only on some platforms, so a wait sized to the whole access window
// would fire late by however long the machine slept. Waking this often and
// re-reading the wall clock keeps the rotation on time.
const renewWakeInterval = 30 * time.Second

// renewRetryDelay is how long KeepRenewed waits after a rotation that
// failed for a transient reason before trying again. A variable so a test
// can retry an outage without waiting it out.
var renewRetryDelay = 15 * time.Second

// KeepRenewed rotates the session shortly before each access window
// closes, for an owner whose sockets outlive one window. A renewal extends
// the session row the backend keys every open socket on, so a socket
// already carried stays authorized without being re-dialled. Ticket rotates
// only when a socket is dialled, which is not enough on its own: a socket
// that lives longer than the window would be refused for age on every call
// until the next dial.
//
// Transient failures are reported and retried until the rotation lands; the
// refresh secret outlives the access window by days, and an aged credential
// still renews. Returns ctx.Err() when stopped, and ErrSessionEnded once the
// session cannot rotate again, which retires the owner when the backend
// refused it for good.
func (c *Client) KeepRenewed(ctx context.Context, report func(error)) error {
	for {
		if c.Retired() {
			return ErrSessionEnded
		}
		if wait := c.untilRenewal(); wait > 0 {
			if err := sleepCtx(ctx, min(wait, renewWakeInterval)); err != nil {
				return err
			}
			continue
		}
		err := c.renew(ctx)
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrSessionEnded) {
			return err
		}
		report(err)
		if err := sleepCtx(ctx, renewRetryDelay); err != nil {
			return err
		}
	}
}

// untilRenewal is how long the held credential stays outside renewMargin
// of its expiry, zero when it is due. A session with no recorded expiry is
// never due; it is read again after one wake interval, since a rotation
// another path ran may have recorded one.
func (c *Client) untilRenewal() time.Duration {
	c.mu.Lock()
	expiresAt := c.session.ExpiresAtMs
	c.mu.Unlock()
	if expiresAt <= 0 {
		return renewWakeInterval
	}
	return time.UnixMilli(expiresAt).Add(-renewMargin).Sub(c.now())
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
