package deviceclient

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"time"

	"agent-overflow/internal/computerroute"
)

// RepairAddress verifies an explicitly entered address with existing computer
// trust, without exposing a credential or replacing the pairing. The caller
// receives success only after the verified alternative has been saved.
func (c *Client) RepairAddress(ctx context.Context, endpoint string) (computerroute.Route, error) {
	held := c.Session()
	candidates, err := computerroute.RepairCandidates(computerroute.Route{Endpoint: c.base, CertFingerprint: held.CertFingerprint}, held.Routes, endpoint)
	if err != nil {
		return computerroute.Route{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, routeProbeTimeout)
	defer cancel()
	var verified computerroute.Route
	for _, candidate := range candidates {
		target, _ := url.Parse(candidate.Endpoint)
		route := &dialRoute{Route: candidate, target: target, transport: NewPinnedTransport(candidate.CertFingerprint)}
		err = verifyComputerRoute(probeCtx, route, held.BackendID)
		closeIdleRoute(route)
		if err == nil {
			verified = candidate
			break
		}
		if probeCtx.Err() != nil {
			break
		}
	}
	if verified.Endpoint == "" {
		return computerroute.Route{}, fmt.Errorf("could not verify this computer at that address: %w", err)
	}
	var routes []computerroute.Route
	if err := c.sessionTransaction(ctx, func(path string, latest *Session) error {
		if latest.SessionID != held.SessionID {
			return errors.New("computer pairing changed while verifying its address")
		}
		allowed, err := computerroute.RepairCandidates(computerroute.Route{Endpoint: c.base, CertFingerprint: latest.CertFingerprint}, latest.Routes, endpoint)
		if err != nil || !slices.Contains(allowed, verified) {
			return errors.New("computer certificate trust changed while verifying its address; retry")
		}
		routes = computerroute.Merge(latest.Routes, []computerroute.Route{verified})
		latest.Routes = routes
		return writeSession(path, *latest)
	}); err != nil {
		return computerroute.Route{}, err
	}
	c.routes.update(routes)
	c.routes.mu.Lock()
	c.routes.failed = true
	c.routes.retryAt = time.Time{}
	c.routes.mu.Unlock()
	return verified, nil
}
