package app

import (
	"context"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/owndevices"
)

func (a *App) startOwnDeviceConnections() {
	if a.backends == nil {
		return
	}
	a.backends.StartOwnDevices(a.lifeCtx(), a.ownDeviceHooks())
}

func (a *App) ownDeviceHooks() attachedbackends.OwnDeviceHooks {
	return attachedbackends.OwnDeviceHooks{
		Snapshot: func() (owndevices.List, error) { return OwnDeviceSnapshot(a) },
		Accept:   func(source owndevices.List) (bool, error) { return MergeOwnDeviceSource(a, source) },
		Mint: func(_ context.Context, key string) (string, error) {
			invite, err := MintLocalOwnDeviceIntroduction(a, key)
			return invite.URL, err
		},
	}
}
