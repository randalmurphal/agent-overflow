package attachedbackends

import (
	"context"
	"time"

	"agent-overflow/internal/transport"
)

// SyncDeviceName updates connected peers without delaying ordinary traffic.
// Unreachable peers retry when their next carried connection opens.
func (m *Manager) SyncDeviceName() {
	m.WakeOwnDevices()
	for _, profile := range m.Attached() {
		if held, err := m.carrier(profile.ID); err == nil {
			held.syncDeviceName()
		}
	}
}

func (c *carrier) syncDeviceName() {
	if c.labelGetter == nil || !c.nameSync.CompareAndSwap(false, true) {
		return
	}
	go func() {
		checkLatest := false
		lastAttempted := ""
		defer func() {
			c.nameSync.Store(false)
			// Release the worker before checking, so a notification either
			// starts a worker itself or is observed here. Retry a failed call
			// only for a newer name, never repeatedly for the same failure.
			if name, err := c.labelGetter(); err == nil && name != "" && name != c.client.Session().Label && (checkLatest || name != lastAttempted) {
				c.syncDeviceName()
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for ctx.Err() == nil {
			name, err := c.labelGetter()
			if err != nil {
				c.setNameError(err.Error())
				return
			}
			lastAttempted = name
			if name == "" || name == c.client.Session().Label {
				c.setNameError("")
				checkLatest = true
				return
			}
			if err := c.sendDeviceName(ctx, name); err != nil {
				c.setNameError("Device name saved locally. Waiting to update this computer: " + err.Error())
				return
			}
		}
	}()
}

func (c *carrier) sendDeviceName(ctx context.Context, name string) error {
	rpc, err := c.openRPC(ctx, transport.CapabilityDeviceName)
	if err != nil {
		return err
	}
	defer rpc.Close()
	if err := rpc.Call(ctx, "UpdateClientDeviceName", nil, name, c.platform); err != nil {
		return err
	}
	return c.client.SetDeviceLabel(name)
}

func (c *carrier) nameError() string {
	c.nameErrorMu.Lock()
	defer c.nameErrorMu.Unlock()
	return c.nameSyncError
}
func (c *carrier) setNameError(message string) {
	c.nameErrorMu.Lock()
	changed := c.nameSyncError != message
	c.nameSyncError = message
	c.nameErrorMu.Unlock()
	if changed && c.nameSyncChanged != nil {
		c.nameSyncChanged()
	}
}
