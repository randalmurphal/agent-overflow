package app

import (
	"context"
	"database/sql"
	"errors"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/owndevices"
	"agent-overflow/internal/transport"
)

type OwnDeviceList = owndevices.List
type OwnDeviceMember = owndevices.Member

func OwnDeviceSnapshot(a *App) (OwnDeviceList, error) {
	out := OwnDeviceList{Members: []OwnDeviceMember{}}
	if a.backends == nil {
		return out, nil
	}
	key, e := a.backends.OwnIdentity()
	if errors.Is(e, deviceclient.ErrNoDeviceKey) {
		return out, nil
	}
	if e != nil {
		return out, e
	}
	out.SelfKeyThumbprint = key
	self, e := a.store.OwnDevice(key)
	if errors.Is(e, sql.ErrNoRows) {
		return out, nil
	}
	if e != nil {
		return out, e
	}
	out.Enabled = !self.Removed
	out.ConnectedBackendIDs, e = a.backends.ConnectedOwnDeviceIDs()
	if e != nil {
		return out, e
	}
	out.ExcludedBackendIDs, e = a.backends.ExcludedOwnDeviceIDs()
	if e != nil {
		return out, e
	}
	out.Members, e = a.store.OwnDevices()
	if e == nil {
		if current, err := a.ownSelf(); err == nil {
			for i, m := range out.Members {
				if m.KeyThumbprint == key && !m.Removed {
					current.Generation = m.Generation
					out.Members[i] = current
				}
			}
		}
	}
	return out, e
}
func (a *App) ownSelf() (OwnDeviceMember, error) {
	if a.backends == nil {
		return OwnDeviceMember{}, errNoBackendProfiles
	}
	key, e := a.backends.OwnIdentity()
	if e != nil {
		return OwnDeviceMember{}, e
	}
	id, _ := a.backendIdentity()
	return OwnDeviceMember{DeviceClass: "desktop", KeyThumbprint: key, BackendID: id, Name: a.backendDisplayName(), Routes: ComputerRoutes(a), Generation: 1}, nil
}
func (a *App) enableOwnDevices() error {
	if a.backends == nil {
		return errNoBackendProfiles
	}
	if _, e := a.backends.EnrollOwnIdentity(); e != nil {
		return e
	}
	self, e := a.ownSelf()
	if e != nil {
		return e
	}
	if old, e := a.store.OwnDevice(self.KeyThumbprint); e == nil {
		self.Generation = old.Generation
		if old.Removed {
			self.Generation++
		}
	} else if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	_, e = MergeOwnDevices(a, []OwnDeviceMember{self})
	if e != nil {
		return e
	}
	return a.store.RegisterOwnDevice(self)
}
func MergeOwnDevices(a *App, members []OwnDeviceMember) (bool, error) {
	state, e := a.accessState()
	if e != nil {
		return false, e
	}
	changed, e := state.sessions.MergeOwnDevices(members)
	if changed {
		NotifyOwnDevices(a)
	}
	return changed, e
}

