package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// seedPasskey writes one credential for an account, filling every column a
// ceremony would.
func seedPasskey(t *testing.T, s *Store, userID, id, label string, credentialID []byte) Passkey {
	t.Helper()
	passkey := Passkey{
		ID:                id,
		UserID:            userID,
		Label:             label,
		CredentialID:      credentialID,
		PublicKey:         []byte("cose-public-key"),
		AttestationType:   "none",
		AttestationFormat: "none",
		Transports:        []string{"internal", "hybrid"},
		AAGUID:            make([]byte, 16),
		Attachment:        "platform",
		RPID:              "localhost",
		SignCount:         3,
		UserVerified:      true,
		BackupEligible:    true,
		BackupState:       true,
		CreatedAt:         1000,
	}
	if err := s.CreatePasskey(passkey); err != nil {
		t.Fatalf("CreatePasskey: %v", err)
	}
	return passkey
}

func TestPasskeyRoundTripsEveryColumn(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	written := seedPasskey(t, s, owner.ID, "pk-1", "Phone", []byte("cred-a"))

	read, err := s.PasskeyByCredentialID([]byte("cred-a"))
	if err != nil {
		t.Fatalf("PasskeyByCredentialID: %v", err)
	}
	if read.ID != written.ID || read.UserID != owner.ID || read.Label != "Phone" {
		t.Fatalf("identity columns did not survive: %+v", read)
	}
	if string(read.PublicKey) != "cose-public-key" || read.RPID != "localhost" {
		t.Fatalf("key material did not survive: %+v", read)
	}
	if len(read.Transports) != 2 || read.Transports[0] != "internal" {
		t.Fatalf("transports did not survive: %#v", read.Transports)
	}
	if len(read.AAGUID) != 16 || read.Attachment != "platform" {
		t.Fatalf("authenticator columns did not survive: %+v", read)
	}
	if read.SignCount != 3 || read.CloneWarning {
		t.Fatalf("counter columns did not survive: %+v", read)
	}
	// The three latching flags are the ones a decision later reads, so a
	// column that silently dropped one would change what a login means.
	if !read.UserVerified || !read.BackupEligible || !read.BackupState {
		t.Fatalf("flags did not survive: %+v", read)
	}
	if read.CreatedAt != 1000 || read.LastUsedAt != 0 {
		t.Fatalf("stamps did not survive: %+v", read)
	}
}

func TestPasskeyCredentialIDIsUniqueAcrossAccounts(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	seedPasskey(t, s, owner.ID, "pk-1", "Phone", []byte("cred-a"))

	member, err := s.CreateUser("Member")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	second := Passkey{
		ID: "pk-2", UserID: member.ID, Label: "Same key", CredentialID: []byte("cred-a"),
		PublicKey: []byte("k"), RPID: "localhost", CreatedAt: 2000,
	}
	if err := s.CreatePasskey(second); err == nil {
		t.Fatal("a second account claiming one authenticator credential must be refused")
	}
}

func TestPasskeyRequiresTheFieldsARowIsMeaninglessWithout(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	full := func() Passkey {
		return Passkey{
			ID: "pk-1", UserID: owner.ID, Label: "Phone", CredentialID: []byte("c"),
			PublicKey: []byte("k"), RPID: "localhost", CreatedAt: 1,
		}
	}
	cases := map[string]func(*Passkey){
		"id":            func(p *Passkey) { p.ID = "" },
		"user id":       func(p *Passkey) { p.UserID = " " },
		"label":         func(p *Passkey) { p.Label = "" },
		"credential id": func(p *Passkey) { p.CredentialID = nil },
		"public key":    func(p *Passkey) { p.PublicKey = nil },
		"rp id":         func(p *Passkey) { p.RPID = "" },
	}
	for name, blank := range cases {
		passkey := full()
		blank(&passkey)
		err := s.CreatePasskey(passkey)
		if !errors.Is(err, ErrIdentityFieldRequired) {
			t.Fatalf("a passkey with no %s must be refused, got %v", name, err)
		}
	}
}

func TestRecordPasskeyAssertionWritesTheCounterVerdictTogether(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	seedPasskey(t, s, owner.ID, "pk-1", "Phone", []byte("cred-a"))

	if err := s.RecordPasskeyAssertion("pk-1", 9, true, false, 5000); err != nil {
		t.Fatalf("RecordPasskeyAssertion: %v", err)
	}
	read, err := s.PasskeyByCredentialID([]byte("cred-a"))
	if err != nil {
		t.Fatalf("PasskeyByCredentialID: %v", err)
	}
	if read.SignCount != 9 || !read.CloneWarning || read.BackupState {
		t.Fatalf("the assertion's report did not land: %+v", read)
	}
	if read.LastUsedAt != 5000 {
		t.Fatalf("last used stamp did not land: %+v", read)
	}
	// Eligibility latches: an assertion may report a backup STATE, never a
	// change to whether this credential could ever be backed up.
	if !read.BackupEligible {
		t.Fatal("an assertion must not be able to clear backup eligibility")
	}
	if err := s.RecordPasskeyAssertion("pk-missing", 1, false, false, 1); err == nil {
		t.Fatal("recording an assertion for no row must fail loudly")
	}
}

