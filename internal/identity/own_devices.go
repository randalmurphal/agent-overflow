package identity

import (
	"errors"
	"reflect"

	"agent-overflow/internal/owndevices"
	"agent-overflow/internal/store"
)

func (s *Sessions) MergeOwnDevices(members []owndevices.Member) (bool, error) {
	return s.mergeOwnDevices(nil, members)
}

// MergeOwnDevicesAs rechecks the caller's admitted generation at the mutation
// boundary. A request authorized before removal cannot commit after removal.
func (s *Sessions) MergeOwnDevicesAs(sponsor owndevices.Member, members []owndevices.Member) (bool, error) {
	return s.mergeOwnDevices(&sponsor, members)
}
func (s *Sessions) mergeOwnDevices(sponsor *owndevices.Member, members []owndevices.Member) (bool, error) {
	s.ownMu.Lock()
	if sponsor != nil {
		if err := s.checkOwnSponsor(*sponsor); err != nil {
			s.ownMu.Unlock()
			return false, err
		}
	}
	changed, ids, err := s.store.MergeOwnDevices(members, s.now().UnixMilli(), s.backendID)
	if changed {
		s.forgetAll(ids)
	}
	s.ownMu.Unlock()
	for _, id := range ids {
		s.closeConns(id)
	}
	return changed, err
}
func (s *Sessions) checkOwnSponsor(sponsor owndevices.Member) error {
	current, err := s.store.OwnDevice(sponsor.KeyThumbprint)
	if err != nil || current.Removed || current.Generation != sponsor.Generation {
		return errors.New("own-device membership changed; reconnect before trying again")
	}
	return nil
}
func (s *Sessions) RegisterOwnDeviceAs(sponsor, member owndevices.Member) (bool, error) {
	s.ownMu.Lock()
	defer s.ownMu.Unlock()
	if err := s.checkOwnSponsor(sponsor); err != nil {
		return false, err
	}
	if sponsor.KeyThumbprint != member.KeyThumbprint {
		return false, errors.New("a device may register only its own identity")
	}
	current, err := s.store.OwnDevice(member.KeyThumbprint)
	if err != nil {
		return false, err
	}
	member.Generation, member.Removed = current.Generation, false
	if member.DeviceClass == "" {
		member.DeviceClass = current.DeviceClass
	}
	if reflect.DeepEqual(current, member) {
		return false, nil
	}
	err = s.store.RegisterOwnDevice(member)
	return err == nil, err
}

func (s *Sessions) ownIntroductionLive(link store.PairingLink) bool {
	m, e := s.store.OwnDevice(link.ExpectedKey)
	if e != nil || m.Removed || m.Generation != link.MemberGeneration {
		return false
	}
	sponsor, e := s.store.OwnDevice(link.SponsorKey)
	return e == nil && !sponsor.Removed
}
func (s *Sessions) MintOwnIntroduction(req PairingRequest) (PairingLink, error) {
	s.ownMu.Lock()
	if s.checkOwnSponsor(owndevices.Member{KeyThumbprint: req.SponsorKey, Generation: req.SponsorGeneration}) != nil || req.Purpose != "own-introduction" || !owndevices.ValidKey(req.ExpectedKey) || !s.ownIntroductionLive(store.PairingLink{ExpectedKey: req.ExpectedKey, MemberGeneration: req.MemberGeneration, SponsorKey: req.SponsorKey}) {
		s.ownMu.Unlock()
		return PairingLink{}, errors.New("own-device introduction is not authorized")
	}
	ids, err := s.store.RetireOwnIntroductions(req.ExpectedKey, s.now().UnixMilli())
	if err != nil {
		s.ownMu.Unlock()
		return PairingLink{}, err
	}
	s.forgetAll(ids)
	link, err := s.mintPairingLink(req)
	s.ownMu.Unlock()
	for _, id := range ids {
		s.closeConns(id)
	}
	return link, err
}

// RecoverOwnAdmissions finishes only already-approved durable generations.
func (s *Sessions) RecoverOwnAdmissions() error {
	s.ownMu.Lock()
	defer s.ownMu.Unlock()
	links, e := s.store.IncompleteOwnAdmissions()
	if e != nil {
		return e
	}
	for _, link := range links {
		if e = s.store.AdmitOwnPairing(link); e != nil {
			if _, e = s.RevokeSession(link.SessionID); e != nil {
				return e
			}
		}
	}
	return nil
}
