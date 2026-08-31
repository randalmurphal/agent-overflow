package store

import (
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
)

// seedOwnerDevice returns a store with an owner account and one device, the
// shape almost every session test starts from.
func seedOwnerDevice(t *testing.T) (*Store, User, Device) {
	t.Helper()
	s := newTestStore(t)
	owner, err := s.EnsureOwnerUser("Owner")
	if err != nil {
		t.Fatalf("EnsureOwnerUser: %v", err)
	}
	device, err := s.CreateDevice(owner.ID, "This Desktop", "desktop", "linux")
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	return s, owner, device
}

func seedSigningKey(t *testing.T, s *Store) SigningKey {
	t.Helper()
	key := SigningKey{ID: "key-1", Secret: []byte("0123456789abcdef"), CreatedAt: 100}
	if err := s.InsertSigningKey(key); err != nil {
		t.Fatalf("InsertSigningKey: %v", err)
	}
	return key
}

func TestEnsureOwnerUserIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	first, err := s.EnsureOwnerUser("Owner")
	if err != nil {
		t.Fatalf("first EnsureOwnerUser: %v", err)
	}
	second, err := s.EnsureOwnerUser("A Different Name")
	if err != nil {
		t.Fatalf("second EnsureOwnerUser: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("owner id moved: %q -> %q", first.ID, second.ID)
	}
	if second.DisplayName != "Owner" {
		t.Fatalf("second call rewrote the display name: %q", second.DisplayName)
	}
	if first.Role != UserRoleOwner {
		t.Fatalf("owner role = %q, want %q", first.Role, UserRoleOwner)
	}
}

// TestEnsureOwnerUserUnderConcurrency — first boot can race itself (two
// goroutines reaching the bootstrap at once). The partial unique index
// decides, and every loser must return the winner's row rather than an
// error, or one of them would pair a device to an account that does not
// exist.
func TestEnsureOwnerUserUnderConcurrency(t *testing.T) {
	s := newTestStore(t)
	const callers = 8
	ids := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			user, err := s.EnsureOwnerUser("Owner")
			ids[i], errs[i] = user.ID, err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Fatalf("caller %d saw owner %q, caller 0 saw %q", i, ids[i], ids[0])
		}
	}
	users, err := s.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("ListUsers = %d rows, want exactly one owner", len(users))
	}
}

// TestSchemaRefusesASecondOwner pins the partial unique index. A second
// owner is unrepresentable, not merely discouraged.
func TestSchemaRefusesASecondOwner(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.EnsureOwnerUser("Owner"); err != nil {
		t.Fatalf("EnsureOwnerUser: %v", err)
	}
	_, err := s.db.Exec(
		`INSERT INTO users (id, display_name, role, created_at, disabled_at)
		 VALUES ('second', 'Second Owner', 'owner', 1, NULL)`)
	if err == nil {
		t.Fatal("schema accepted a second owner row")
	}
}

// TestUsersAreGenuinelyPlural — members coexist with the owner and every
// read takes an explicit id. Nothing collapses to "the user".
func TestUsersAreGenuinelyPlural(t *testing.T) {
	s := newTestStore(t)
	owner, err := s.EnsureOwnerUser("Owner")
	if err != nil {
		t.Fatalf("EnsureOwnerUser: %v", err)
	}
	member, err := s.CreateUser("Teammate")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if member.Role != UserRoleMember {
		t.Fatalf("member role = %q", member.Role)
	}
	users, err := s.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("ListUsers = %d rows, want 2", len(users))
	}
	for _, want := range []User{owner, member} {
		got, err := s.GetUser(want.ID)
		if err != nil {
			t.Fatalf("GetUser(%s): %v", want.ID, err)
		}
		if got.DisplayName != want.DisplayName {
			t.Fatalf("GetUser(%s) = %q, want %q", want.ID, got.DisplayName, want.DisplayName)
		}
	}
}

func TestGetUserMissingIsErrNoRows(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.GetUser("nobody"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetUser(missing) = %v, want sql.ErrNoRows", err)
	}
}

