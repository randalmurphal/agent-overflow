package identity

import (
	"fmt"

	"agent-overflow/internal/appidentity"
)

// UpdateDeviceName changes only the calling session's paired-device metadata.
// The device ID is resolved here, never accepted from the client.
func (s *Sessions) UpdateDeviceName(sessionID, name, platform string) (bool, error) {
	session, reason := s.Live(sessionID)
	if reason.Refused() {
		return false, fmt.Errorf("device name: %s", reason)
	}
	device, err := s.store.GetDevice(session.DeviceID)
	if err != nil {
		return false, err
	}
	if device.Channel == LocalChannel {
		return false, fmt.Errorf("use this computer's device name setting")
	}
	name, err = appidentity.NormalizeDeviceName(name)
	if err != nil {
		return false, err
	}
	if name == "" {
		return false, fmt.Errorf("device name must not be empty")
	}
	platform, err = appidentity.NormalizeDeviceName(platform)
	if err != nil {
		return false, fmt.Errorf("device platform: %w", err)
	}
	if platform == "" {
		platform = device.Platform
	}
	if device.Label == name && device.Platform == platform {
		return false, nil
	}
	if err := s.store.RelabelDevice(device.ID, name, platform); err != nil {
		return false, err
	}
	return true, nil
}
