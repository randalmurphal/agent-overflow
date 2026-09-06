package app

import (
	"context"
	"fmt"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/transport"
)

// UpdateClientDeviceName publishes the name of the paired installation making
// this call. It cannot rename another device or change its access.
//
//ao:scope session
//ao:route home
func (a *App) UpdateClientDeviceName(ctx context.Context, name, platform string) error {
	state := a.identityState()
	if state == nil {
		return fmt.Errorf("device identity is unavailable")
	}
	changed, err := state.sessions.UpdateDeviceName(transport.SessionFromContext(ctx), name, platform)
	if err != nil {
		return err
	}
	if changed {
		a.emit(eventchan.AccessDevicesChanged, struct{}{})
	}
	return nil
}