func TestDeviceClassCheckAcceptsTheDeclaredSetAndNothingElse(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	for _, class := range []string{"desktop", "browser", "phone", "cli", "backend-peer"} {
		if _, err := s.CreateDevice(owner.ID, "Device", class, "linux"); err != nil {
			t.Fatalf("CreateDevice(class=%q): %v", class, err)
		}
	}
	if _, err := s.CreateDevice(owner.ID, "Device", "toaster", "linux"); err == nil {
		t.Fatal("schema accepted an undeclared device class")
	}
}

func TestDeviceProofSlotsStartEmptyAndAreUniqueWhenSet(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	if device.KeyThumbprint != "" || device.PasskeyCredentialID != "" {
		t.Fatalf("new device already carries proof material: %+v", device)
	}
	if err := s.SetDeviceKeyThumbprint(device.ID, "thumb-a"); err != nil {
		t.Fatalf("SetDeviceKeyThumbprint: %v", err)
	}
	if err := s.SetDevicePasskeyCredential(device.ID, "cred-a"); err != nil {
		t.Fatalf("SetDevicePasskeyCredential: %v", err)
	}
	other, err := s.CreateDevice(owner.ID, "Other", "browser", "linux")
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	if err := s.SetDeviceKeyThumbprint(other.ID, "thumb-a"); err == nil {
		t.Fatal("one key thumbprint named two devices")
	}
	if err := s.SetDevicePasskeyCredential(other.ID, "cred-a"); err == nil {
		t.Fatal("one passkey credential named two devices")
	}
	// Two devices with NO proof material must still coexist: the unique
	// indexes are partial, so NULL is not a value that collides.
	if _, err := s.CreateDevice(owner.ID, "Third", "cli", "linux"); err != nil {
		t.Fatalf("third device with empty proof slots refused: %v", err)
	}
	read, err := s.GetDevice(device.ID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if read.KeyThumbprint != "thumb-a" || read.PasskeyCredentialID != "cred-a" {
		t.Fatalf("proof slots did not round-trip: %+v", read)
	}
}

func TestTouchDeviceReportsWhetherItMoved(t *testing.T) {
	s, _, device := seedOwnerDevice(t)
	moved, err := s.TouchDevice(device.ID, 500)
	if err != nil || !moved {
		t.Fatalf("first touch: moved=%v err=%v", moved, err)
	}
	moved, err = s.TouchDevice(device.ID, 500)
	if err != nil || moved {
		t.Fatalf("repeat touch at the same stamp: moved=%v err=%v", moved, err)
	}
	read, err := s.GetDevice(device.ID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if read.LastSeenAt != 500 {
		t.Fatalf("last_seen_at = %d, want 500", read.LastSeenAt)
	}
}

func newTestSession(id string, owner User, device Device, key SigningKey, expires int64) Session {
	return Session{
		ID: id, UserID: owner.ID, DeviceID: device.ID,
		BindingClass: "device-bound", Scopes: []string{"threads:read"},
		SigningKeyID: key.ID, CreatedAt: 1000, ExpiresAt: expires,
	}
}

func TestCreateSessionRoundTrips(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	want := newTestSession("sess-1", owner, device, key, 9000)
	want.Scopes = []string{"threads:read", "files:read"}
	if err := s.CreateSession(want); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := s.GetSession("sess-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.UserID != owner.ID || got.DeviceID != device.ID || got.BindingClass != "device-bound" {
		t.Fatalf("session did not round-trip: %+v", got)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "threads:read" || got.Scopes[1] != "files:read" {
		t.Fatalf("scopes = %v", got.Scopes)
	}
	if got.RevokedAt != 0 || got.LastSeenAt != 0 {
		t.Fatalf("new session already carries stamps: %+v", got)
	}
	if !got.Live(8999) {
		t.Fatal("fresh session reported not live")
	}
	if got.Live(9000) {
		t.Fatal("session reported live at its own expiry")
	}
}

func TestCreateSessionRefusesUnusableRows(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	base := newTestSession("sess-1", owner, device, key, 9000)

	cases := map[string]func(Session) Session{
		"empty id":             func(v Session) Session { v.ID = ""; return v },
		"empty user":           func(v Session) Session { v.UserID = ""; return v },
		"empty device":         func(v Session) Session { v.DeviceID = ""; return v },
		"empty binding class":  func(v Session) Session { v.BindingClass = ""; return v },
		"empty signing key":    func(v Session) Session { v.SigningKeyID = ""; return v },
		"expiry before minted": func(v Session) Session { v.ExpiresAt = v.CreatedAt - 1; return v },
		"expiry at mint":       func(v Session) Session { v.ExpiresAt = v.CreatedAt; return v },
	}
	for name, mutate := range cases {
		if err := s.CreateSession(mutate(base)); err == nil {
			t.Fatalf("%s: CreateSession accepted an unusable row", name)
		}
	}
	if err := s.CreateSession(func(v Session) Session { v.BindingClass = "sort-of-bound"; return v }(base)); err == nil {
		t.Fatal("schema accepted an undeclared binding class")
	}
}

func TestSessionScopesEncodeEmptyAsAnArray(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	session := newTestSession("sess-empty", owner, device, key, 9000)
	session.Scopes = nil
	if err := s.CreateSession(session); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	var raw string
	if err := s.db.QueryRow(`SELECT scopes FROM sessions WHERE id = 'sess-empty'`).Scan(&raw); err != nil {
		t.Fatalf("read scopes: %v", err)
	}
	if raw != "[]" {
		t.Fatalf("empty scope set stored as %q, want []", raw)
	}
	got, err := s.GetSession("sess-empty")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(got.Scopes) != 0 {
		t.Fatalf("scopes = %v, want empty", got.Scopes)
	}
}

// TestSessionScopesReadIsStrict — a scope blob that does not decode is a
// storage fault, and reporting it as "no scopes" would turn that fault
// into a permissions answer no caller could distinguish from a real one.
func TestSessionScopesReadIsStrict(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	if err := s.CreateSession(newTestSession("sess-1", owner, device, key, 9000)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET scopes = '{"not":"an array"}' WHERE id = 'sess-1'`); err != nil {
		t.Fatalf("corrupt scopes: %v", err)
	}
	if _, err := s.GetSession("sess-1"); err == nil {
		t.Fatal("GetSession read a non-array scope blob as a valid grant")
	}
	if _, err := s.db.Exec(`UPDATE sessions SET scopes = 'not json' WHERE id = 'sess-1'`); err == nil {
		t.Fatal("schema accepted a scopes value that is not JSON")
	}
}

func TestRevokeSessionReportsWhetherItMovedAndKeepsTheFirstStamp(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	if err := s.CreateSession(newTestSession("sess-1", owner, device, key, 9000)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	moved, err := s.RevokeSession("sess-1", 2000)
	if err != nil || !moved {
		t.Fatalf("first revoke: moved=%v err=%v", moved, err)
	}
	moved, err = s.RevokeSession("sess-1", 3000)
	if err != nil || moved {
		t.Fatalf("second revoke: moved=%v err=%v", moved, err)
	}
	got, err := s.GetSession("sess-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.RevokedAt != 2000 {
		t.Fatalf("revoked_at = %d, want the first stamp 2000", got.RevokedAt)
	}
	if got.Live(1) {
		t.Fatal("revoked session still reports live")
	}
	moved, err = s.RevokeSession("no-such-session", 4000)
	if err != nil || moved {
		t.Fatalf("revoke of an unknown session: moved=%v err=%v", moved, err)
	}
}

func TestTouchSessionSkipsRevokedRows(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	if err := s.CreateSession(newTestSession("sess-1", owner, device, key, 9000)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if moved, err := s.TouchSession("sess-1", 1500); err != nil || !moved {
		t.Fatalf("touch live session: moved=%v err=%v", moved, err)
	}
	if _, err := s.RevokeSession("sess-1", 2000); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if moved, err := s.TouchSession("sess-1", 2500); err != nil || moved {
		t.Fatalf("touch revoked session: moved=%v err=%v", moved, err)
	}
	got, err := s.GetSession("sess-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.LastSeenAt != 1500 {
		t.Fatalf("last_seen_at = %d, want 1500 (the last live use)", got.LastSeenAt)
	}
}

// TestRevokeDeviceClosesItsSessionsInOneWrite — the device flag and the
// session flags are one intent. A caller receives the ids it has to
// force-close, and a device flagged revoked can never be left holding a
// live session.
func TestRevokeDeviceClosesItsSessionsInOneWrite(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	for _, id := range []string{"sess-a", "sess-b"} {
		if err := s.CreateSession(newTestSession(id, owner, device, key, 9000)); err != nil {
			t.Fatalf("CreateSession(%s): %v", id, err)
		}
	}
	// A session on a second device must be untouched.
	otherDevice, err := s.CreateDevice(owner.ID, "Phone", "phone", "ios")
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	if err := s.CreateSession(newTestSession("sess-other", owner, otherDevice, key, 9000)); err != nil {
		t.Fatalf("CreateSession(other): %v", err)
	}
	// One of this device's sessions is already revoked; it must not be
	// reported again, because it was closed by whoever revoked it.
	if _, err := s.RevokeSession("sess-b", 1500); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}

	ids, err := s.RevokeDevice(device.ID, 2000)
	if err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if len(ids) != 1 || ids[0] != "sess-a" {
		t.Fatalf("RevokeDevice returned %v, want [sess-a]", ids)
	}
	read, err := s.GetDevice(device.ID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if read.RevokedAt != 2000 {
		t.Fatalf("device revoked_at = %d, want 2000", read.RevokedAt)
	}
	live, err := s.ListLiveSessions(1)
	if err != nil {
		t.Fatalf("ListLiveSessions: %v", err)
	}
	if len(live) != 1 || live[0].ID != "sess-other" {
		t.Fatalf("live sessions after device revoke = %v, want only sess-other", live)
	}
	// A second revocation is a no-op and reports nothing to close.
	again, err := s.RevokeDevice(device.ID, 3000)
	if err != nil {
		t.Fatalf("second RevokeDevice: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second RevokeDevice reported %v, want nothing", again)
	}
}

func TestListLiveSessionsExcludesExpiredAndRevoked(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	if err := s.CreateSession(newTestSession("live", owner, device, key, 9000)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.CreateSession(newTestSession("expired", owner, device, key, 1500)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.CreateSession(newTestSession("revoked", owner, device, key, 9000)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.RevokeSession("revoked", 1600); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	live, err := s.ListLiveSessions(2000)
	if err != nil {
		t.Fatalf("ListLiveSessions: %v", err)
	}
	if len(live) != 1 || live[0].ID != "live" {
		t.Fatalf("ListLiveSessions = %v, want only the live row", live)
	}
	all, err := s.ListSessionsForDevice(device.ID)
	if err != nil {
		t.Fatalf("ListSessionsForDevice: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListSessionsForDevice = %d rows, want all 3", len(all))
	}
}

func TestSigningKeysAndTheirCascade(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	if _, err := s.ActiveSigningKey(); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ActiveSigningKey on a fresh store = %v, want sql.ErrNoRows", err)
	}
	older := SigningKey{ID: "key-old", Secret: []byte("old"), CreatedAt: 100}
	newer := SigningKey{ID: "key-new", Secret: []byte("new"), CreatedAt: 200}
	for _, key := range []SigningKey{older, newer} {
		if err := s.InsertSigningKey(key); err != nil {
			t.Fatalf("InsertSigningKey(%s): %v", key.ID, err)
		}
	}
	active, err := s.ActiveSigningKey()
	if err != nil {
		t.Fatalf("ActiveSigningKey: %v", err)
	}
	if active.ID != "key-new" {
		t.Fatalf("ActiveSigningKey = %q, want the newest", active.ID)
	}
	byID, err := s.SigningKeyByID("key-old")
	if err != nil {
		t.Fatalf("SigningKeyByID: %v", err)
	}
	if string(byID.Secret) != "old" {
		t.Fatalf("secret did not round-trip: %q", byID.Secret)
	}
	if _, err := s.SigningKeyByID("key-missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SigningKeyByID(missing) = %v, want sql.ErrNoRows", err)
	}

	// Dropping a key retires everything it minted.
	if err := s.CreateSession(newTestSession("sess-old", owner, device, older, 9000)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM signing_keys WHERE id = 'key-old'`); err != nil {
		t.Fatalf("delete signing key: %v", err)
	}
	if _, err := s.GetSession("sess-old"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("session outlived its signing key: %v", err)
	}
}

func TestInsertSigningKeyIsIdempotentOnID(t *testing.T) {
	s := newTestStore(t)
	key := SigningKey{ID: "key-1", Secret: []byte("first"), CreatedAt: 1}
	if err := s.InsertSigningKey(key); err != nil {
		t.Fatalf("InsertSigningKey: %v", err)
	}
	key.Secret = []byte("second")
	if err := s.InsertSigningKey(key); err != nil {
		t.Fatalf("repeat InsertSigningKey: %v", err)
	}
	got, err := s.SigningKeyByID("key-1")
	if err != nil {
		t.Fatalf("SigningKeyByID: %v", err)
	}
	if string(got.Secret) != "first" {
		t.Fatalf("secret was overwritten: %q — a key's bytes must never move", got.Secret)
	}
}

func TestRecoveryCodesAreSingleUse(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	hashes := [][]byte{[]byte("hash-a"), []byte("hash-b"), []byte("hash-c")}
	if err := s.ReplaceRecoveryCodes(owner.ID, hashes, 1000); err != nil {
		t.Fatalf("ReplaceRecoveryCodes: %v", err)
	}
	if count, err := s.CountUnspentRecoveryCodes(owner.ID); err != nil || count != 3 {
		t.Fatalf("CountUnspentRecoveryCodes = %d (err %v), want 3", count, err)
	}
	userID, err := s.ConsumeRecoveryCode([]byte("hash-b"), 1100, device.ID)
	if err != nil {
		t.Fatalf("ConsumeRecoveryCode: %v", err)
	}
	if userID != owner.ID {
		t.Fatalf("code admitted %q, want the owner %q", userID, owner.ID)
	}
	// The replay: same code, second time.
	if _, err := s.ConsumeRecoveryCode([]byte("hash-b"), 1200, device.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("replayed code = %v, want sql.ErrNoRows", err)
	}
	// A code that never existed answers identically.
	if _, err := s.ConsumeRecoveryCode([]byte("hash-never"), 1200, device.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown code = %v, want sql.ErrNoRows", err)
	}
	if count, err := s.CountUnspentRecoveryCodes(owner.ID); err != nil || count != 2 {
		t.Fatalf("CountUnspentRecoveryCodes = %d (err %v), want 2", count, err)
	}
	var consumedBy string
	if err := s.db.QueryRow(
		`SELECT consumed_by FROM recovery_codes WHERE code_hash = ?`, []byte("hash-b"),
	).Scan(&consumedBy); err != nil {
		t.Fatalf("read consumed_by: %v", err)
	}
	if consumedBy != device.ID {
		t.Fatalf("consumed_by = %q, want the device that spent it", consumedBy)
	}
}

// TestRecoveryCodeConsumptionIsAtomic — several callers presenting the same
// code concurrently must produce exactly one winner. The single UPDATE with
// the `consumed_at IS NULL` predicate is the whole mechanism.
func TestRecoveryCodeConsumptionIsAtomic(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	if err := s.ReplaceRecoveryCodes(owner.ID, [][]byte{[]byte("hash-a")}, 1000); err != nil {
		t.Fatalf("ReplaceRecoveryCodes: %v", err)
	}
	const callers = 8
	results := make([]error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			_, results[i] = s.ConsumeRecoveryCode([]byte("hash-a"), 1100, device.ID)
		}()
	}
	wg.Wait()
	winners := 0
	for i, err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, sql.ErrNoRows):
		default:
			t.Fatalf("caller %d: unexpected error %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d callers consumed the same code, want exactly 1", winners)
	}
}

