package identity

import (
	"agent-overflow/internal/owndevices"
	"agent-overflow/internal/store"
	"database/sql"
	"errors"
	"testing"
)

func personalPair(t *testing.T, s *Sessions, owner store.User, d *signingDevice) Redemption {
	t.Helper()
	link, e := s.MintPairingLink(PairingRequest{Purpose: "own-device", UserID: owner.ID, DeviceClass: DevicePhone, BindingClass: BindingDeviceBound, Scopes: []Scope{ScopeThreadsRead}})
	if e != nil {
		t.Fatal(e)
	}
	r, reason := s.RedeemPairing(RedemptionRequest{Token: link.Token, Label: "Phone", Proof: d.proof(t, "POST", "/auth/pair", link.Token, s.now())})
	if reason.Refused() {
		t.Fatal(reason)
	}
	if _, e = s.store.OwnDeviceForSession(r.Tokens.SessionID); !errors.Is(e, sql.ErrNoRows) {
		t.Fatal("pending pairing became a group member", e)
	}
	if _, e = s.ConfirmPairing(link.Link.ID); e != nil {
		t.Fatal(e)
	}
	return r
}
func ownIntro(t *testing.T, s *Sessions, owner store.User, from, to *signingDevice) PairingLink {
	t.Helper()
	target, e := s.store.OwnDevice(to.thumbprint())
	if e != nil {
		t.Fatal(e)
	}
	sponsor, e := s.store.OwnDevice(from.thumbprint())
	if e != nil {
		t.Fatal(e)
	}
	link, e := s.MintOwnIntroduction(PairingRequest{Purpose: "own-introduction", ExpectedKey: to.thumbprint(), MemberGeneration: target.Generation, SponsorKey: from.thumbprint(), SponsorGeneration: sponsor.Generation, UserID: owner.ID, DeviceClass: DevicePhone, BindingClass: BindingDeviceBound, Scopes: []Scope{ScopeThreadsRead}})
	if e != nil {
		t.Fatal(e)
	}
	return link
}
func redeemOwn(t *testing.T, s *Sessions, link PairingLink, d *signingDevice) Redemption {
	t.Helper()
	r, reason := s.RedeemPairing(RedemptionRequest{Token: link.Token, Proof: d.proof(t, "POST", "/auth/pair", link.Token+d.thumbprint(), s.now())})
	if reason.Refused() {
		t.Fatal(reason)
	}
	return r
}
func TestOwnDeviceAdmissionSeparatesLegacyAndBindsIntroductions(t *testing.T) {
	s, st, _, owner, _ := newFixture(t)
	member := newSigningDevice(t)
	legacy := newSigningDevice(t)
	_, tokens := keyPairedDevice(t, s, owner, legacy, s.now())
	if _, e := st.OwnDeviceForSession(tokens.SessionID); !errors.Is(e, sql.ErrNoRows) {
		t.Fatal("legacy pair entered own group", e)
	}
	own := personalPair(t, s, owner, member)
	if _, e := st.OwnDeviceForSession(own.Tokens.SessionID); e != nil {
		t.Fatal(e)
	}
	target := newSigningDevice(t)
	if _, e := s.MergeOwnDevices([]owndevices.Member{{KeyThumbprint: target.thumbprint(), Generation: 1}}); e != nil {
		t.Fatal(e)
	}
	link := ownIntro(t, s, owner, member, target)
	if _, reason := s.RedeemPairing(RedemptionRequest{Token: link.Token, Proof: legacy.proof(t, "POST", "/auth/pair", "wrong-recipient", s.now())}); reason != ReasonKeyMismatch {
		t.Fatal("wrong key could spend introduction", reason)
	}
	r := redeemOwn(t, s, link, target)
	if _, reason := s.Live(r.Tokens.SessionID); reason.Refused() {
		t.Fatal("introduced session not activated", reason)
	}
	if _, e := st.OwnDeviceForSession(r.Tokens.SessionID); e != nil {
		t.Fatal(e)
	}
	targetRow, e := st.OwnDevice(target.thumbprint())
	if e != nil {
		t.Fatal(e)
	}
	targetRow.Removed = true
	if _, e = s.MergeOwnDevices([]owndevices.Member{targetRow}); e != nil {
		t.Fatal(e)
	}
	if _, reason := s.Live(r.Tokens.SessionID); !reason.Refused() {
		t.Fatal("cached session survived membership removal")
	}
	if _, reason := s.Verify(r.Tokens.Credential); !reason.Refused() {
		t.Fatal("signed credential survived removal")
	}
	targetRow.Removed = false
	if changed, e := s.MergeOwnDevices([]owndevices.Member{targetRow}); e != nil || changed {
		t.Fatal("stale catalog restored revoked member", changed, e)
	}
	restored := personalPair(t, s, owner, target)
	restoredMember, e := st.OwnDeviceForSession(restored.Tokens.SessionID)
	if e != nil || restoredMember.Generation != 2 {
		t.Fatal("explicit pairing did not restore newer membership", restoredMember, e)
	}
	if _, reason := s.Live(r.Tokens.SessionID); !reason.Refused() {
		t.Fatal("restoration revived old session")
	}
}
func TestOwnIntroductionReplacementRetiresLostRepliesButPreservesAcknowledgedSession(t *testing.T) {
	s, st, _, owner, _ := newFixture(t)
	sponsor := newSigningDevice(t)
	target := newSigningDevice(t)
	personalPair(t, s, owner, sponsor)
	if _, e := s.MergeOwnDevices([]owndevices.Member{{KeyThumbprint: target.thumbprint(), Generation: 1}}); e != nil {
		t.Fatal(e)
	}
	first := redeemOwn(t, s, ownIntro(t, s, owner, sponsor, target), target)
	second := redeemOwn(t, s, ownIntro(t, s, owner, sponsor, target), target)
	if _, reason := s.Live(first.Tokens.SessionID); !reason.Refused() {
		t.Fatal("unacknowledged orphan stayed active")
	}
	renewed, reason := s.Refresh(RefreshRequest{Secret: second.Tokens.RefreshSecret, Proof: target.proof(t, "POST", "/auth/token", "acknowledge", s.now())})
	if reason.Refused() {
		t.Fatal(reason)
	}
	_ = ownIntro(t, s, owner, sponsor, target)
	if _, reason = s.Live(renewed.SessionID); reason.Refused() {
		t.Fatal("retry revoked acknowledged session", reason)
	}
	member, e := st.OwnDevice(sponsor.thumbprint())
	if e != nil {
		t.Fatal(e)
	}
	member.Removed = true
	pending := ownIntro(t, s, owner, sponsor, target)
	if _, e = s.MergeOwnDevices([]owndevices.Member{member}); e != nil {
		t.Fatal(e)
	}
	if _, reason := s.RedeemPairing(RedemptionRequest{Token: pending.Token, Proof: target.proof(t, "POST", "/auth/pair", "removed-sponsor", s.now())}); !reason.Refused() {
		t.Fatal("removed sponsor invitation admitted")
	}
}
func TestOwnDeviceRequiresSignedKeyAndRestrictedConstructor(t *testing.T) {
	s, _, _, owner, _ := newFixture(t)
	req := PairingRequest{Purpose: "own-device", UserID: owner.ID, DeviceClass: DevicePhone, BindingClass: BindingDeviceBound, Scopes: []Scope{ScopeThreadsRead}}
	link, e := s.MintPairingLink(req)
	if e != nil {
		t.Fatal(e)
	}
	if _, reason := s.RedeemPairing(RedemptionRequest{Token: link.Token, Proof: bearerProof("bare")}); !reason.Refused() {
		t.Fatal("bearer enrolled own device")
	}
	req.Purpose = "own-introduction"
	if _, e = s.MintPairingLink(req); e == nil {
		t.Fatal("ordinary constructor bypassed introduction authorization")
	}
}

