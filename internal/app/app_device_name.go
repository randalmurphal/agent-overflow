package app

import (
	"log"

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/assetwatch"
	"agent-overflow/internal/eventchan"
)

func SetDeviceNameIdentity(a *App, name *appidentity.DeviceName) { a.deviceName = name }
func DeviceDisplayName(a *App) string                            { return a.backendDisplayName() }

// GetDeviceName returns this installation's advertised name.
//
//ao:scope session
//ao:route home
func (a *App) GetDeviceName() (string, error) {
	if a.deviceName != nil {
		return a.deviceName.Get()
	}
	return appidentity.NewDeviceName(a.configDir).Get()
}

// SetDeviceName changes display metadata without changing identity or access.
//
//ao:scope access:admin
//ao:route home
func (a *App) SetDeviceName(name string) error {
	identity := a.deviceName
	if identity == nil {
		identity = appidentity.NewDeviceName(a.configDir)
	}
	if err := identity.Set(name); err != nil {
		return err
	}
	current, err := identity.Get()
	if err != nil {
		return err
	}
	if a.deviceNameWatcher == nil {
		a.deviceNameChanged(current)
	}
	return nil
}

func (a *App) deviceNameChanged(name string) {
	if a.backends != nil {
		a.backends.SyncDeviceName()
	}
	a.emit(eventchan.BackendNameChanged, map[string]string{"name": name})
}
func (a *App) startDeviceNameWatcher() {
	if a.backends != nil {
		a.backends.SetNameSyncChanged(func(id string) {
			a.emit(eventchan.BackendSetChanged, map[string]string{"action": "device-name-sync", "id": id})
		})
	}

	if a.deviceName == nil {
		a.deviceName = appidentity.NewDeviceName(a.configDir)
	}
	if a.deviceName.Path() == "" {
		return
	}
	watcher, err := assetwatch.NewDeviceNameWatcher(a.deviceName.Path(), func() {
		name, err := a.GetDeviceName()
		if err != nil {
			log.Printf("device name: %v", err)
			return
		}
		a.deviceNameChanged(name)
	})
	if err != nil {
		log.Printf("device name watcher: %v", err)
		return
	}
	a.deviceNameWatcher = watcher
}