func TestDeletePasskeyIsScopedToItsAccountAndIdempotent(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	seedPasskey(t, s, owner.ID, "pk-1", "Phone", []byte("cred-a"))
	member, err := s.CreateUser("Member")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	removed, err := s.DeletePasskey(member.ID, "pk-1")
	if err != nil {
		t.Fatalf("DeletePasskey: %v", err)
	}
	if removed {
		t.Fatal("an account must not be able to delete another account's credential")
	}

	removed, err = s.DeletePasskey(owner.ID, "pk-1")
	if err != nil || !removed {
		t.Fatalf("the owner's own delete must remove the row: %v %v", removed, err)
	}
	removed, err = s.DeletePasskey(owner.ID, "pk-1")
	if err != nil || removed {
		t.Fatalf("a second delete is a no-op, not an error: %v %v", removed, err)
	}
}

func TestPasskeysCascadeWithTheirAccount(t *testing.T) {
	s := newTestStore(t)
	member, err := s.CreateUser("Member")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	seedPasskey(t, s, member.ID, "pk-1", "Phone", []byte("cred-a"))
	if _, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, member.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := s.PasskeyByCredentialID([]byte("cred-a")); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a deleted account must take its credentials with it, got %v", err)
	}
}

func TestListPasskeysCarriesARowWhoseRelyingPartyMoved(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	seedPasskey(t, s, owner.ID, "pk-1", "Phone", []byte("cred-a"))
	stale := Passkey{
		ID: "pk-2", UserID: owner.ID, Label: "Old domain", CredentialID: []byte("cred-b"),
		PublicKey: []byte("k"), RPID: "was.example.com", CreatedAt: 2000,
	}
	if err := s.CreatePasskey(stale); err != nil {
		t.Fatalf("CreatePasskey: %v", err)
	}

	listed, err := s.ListPasskeysForUser(owner.ID)
	if err != nil {
		t.Fatalf("ListPasskeysForUser: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("a credential registered under another RP ID must still be listed, got %d", len(listed))
	}
	if listed[1].RPID != "was.example.com" {
		t.Fatalf("the list must carry the RP ID it was registered under: %+v", listed[1])
	}
}

func TestEnsureUserWebAuthnHandleKeepsTheFirstOneWritten(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	first, err := s.EnsureUserWebAuthnHandle(owner.ID, []byte("handle-one"))
	if err != nil {
		t.Fatalf("EnsureUserWebAuthnHandle: %v", err)
	}
	if string(first) != "handle-one" {
		t.Fatalf("the first ceremony's handle must be stored, got %q", first)
	}
	second, err := s.EnsureUserWebAuthnHandle(owner.ID, []byte("handle-two"))
	if err != nil {
		t.Fatalf("second EnsureUserWebAuthnHandle: %v", err)
	}
	if string(second) != "handle-one" {
		t.Fatalf("a later ceremony must read the stored handle, got %q", second)
	}

	resolved, err := s.UserByWebAuthnHandle([]byte("handle-one"))
	if err != nil {
		t.Fatalf("UserByWebAuthnHandle: %v", err)
	}
	if resolved.ID != owner.ID {
		t.Fatalf("the handle must resolve its account, got %s", resolved.ID)
	}
	if _, err := s.UserByWebAuthnHandle([]byte("handle-two")); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("an unknown handle must resolve nothing, got %v", err)
	}
}

func TestUsersWithoutAHandleAreNotResolvedByAnEmptyOne(t *testing.T) {
	s, _, _ := seedOwnerDevice(t)
	// Every account starts with a NULL handle. An empty lookup must not
	// match them all, which is what a bare `= ?` would do for NULL-vs-empty
	// confusion in a future rewrite.
	if _, err := s.UserByWebAuthnHandle(nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("an empty handle must resolve nothing, got %v", err)
	}
	if _, err := s.UserByWebAuthnHandle([]byte{}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("an empty handle must resolve nothing, got %v", err)
	}
}

func TestPasskeyTransportsRefuseACorruptBlob(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	seedPasskey(t, s, owner.ID, "pk-1", "Phone", []byte("cred-a"))
	if _, err := s.db.Exec(`UPDATE passkeys SET transports = ? WHERE id = ?`, "{not json", "pk-1"); err != nil {
		t.Fatalf("corrupt the blob: %v", err)
	}
	_, err := s.PasskeyByCredentialID([]byte("cred-a"))
	if err == nil || !strings.Contains(err.Error(), "decode passkey transports") {
		t.Fatalf("a corrupt transports blob must be reported, got %v", err)
	}
}
