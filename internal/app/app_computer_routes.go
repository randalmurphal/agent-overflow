package app

import (
	"slices"
	"sync"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/eventchan"
)

// Only the previous publication is retained, to avoid network-status polls
// invalidating every connected client when no usable address changed.
type computerRoutesPublication struct {
	mu     sync.Mutex
	routes []computerroute.Route
}

// Call after releasing the network-state locks. The event carries no route
// trust: delayed replay only asks clients for a fresh authenticated bootstrap.
func (a *App) publishComputerRoutes() {
	state := &a.computerRoutesPublished
	state.mu.Lock()
	routes := ComputerRoutes(a)
	changed := !slices.Equal(state.routes, routes)
	if changed {
		state.routes = routes
	}
	state.mu.Unlock()
	if changed {
		a.emit(eventchan.ComputerRoutesChanged, struct{}{})
	}
}

// GetComputerRoutes refreshes this computer's public addresses over an already
// authenticated socket when a listener move made HTTP bootstrap unreachable.
//
//ao:scope session
//ao:route selected
func (a *App) GetComputerRoutes() []computerroute.Route { return ComputerRoutes(a) }
