package deviceclient

import (
	"context"
	"errors"

	"agent-overflow/internal/computerroute"
)

// SelectPairingRoute verifies trusted public destinations before any invitation
// token or device proof crosses the wire. Enrollment still makes exactly one
// redemption request; a lost response must never retry a potentially spent link.
func SelectPairingRoute(ctx context.Context, backendID string, candidates []computerroute.Route, opts ...Option) (computerroute.Route, error) {
	if len(candidates) == 0 || len(candidates) > computerroute.MaxRoutes {
		return computerroute.Route{}, errors.New("deviceclient: pairing requires a bounded set of trusted computer routes")
	}
	routes := make([]*dialRoute, 0, len(candidates))
	for _, candidate := range candidates {
		route, err := computerroute.Normalize(candidate)
		if err != nil {
			return computerroute.Route{}, err
		}
		routes = append(routes, &dialRoute{Route: route, transport: NewPinnedTransport(route.CertFingerprint, opts...)})
	}
	ctx, cancel := context.WithTimeout(ctx, routeProbeTimeout)
	defer cancel()
	defer func() {
		for _, route := range routes {
			closeIdleRoute(route)
		}
	}()
	results := make(chan *dialRoute, len(routes))
	for _, route := range routes {
		go func() {
			if verifyComputerRoute(ctx, route, backendID) != nil {
				results <- nil
			} else {
				results <- route
			}
		}()
	}
	for range routes {
		select {
		case route := <-results:
			if route != nil {
				return route.Route, nil
			}
		case <-ctx.Done():
			return computerroute.Route{}, ctx.Err()
		}
	}
	return computerroute.Route{}, errors.New("deviceclient: no verified route to this computer is reachable")
}
