package identity

import (
	"database/sql"
	"slices"
	"testing"
)

// passkeyWriteFixture is a passkey fixture with a second handle on the
// database file, for injecting a write failure with a trigger.
func passkeyWriteFixture(t *testing.T) (*Sessions, *sql.DB, *softAuthenticator, string) {
	t.Helper()
	sessions, _, raw, owner, _ := stragglerFixture(t)
	sessions.SetRelyingParty(testRelyingParty)
	auth := newSoftAuthenticator(t)
	enroll(t, sessions, owner.ID, auth)
	return sessions, raw, auth, owner.ID
}

// signInAs is signIn with the label and platform the browser reports.
func signInAs(t *testing.T, s *Sessions, auth *softAuthenticator, thumbprint, label, platform string) (PasskeySignIn, Reason) {
	t.Helper()
	challenge, reason := s.BeginPasskeySignIn()
	if reason.Refused() {
		t.Fatalf("BeginPasskeySignIn: %s", reason)
	}
	return s.FinishPasskeySignIn(PasskeySignInRequest{
		CeremonyID: challenge.CeremonyID,
		Response:   auth.assert(t, challenge, testOrigin),
		Proof:      bearerProof(thumbprint),
		Label:      label,
		Platform:   platform,
		Peer:       "192.0.2.10:44321",
	})
}

// signInWrites is what a sign-in can leave in the database.
type signInWrites struct {
	devices, sessions, refreshSecrets, mintAudits int
	labels                                        []string
}

func countSignInWrites(t *testing.T, raw *sql.DB) signInWrites {
	t.Helper()
	var w signInWrites
	for _, count := range []struct {
		query string
		into  *int
	}{
		{`SELECT COUNT(*) FROM devices`, &w.devices},
		{`SELECT COUNT(*) FROM sessions`, &w.sessions},
		{`SELECT COUNT(*) FROM refresh_secrets`, &w.refreshSecrets},
		{`SELECT COUNT(*) FROM auth_audit WHERE event IN ('session-minted', 'passkey-signed-in')`, &w.mintAudits},
	} {
		if err := raw.QueryRow(count.query).Scan(count.into); err != nil {
			t.Fatalf("%s: %v", count.query, err)
		}
	}
	rows, err := raw.Query(`SELECT label || '/' || platform FROM devices ORDER BY id`)
	if err != nil {
		t.Fatalf("read device labels: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			t.Fatalf("scan device label: %v", err)
		}
		w.labels = append(w.labels, label)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read device labels: %v", err)
	}
	return w
}

// A sign-in that fails at any of its writes leaves nothing: no new device,
// no renamed device, no session (live or revoked), no refresh secret, and
// no audit entry saying a session was minted.
func TestAPasskeySignInThatFailsAtAnyWriteLeavesNothing(t *testing.T) {
	writes := []struct {
		name, newDevice, existingDevice string
	}{
		{"device", `BEFORE INSERT ON devices`, `BEFORE UPDATE OF label ON devices`},
		{"session", `BEFORE INSERT ON sessions`, `BEFORE INSERT ON sessions`},
		{"refresh secret", `BEFORE INSERT ON refresh_secrets`, `BEFORE INSERT ON refresh_secrets`},
	}
	for _, write := range writes {
		for _, existing := range []bool{false, true} {
			name := write.name + " on a new device"
			if existing {
				name = write.name + " on a known device"
			}
			t.Run(name, func(t *testing.T) {
				sessions, raw, auth, _ := passkeyWriteFixture(t)
				trigger := write.newDevice
				if existing {
					if _, reason := signInAs(t, sessions, auth, "thumb-laptop", "Laptop browser", "linux"); reason.Refused() {
						t.Fatalf("first sign-in: %s", reason)
					}
					trigger = write.existingDevice
				}
				before := countSignInWrites(t, raw)
				if _, err := raw.Exec(`CREATE TRIGGER fail_sign_in ` + trigger +
					` BEGIN SELECT RAISE(ABORT, 'injected sign-in write failure'); END`); err != nil {
					t.Fatalf("install the failure: %v", err)
				}

				_, reason := signInAs(t, sessions, auth, "thumb-laptop", "Renamed browser", "windows")
				if reason != ReasonUnknownCredential {
					t.Fatalf("a sign-in whose %s write failed = %s, want %s", write.name, reason, ReasonUnknownCredential)
				}
				after := countSignInWrites(t, raw)
				if after.devices != before.devices || !slices.Equal(after.labels, before.labels) {
					t.Errorf("a failed sign-in changed the devices: %v, want %v", after.labels, before.labels)
				}
				if after.sessions != before.sessions {
					t.Errorf("a failed sign-in left %d session rows, want %d", after.sessions, before.sessions)
				}
				if after.refreshSecrets != before.refreshSecrets {
					t.Errorf("a failed sign-in left %d refresh secrets, want %d", after.refreshSecrets, before.refreshSecrets)
				}
				if after.mintAudits != before.mintAudits {
					t.Errorf("a failed sign-in recorded %d mint audit entries, want %d", after.mintAudits, before.mintAudits)
				}
			})
		}
	}
}

// A sign-in that succeeds writes the device's new name with a live,
// renewable session, and records the mint and the sign-in.
func TestAPasskeySignInWritesItsDeviceSessionAndAudit(t *testing.T) {
	sessions, raw, auth, ownerID := passkeyWriteFixture(t)
	first, reason := signInAs(t, sessions, auth, "thumb-laptop", "Laptop browser", "linux")
	if reason.Refused() {
		t.Fatalf("first sign-in: %s", reason)
	}
	second, reason := signInAs(t, sessions, auth, "thumb-laptop", "Renamed browser", "windows")
	if reason.Refused() {
		t.Fatalf("second sign-in: %s", reason)
	}
	if second.DeviceID != first.DeviceID {
		t.Fatalf("the known browser got a second device: %s then %s", first.DeviceID, second.DeviceID)
	}
	device, err := sessions.store.GetDevice(second.DeviceID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if device.Label != "Renamed browser" || device.Platform != "windows" || device.UserID != ownerID || device.Class != string(DeviceBrowser) {
		t.Fatalf("the signed-in device = %+v, want the reported name and platform on the owner's browser row", device)
	}
	session, reason := sessions.Live(second.Tokens.SessionID)
	if reason.Refused() {
		t.Fatalf("the session must admit calls immediately, got %s", reason)
	}
	if want := RenewablePolicyFor(DeviceBrowser).Access.Milliseconds(); session.ExpiresAt-session.CreatedAt != want {
		t.Fatalf("session window = %dms, want the browser access window %dms", session.ExpiresAt-session.CreatedAt, want)
	}
	if second.Tokens.RefreshSecret == "" {
		t.Fatal("the sign-in must issue a renewable pair")
	}
	var refresh int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM refresh_secrets WHERE session_id = ?`, session.ID).Scan(&refresh); err != nil || refresh != 1 {
		t.Fatalf("the session's refresh secrets = %d (%v), want 1", refresh, err)
	}
	entries, err := sessions.store.ListAuthAuditForDevice(device.ID, 10)
	if err != nil {
		t.Fatalf("ListAuthAuditForDevice: %v", err)
	}
	var events []string
	for _, entry := range entries {
		if entry.SessionID == session.ID {
			events = append(events, entry.Event)
		}
	}
	if want := []string{string(AuditPasskeySignedIn), string(AuditSessionMinted)}; !slices.Equal(events, want) {
		t.Fatalf("the sign-in's audit entries, newest first = %v, want %v", events, want)
	}
}
