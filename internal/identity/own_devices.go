package identity

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/owndevices"
	"agent-overflow/internal/store"
)

// errOwnMembershipChanged refuses a sponsor that is no longer the member,
// at the generation, it was admitted as.
var errOwnMembershipChanged = errors.New("own-device membership changed; reconnect before trying again")

// errOwnIntroductionRefused refuses an introduction its request or the
// membership does not allow.
var errOwnIntroductionRefused = errors.New("own-device introduction is not authorized")

// OwnDevicesUnread answers an own-device check whose member or sponsor
// could not be read from storage: this computer's failure, not a refusal,
// so the same request can be made again.
func OwnDevicesUnread(err error) error {
	return errorsx.Public(ReasonTemporarilyUnavailable.Code(), "this computer could not read its own devices; try again", err)
}

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
	changed, ids, released, err := s.store.MergeOwnDevices(members, s.now().UnixMilli(), s.backendID)
	if changed {
		s.forgetAll(ids)
	}
	s.ownMu.Unlock()
	for _, id := range ids {
		s.closeConns(id)
	}
	s.linksReleased(released)
	return changed, err
}
func (s *Sessions) checkOwnSponsor(sponsor owndevices.Member) error {
	current, err := s.store.OwnDevice(sponsor.KeyThumbprint)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return OwnDevicesUnread(err)
	}
	if err != nil || current.Removed || current.Generation != sponsor.Generation {
		return errOwnMembershipChanged
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

// ownIntroductionLive reports whether the introduction's member is still at
// the generation it was minted for and its sponsor still a member. A member
// that is not in the group is an answer; any other failure to read one is
// returned.
func (s *Sessions) ownIntroductionLive(link store.PairingLink) (bool, error) {
	m, e := s.store.OwnDevice(link.ExpectedKey)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if m.Removed || m.Generation != link.MemberGeneration {
		return false, nil
	}
	sponsor, e := s.store.OwnDevice(link.SponsorKey)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	return e == nil && !sponsor.Removed, e
}
func (s *Sessions) MintOwnIntroduction(req PairingRequest) (PairingLink, error) {
	s.ownMu.Lock()
	if req.Purpose != "own-introduction" || !owndevices.ValidKey(req.ExpectedKey) {
		s.ownMu.Unlock()
		return PairingLink{}, errOwnIntroductionRefused
	}
	if err := s.checkOwnSponsor(owndevices.Member{KeyThumbprint: req.SponsorKey, Generation: req.SponsorGeneration}); err != nil {
		s.ownMu.Unlock()
		if errors.Is(err, errOwnMembershipChanged) {
			err = errOwnIntroductionRefused
		}
		return PairingLink{}, err
	}
	live, err := s.ownIntroductionLive(store.PairingLink{ExpectedKey: req.ExpectedKey, MemberGeneration: req.MemberGeneration, SponsorKey: req.SponsorKey})
	if err != nil || !live {
		s.ownMu.Unlock()
		if err != nil {
			return PairingLink{}, OwnDevicesUnread(err)
		}
		return PairingLink{}, errOwnIntroductionRefused
	}
	ids, released, err := s.store.RetireOwnIntroductions(req.ExpectedKey, s.now().UnixMilli())
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
	s.linksReleased(released)
	return link, err
}

// RecoverOwnAdmissions finishes only already-approved durable generations. An
// admission the membership refuses (store.ErrOwnAdmissionRefused) can never
// complete, so its session is revoked; any other failure leaves the approved
// session and its link for the next start to finish, and is returned with
// every other link still tried.
func (s *Sessions) RecoverOwnAdmissions() error {
	s.ownMu.Lock()
	defer s.ownMu.Unlock()
	links, e := s.store.IncompleteOwnAdmissions()
	if e != nil {
		return e
	}
	var failed []error
	for _, link := range links {
		e = s.store.AdmitOwnPairing(link)
		switch {
		case errors.Is(e, store.ErrOwnAdmissionRefused):
			if _, e = s.RevokeSession(link.SessionID); e != nil {
				failed = append(failed, fmt.Errorf("revoke the refused own-device admission of link %s: %w", link.ID, e))
			}
		case e != nil:
			failed = append(failed, fmt.Errorf("finish the own-device admission of link %s: %w", link.ID, e))
		}
	}
	return errors.Join(failed...)
}