// MergeOwnDeviceSource is for an already authenticated direct carrier. The
// caller verifies source.SelfKeyThumbprint belongs to its pinned backend before
// refreshing that source's own metadata; forwarded metadata never replaces trust.
func MergeOwnDeviceSource(a *App, source OwnDeviceList) (bool, error) {
	if !source.Enabled {
		return false, nil
	}
	self, e := a.ownSelf()
	if e != nil {
		return false, e
	}
	members := append([]OwnDeviceMember(nil), source.Members...)
	found := false
	for _, m := range members {
		if m.KeyThumbprint == self.KeyThumbprint {
			found = true
		}
	}
	if !found {
		return false, errors.New("source did not admit this device")
	}
	if _, existing := a.store.OwnDevice(self.KeyThumbprint); errors.Is(existing, sql.ErrNoRows) {
		if e := a.ensureOwnDeviceHosting(); e != nil {
			return false, e
		}
	} else if existing != nil {
		return false, existing
	}
	changed, e := MergeOwnDevices(a, members)
	if e != nil {
		return false, e
	}
	for _, m := range members {
		if m.KeyThumbprint == source.SelfKeyThumbprint && !m.Removed {
			if e = a.store.RegisterOwnDevice(m); e != nil {
				return changed, e
			}
		}
	}
	if own, e := a.store.OwnDevice(self.KeyThumbprint); e == nil && !own.Removed {
		self.Generation = own.Generation
		e = a.store.RegisterOwnDevice(self)
		if e != nil {
			return changed, e
		}
	}
	return changed, nil
}
func NotifyOwnDevices(a *App) {
	a.emit(eventchan.OwnDevicesChanged, struct{}{})
	if a.backends != nil {
		a.backends.WakeOwnDevices()
	}
}
func (a *App) ownCaller(ctx context.Context) (OwnDeviceMember, error) {
	if transport.SessionFromContext(ctx) == "" {
		self, e := a.ownSelf()
		if e != nil {
			return OwnDeviceMember{}, e
		}
		return a.store.OwnDevice(self.KeyThumbprint)
	}
	state, e := a.accessState()
	if e != nil {
		return OwnDeviceMember{}, e
	}
	id := transport.SessionFromContext(ctx)
	session, reason := state.sessions.Live(id)
	if reason.Refused() {
		return OwnDeviceMember{}, errors.New("session is no longer active")
	}
	device, err := a.store.GetDevice(session.DeviceID)
	if err != nil {
		return OwnDeviceMember{}, err
	}
	if device.Channel == identity.LocalChannel {
		self, err := a.ownSelf()
		if err != nil {
			return OwnDeviceMember{}, err
		}
		return a.store.OwnDevice(self.KeyThumbprint)
	}
	m, e := a.store.OwnDeviceForSession(id)
	if e != nil {
		return OwnDeviceMember{}, e
	}
	if m.Removed {
		return OwnDeviceMember{}, errors.New("device is no longer a member")
	}
	return m, nil
}

//ao:scope session
//ao:route home
func (a *App) ListOwnDevices(ctx context.Context) (OwnDeviceList, error) {
	caller, e := a.ownCaller(ctx)
	if errors.Is(e, sql.ErrNoRows) || errors.Is(e, deviceclient.ErrNoDeviceKey) || e == errNoBackendProfiles {
		return OwnDeviceList{Members: []OwnDeviceMember{}}, nil
	}
	if e != nil {
		return OwnDeviceList{}, e
	}
	if caller.Removed {
		return OwnDeviceList{Members: []OwnDeviceMember{}}, nil
	}
	return OwnDeviceSnapshot(a)
}

//ao:scope session
//ao:route home
func (a *App) RegisterOwnDevice(ctx context.Context, member OwnDeviceMember) (OwnDeviceList, error) {
	caller, e := a.ownCaller(ctx)
	if e != nil {
		return OwnDeviceList{}, e
	}
	if member.KeyThumbprint != caller.KeyThumbprint {
		return OwnDeviceList{}, errors.New("a device may register only its own identity")
	}
	state, e := a.accessState()
	if e != nil {
		return OwnDeviceList{}, e
	}
	changed, e := state.sessions.RegisterOwnDeviceAs(caller, member)
	if e != nil {
		return OwnDeviceList{}, e
	}
	if changed {
		NotifyOwnDevices(a)
	}
	return OwnDeviceSnapshot(a)
}

//ao:scope session
//ao:route home
func (a *App) SyncOwnDevices(ctx context.Context, members []OwnDeviceMember) (OwnDeviceList, error) {
	caller, e := a.ownCaller(ctx)
	if e != nil {
		return OwnDeviceList{}, e
	}
	if caller.Removed {
		return OwnDeviceList{}, errors.New("device was removed")
	}
	if _, e = a.mergeOwnDevicesAs(caller, members); e != nil {
		return OwnDeviceList{}, e
	}
	return OwnDeviceSnapshot(a)
}