func TestOwnMutationsRejectSponsorReadBeforeRevocation(t *testing.T) {
	s, st, _, owner, _ := newFixture(t)
	device := newSigningDevice(t)
	personalPair(t, s, owner, device)
	stale, err := st.OwnDevice(device.thumbprint())
	if err != nil {
		t.Fatal(err)
	}
	removed := stale
	removed.Removed = true
	if _, err := s.MergeOwnDevices([]owndevices.Member{removed}); err != nil {
		t.Fatal(err)
	}
	target := owndevices.Member{KeyThumbprint: newSigningDevice(t).thumbprint(), Generation: 1}
	if _, err := s.MergeOwnDevicesAs(stale, []owndevices.Member{target}); err == nil {
		t.Fatal("removed sponsor committed an admission")
	}
	if _, err := st.OwnDevice(target.KeyThumbprint); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("refused admission mutated catalog", err)
	}
	// Rejoining never makes a stale request from the old generation valid.
	personalPair(t, s, owner, device)
	stale.Name = "Stale request"
	if _, err := s.RegisterOwnDeviceAs(stale, stale); err == nil {
		t.Fatal("stale generation changed current metadata")
	}
	if _, err := s.MergeOwnDevicesAs(stale, []owndevices.Member{target}); err == nil {
		t.Fatal("stale generation admitted a device")
	}
	current, err := st.OwnDevice(device.thumbprint())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MergeOwnDevicesAs(current, []owndevices.Member{target}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MintOwnIntroduction(PairingRequest{Purpose: "own-introduction", ExpectedKey: target.KeyThumbprint, MemberGeneration: target.Generation, SponsorKey: stale.KeyThumbprint, SponsorGeneration: stale.Generation, UserID: owner.ID, DeviceClass: DevicePhone, BindingClass: BindingDeviceBound, Scopes: []Scope{ScopeThreadsRead}}); err == nil {
		t.Fatal("stale generation minted an introduction")
	}
}