func TestReplaceRecoveryCodesDropsUnspentAndKeepsSpent(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	if err := s.ReplaceRecoveryCodes(owner.ID, [][]byte{[]byte("old-1"), []byte("old-2")}, 1000); err != nil {
		t.Fatalf("ReplaceRecoveryCodes: %v", err)
	}
	if _, err := s.ConsumeRecoveryCode([]byte("old-1"), 1100, device.ID); err != nil {
		t.Fatalf("ConsumeRecoveryCode: %v", err)
	}
	if err := s.ReplaceRecoveryCodes(owner.ID, [][]byte{[]byte("new-1")}, 1200); err != nil {
		t.Fatalf("second ReplaceRecoveryCodes: %v", err)
	}
	if _, err := s.ConsumeRecoveryCode([]byte("old-2"), 1300, device.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a superseded code still worked: %v", err)
	}
	if _, err := s.ConsumeRecoveryCode([]byte("new-1"), 1300, device.ID); err != nil {
		t.Fatalf("fresh code refused: %v", err)
	}
	// The spent row survives, so a replay of it is still visibly a replay.
	var spent int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM recovery_codes WHERE code_hash = ?`, []byte("old-1"),
	).Scan(&spent); err != nil {
		t.Fatalf("count spent: %v", err)
	}
	if spent != 1 {
		t.Fatalf("spent recovery code row was dropped by a re-mint")
	}
}

func TestReplaceRecoveryCodesRefusesAnEmptySet(t *testing.T) {
	s, owner, _ := seedOwnerDevice(t)
	if err := s.ReplaceRecoveryCodes(owner.ID, nil, 1000); !errors.Is(err, ErrIdentityFieldRequired) {
		t.Fatalf("empty set = %v, want ErrIdentityFieldRequired", err)
	}
	if err := s.ReplaceRecoveryCodes(owner.ID, [][]byte{nil}, 1000); !errors.Is(err, ErrIdentityFieldRequired) {
		t.Fatalf("empty hash = %v, want ErrIdentityFieldRequired", err)
	}
}

func TestAuthAuditAppendsAndReads(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	entries := []AuthAuditEntry{
		{At: 100, Event: "session-minted", Outcome: AuthAuditOutcomeAllowed,
			UserID: owner.ID, DeviceID: device.ID, SessionID: "sess-1", Peer: "127.0.0.1"},
		{At: 200, Event: "verification-refused", Outcome: AuthAuditOutcomeRefused,
			Reason: "revoked_session", DeviceID: device.ID, Peer: "192.168.1.9"},
	}
	for _, entry := range entries {
		if _, err := s.AppendAuthAudit(entry); err != nil {
			t.Fatalf("AppendAuthAudit: %v", err)
		}
	}
	got, err := s.ListRecentAuthAudit(10)
	if err != nil {
		t.Fatalf("ListRecentAuthAudit: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListRecentAuthAudit = %d rows, want 2", len(got))
	}
	if got[0].At != 200 || got[0].Reason != "revoked_session" {
		t.Fatalf("newest entry first not honoured: %+v", got[0])
	}
	perDevice, err := s.ListAuthAuditForDevice(device.ID, 10)
	if err != nil {
		t.Fatalf("ListAuthAuditForDevice: %v", err)
	}
	if len(perDevice) != 2 {
		t.Fatalf("ListAuthAuditForDevice = %d rows, want 2", len(perDevice))
	}
	if rows, err := s.ListAuthAuditForDevice("", 10); err != nil || rows != nil {
		t.Fatalf("empty device id = %v (err %v), want no rows", rows, err)
	}
}

// TestAuthAuditOutlivesWhatItDescribes — attribution columns are not
// foreign keys on purpose: the record that a device was revoked is worth
// most once that device row is gone.
func TestAuthAuditOutlivesWhatItDescribes(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	if _, err := s.AppendAuthAudit(AuthAuditEntry{
		At: 100, Event: "device-revoked", Outcome: AuthAuditOutcomeAllowed,
		UserID: owner.ID, DeviceID: device.ID,
	}); err != nil {
		t.Fatalf("AppendAuthAudit: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM devices WHERE id = ?`, device.ID); err != nil {
		t.Fatalf("delete device: %v", err)
	}
	got, err := s.ListAuthAuditForDevice(device.ID, 10)
	if err != nil {
		t.Fatalf("ListAuthAuditForDevice: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("audit rows vanished with the device they describe: %v", got)
	}
}