//ao:scope access:admin
//ao:route home
//ao:stepup
func (a *App) MintOwnDevicePairingOnNetwork(ctx context.Context, deviceClass, networkChoice string) (PairingInvite, error) {
	if err := a.ownPairingAdmin(ctx); err != nil {
		return PairingInvite{}, err
	}
	if networkChoice != "lan" && networkChoice != "tailnet" {
		return PairingInvite{}, errors.New("choose Local network or Tailscale")
	}
	if e := a.enableOwnDevices(); e != nil {
		return PairingInvite{}, e
	}
	return a.mintDevicePairingPurpose(deviceClass, "full", networkChoice, "own-device")
}

//ao:scope access:admin
//ao:route home
//ao:stepup
func (a *App) OpenOwnComputerPairing(ctx context.Context, networkChoice string) (ComputerPairingWindow, error) {
	if err := a.ownPairingAdmin(ctx); err != nil {
		return ComputerPairingWindow{}, err
	}
	if e := a.enableOwnDevices(); e != nil {
		return ComputerPairingWindow{}, e
	}
	return a.openComputerPairing(ctx, networkChoice, "full", "own-device")
}

//ao:scope session
//ao:route home
func (a *App) MintOwnDeviceIntroduction(ctx context.Context, keyThumbprint string) (PairingInvite, error) {
	caller, e := a.ownCaller(ctx)
	if e != nil {
		return PairingInvite{}, e
	}
	return a.mintOwnIntroduction(caller, keyThumbprint)
}
func (a *App) mintOwnIntroduction(caller OwnDeviceMember, key string) (PairingInvite, error) {
	target, e := a.store.OwnDevice(key)
	if e != nil || target.Removed {
		return PairingInvite{}, errors.New("target is not an active own device")
	}
	state, e := a.accessState()
	if e != nil {
		return PairingInvite{}, e
	}
	routes := ComputerRoutes(a)
	if len(routes) == 0 {
		return PairingInvite{}, errors.New("enable LAN access or finish connecting this computer to Tailscale before connecting its other devices")
	}
	endpoint, pin := routes[0].Endpoint, routes[0].CertFingerprint
	page := endpoint
	grants, e := identity.PairingAccess("full").Grants()
	if e != nil {
		return PairingInvite{}, e
	}
	link, e := state.sessions.MintOwnIntroduction(identity.PairingRequest{Purpose: "own-introduction", ExpectedKey: key, MemberGeneration: target.Generation, SponsorKey: caller.KeyThumbprint, SponsorGeneration: caller.Generation, UserID: state.owner.ID, DeviceClass: ownMemberClass(target), BindingClass: identity.BindingDeviceBound, Scopes: grants, CertFingerprint: pin})
	if e != nil {
		return PairingInvite{}, e
	}
	id, _ := a.backendIdentity()
	payload, e := identity.PairingPayload{Version: identity.PairingPayloadVersion, Purpose: "own-introduction", BackendID: id, BackendName: a.backendDisplayName(), Endpoint: endpoint, Token: link.Token, CertFingerprint: pin}.Encode()
	if e != nil {
		return PairingInvite{}, e
	}
	return PairingInvite{LinkID: link.Link.ID, URL: page + pairingFragmentPrefix + payload, ExpiresAtMs: link.Link.ExpiresAt}, nil
}

//ao:scope session
//ao:route home
func (a *App) IntroduceOwnDevice(ctx context.Context, targetBackendID string) (PairingInvite, error) {
	caller, e := a.ownCaller(ctx)
	if e != nil {
		return PairingInvite{}, e
	}
	if a.backends == nil {
		return PairingInvite{}, errNoBackendProfiles
	}
	rows, e := a.store.OwnDevices()
	if e != nil {
		return PairingInvite{}, e
	}
	found := false
	for _, m := range rows {
		if m.BackendID == targetBackendID && !m.Removed {
			found = true
		}
	}
	if !found {
		return PairingInvite{}, errors.New("computer is not an active own device")
	}
	snapshot, e := OwnDeviceSnapshot(a)
	if e != nil {
		return PairingInvite{}, e
	}
	var synced OwnDeviceList
	if e = a.backends.CallOwnDevice(ctx, targetBackendID, "SyncOwnDevices", &synced, snapshot.Members); e != nil {
		return PairingInvite{}, e
	}
	var invite PairingInvite
	e = a.backends.CallOwnDevice(ctx, targetBackendID, "MintOwnDeviceIntroduction", &invite, caller.KeyThumbprint)
	return invite, e
}