func TestRemovingServingOwnHostRevokesAllPersonalInboundSessions(t *testing.T) {
	s, _, _, owner, _ := newFixture(t)
	s.backendID = "93fd7522-831e-4401-8aa0-6dca4e65bcb0"
	host := owndevices.Member{KeyThumbprint: newSigningDevice(t).thumbprint(), BackendID: s.backendID, Generation: 1}
	if _, err := s.MergeOwnDevices([]owndevices.Member{host}); err != nil {
		t.Fatal(err)
	}
	personal := personalPair(t, s, owner, newSigningDevice(t))
	_, ordinary := keyPairedDevice(t, s, owner, newSigningDevice(t), s.now())
	if _, reason := s.Live(personal.Tokens.SessionID); reason.Refused() {
		t.Fatal(reason)
	}
	pending, err := s.MintPairingLink(PairingRequest{Purpose: "own-device", UserID: owner.ID, DeviceClass: DevicePhone, BindingClass: BindingDeviceBound, Scopes: []Scope{ScopeThreadsRead}})
	if err != nil {
		t.Fatal(err)
	}
	host.Removed = true
	if _, err := s.MergeOwnDevices([]owndevices.Member{host}); err != nil {
		t.Fatal(err)
	}
	if _, reason := s.Live(personal.Tokens.SessionID); !reason.Refused() {
		t.Fatal("removed host kept inbound personal access")
	}
	late := newSigningDevice(t)
	if _, reason := s.RedeemPairing(RedemptionRequest{Token: pending.Token, Proof: late.proof(t, "POST", "/auth/pair", pending.Token, s.now())}); !reason.Refused() {
		t.Fatal("pending personal invitation survived host withdrawal")
	}
	if _, reason := s.Live(ordinary.SessionID); reason.Refused() {
		t.Fatal("ordinary share was revoked with the personal group", reason)
	}
}