func TestAuthAuditRowsAreImmutable(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AppendAuthAudit(AuthAuditEntry{
		At: 100, Event: "session-minted", Outcome: AuthAuditOutcomeAllowed,
	}); err != nil {
		t.Fatalf("AppendAuthAudit: %v", err)
	}
	_, err := s.db.Exec(`UPDATE auth_audit SET outcome = 'refused' WHERE id = 1`)
	if err == nil {
		t.Fatal("an auth_audit row was rewritten")
	}
	if !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("update refused for the wrong reason: %v", err)
	}
}

func TestAuthAuditRefusesUnreadableRows(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AppendAuthAudit(AuthAuditEntry{
		At: 1, Event: "", Outcome: AuthAuditOutcomeAllowed,
	}); !errors.Is(err, ErrIdentityFieldRequired) {
		t.Fatalf("empty event = %v, want ErrIdentityFieldRequired", err)
	}
	if _, err := s.AppendAuthAudit(AuthAuditEntry{
		At: 1, Event: "session-minted", Outcome: "maybe",
	}); err == nil {
		t.Fatal("an outcome outside allowed/refused was accepted")
	}
}

// TestAuthAuditPrunesToItsBound — a peer that keeps presenting a dead
// credential must not grow the log without limit. The prune runs on the
// append, keyed on insert order rather than on any clock.
func TestAuthAuditPrunesToItsBound(t *testing.T) {
	s := newTestStore(t)
	total := maxAuthAuditRows + 2*authAuditPruneEvery
	for i := range total {
		if _, err := s.AppendAuthAudit(AuthAuditEntry{
			At: int64(i + 1), Event: "verification-refused", Outcome: AuthAuditOutcomeRefused,
		}); err != nil {
			t.Fatalf("AppendAuthAudit #%d: %v", i, err)
		}
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM auth_audit`).Scan(&count); err != nil {
		t.Fatalf("count auth_audit: %v", err)
	}
	if count > maxAuthAuditRows+authAuditPruneEvery {
		t.Fatalf("auth_audit holds %d rows, want at most %d",
			count, maxAuthAuditRows+authAuditPruneEvery)
	}
	if count < maxAuthAuditRows {
		t.Fatalf("auth_audit pruned below its bound: %d rows", count)
	}
	// The survivors are the newest ones.
	var oldest int64
	if err := s.db.QueryRow(`SELECT MIN(at) FROM auth_audit`).Scan(&oldest); err != nil {
		t.Fatalf("read oldest: %v", err)
	}
	if oldest == 1 {
		t.Fatal("the prune kept the oldest rows")
	}
}

func TestIdentityWritesRefuseMissingRequiredFields(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.EnsureOwnerUser("  "); !errors.Is(err, ErrIdentityFieldRequired) {
		t.Fatalf("blank owner name = %v", err)
	}
	if _, err := s.CreateUser(""); !errors.Is(err, ErrIdentityFieldRequired) {
		t.Fatalf("blank user name = %v", err)
	}
	if _, err := s.CreateDevice("", "Label", "desktop", ""); !errors.Is(err, ErrIdentityFieldRequired) {
		t.Fatalf("blank device user = %v", err)
	}
	if _, err := s.CreateDevice("user", "", "desktop", ""); !errors.Is(err, ErrIdentityFieldRequired) {
		t.Fatalf("blank device label = %v", err)
	}
	if err := s.InsertSigningKey(SigningKey{ID: "", Secret: []byte("x")}); !errors.Is(err, ErrIdentityFieldRequired) {
		t.Fatalf("blank key id = %v", err)
	}
	if err := s.InsertSigningKey(SigningKey{ID: "k"}); !errors.Is(err, ErrIdentityFieldRequired) {
		t.Fatalf("empty key secret = %v", err)
	}
	if _, err := s.ConsumeRecoveryCode(nil, 1, ""); !errors.Is(err, ErrIdentityFieldRequired) {
		t.Fatalf("empty code hash = %v", err)
	}
}

func TestDeviceAndSessionCascadeWithTheirUser(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	key := seedSigningKey(t, s)
	if err := s.CreateSession(newTestSession("sess-1", owner, device, key, 9000)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, owner.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := s.GetDevice(device.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("device outlived its user: %v", err)
	}
	if _, err := s.GetSession("sess-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("session outlived its user: %v", err)
	}
}