//ao:scope session
//ao:route home
func (a *App) AcceptOwnDeviceIntroduction(ctx context.Context, link string) error {
	_, e := a.ownCaller(ctx)
	if e != nil {
		return e
	}
	if a.backends == nil {
		return errNoBackendProfiles
	}
	payload, e := deviceclient.DecodeLink(link)
	if e != nil {
		return e
	}
	if payload.Purpose != "own-introduction" {
		return errors.New("invitation is not an own-device introduction")
	}
	members, e := a.store.OwnDevices()
	if e != nil {
		return e
	}
	var routes []computerroute.Route
	for _, member := range members {
		if !member.Removed && member.BackendID == payload.BackendID {
			routes = member.Routes
			break
		}
	}
	if len(routes) == 0 {
		return errors.New("introduction does not name an active own computer with reachable addresses")
	}
	// The session and key remain owned by the local manager. The target enforces
	// the exact recipient key before spending the invitation.
	added, e := a.backends.AcceptOwnDeviceIntroduction(ctx, link, routes)
	if e == nil && added {
		NotifyOwnDevices(a)
	}
	return e
}

func MintLocalOwnDeviceIntroduction(a *App, key string) (PairingInvite, error) {
	return a.MintOwnDeviceIntroduction(context.Background(), key)
}
func ownMemberClass(m OwnDeviceMember) identity.DeviceClass {
	if m.DeviceClass != "" {
		return identity.DeviceClass(m.DeviceClass)
	}
	return identity.DeviceDesktop
}
func (a *App) ownPairingAdmin(ctx context.Context) error {
	if id := transport.SessionFromContext(ctx); id == "" {
		return nil
	} else {
		state, e := a.accessState()
		if e != nil {
			return e
		}
		session, reason := state.sessions.Live(id)
		if reason.Refused() {
			return errors.New("session is no longer active")
		}
		device, e := a.store.GetDevice(session.DeviceID)
		if e != nil {
			return e
		}
		if device.Channel == identity.LocalChannel {
			return nil
		}
	}
	_, err := a.ownCaller(ctx)
	return err
}

// RemoveOwnDevice revokes this membership generation everywhere as peers
// reconnect. Local connection exclusions are separate and never call this RPC.
//
//ao:scope access:admin
//ao:route home
func (a *App) RemoveOwnDevice(ctx context.Context, keyThumbprint string) error {
	caller, e := a.ownCaller(ctx)
	if e != nil {
		return e
	}
	member, e := a.store.OwnDevice(keyThumbprint)
	if e != nil {
		return e
	}
	member.Removed = true
	_, e = a.mergeOwnDevicesAs(caller, []OwnDeviceMember{member})
	return e
}

// Personal enrollment authorizes the joining computer to host reciprocal
// connections. Use the same listener/native relay lifecycle as the settings UI.
func (a *App) ensureOwnDeviceHosting() error {
	settings := a.persistedNetworkSettings()
	if settings.BindAll {
		return nil
	}
	settings.BindAll = true
	_, err := a.SetNetworkSettings(context.Background(), settings)
	return err
}

// MintOwnDevicePairing is the network-automatic counterpart for local console
// and headless setup. Ordinary/limited invitations keep their existing method.
//
//ao:scope access:admin
//ao:route home
//ao:stepup
func (a *App) MintOwnDevicePairing(ctx context.Context, deviceClass string) (PairingInvite, error) {
	if e := a.ownPairingAdmin(ctx); e != nil {
		return PairingInvite{}, e
	}
	if e := a.enableOwnDevices(); e != nil {
		return PairingInvite{}, e
	}
	return a.mintDevicePairingPurpose(deviceClass, "full", "", "own-device")
}

func (a *App) mergeOwnDevicesAs(caller OwnDeviceMember, members []OwnDeviceMember) (bool, error) {
	state, e := a.accessState()
	if e != nil {
		return false, e
	}
	changed, e := state.sessions.MergeOwnDevicesAs(caller, members)
	if changed {
		NotifyOwnDevices(a)
	}
	return changed, e
}
