package store

import "testing"

func TestLocalSessionMigrationPreservesPairingAndRevocation(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		name := "live newest session"
		if revoked {
			name = "revoked newest session"
		}
		t.Run(name, func(t *testing.T) {
			testLocalSessionMigration(t, revoked)
		})
	}
}

func testLocalSessionMigration(t *testing.T, newestRevoked bool) {
	t.Helper()
	db := migrateThrough(t, 109)
	mustExec(t, db, `INSERT INTO users(id,display_name,role,created_at) VALUES('u','Owner','owner',1)`)
	mustExec(t, db, `INSERT INTO devices(id,user_id,label,class,created_at,channel) VALUES('local','u','Local','desktop',1,'local'),('remote','u','Remote','browser',1,'')`)
	mustExec(t, db, `INSERT INTO signing_keys VALUES('key',X'1234',1)`)
	mustExec(t, db, `INSERT INTO sessions(id,user_id,device_id,binding_class,scopes,signing_key_id,created_at,expires_at,revoked_at,activated_at) VALUES
 ('local-old','u','local','loopback-only','[]','key',1,100,50,1),
 ('local-middle','u','local','loopback-only','[]','key',1,150,NULL,1),
 ('local-current','u','local','loopback-only','[]','key',2,200,NULL,2),
 ('remote','u','remote','device-bound','[]','key',1,300,NULL,1)`)
	mustExec(t, db, `INSERT INTO refresh_secrets(id,session_id,secret_hash,created_at,expires_at) VALUES('refresh','remote',X'01',1,1000)`)
	if newestRevoked {
		mustExec(t, db, `UPDATE sessions SET revoked_at=75 WHERE id='local-current'`)
	}
	migrateFrom(t, db, 109)
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sessions WHERE binding_class='loopback-only' AND expires_at IS NULL`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("local lifetimes=%d: %v", count, err)
	}
	wantLive := 1
	if newestRevoked {
		wantLive = 0
	}
	if err := db.QueryRow(`SELECT count(*) FROM sessions WHERE device_id='local' AND revoked_at IS NULL`).Scan(&count); err != nil || count != wantLive {
		t.Fatalf("migration revived old local rows: %d %v", count, err)
	}
	if newestRevoked {
		var currentRevocation int
		if err := db.QueryRow(`SELECT revoked_at FROM sessions WHERE id='local-current'`).Scan(&currentRevocation); err != nil || currentRevocation != 75 {
			t.Fatalf("newest session revocation=%d: %v", currentRevocation, err)
		}
	}
	var revoked int
	if err := db.QueryRow(`SELECT revoked_at FROM sessions WHERE id='local-old'`).Scan(&revoked); err != nil || revoked != 50 {
		t.Fatalf("revocation=%d: %v", revoked, err)
	}
	var expiry int
	if err := db.QueryRow(`SELECT expires_at FROM sessions WHERE id='remote'`).Scan(&expiry); err != nil || expiry != 300 {
		t.Fatalf("remote expiry=%d: %v", expiry, err)
	}
	if _, err := db.Exec(`UPDATE sessions SET expires_at=NULL WHERE id='remote'`); err == nil {
		t.Fatal("timed session accepted absent expiry")
	}
	if _, err := db.Exec(`UPDATE sessions SET expires_at=400 WHERE id='local-current'`); err == nil {
		t.Fatal("local session accepted timed expiry")
	}
	if err := db.QueryRow(`SELECT count(*) FROM refresh_secrets`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("refresh lost during rebuild: %d %v", count, err)
	}
	mustExec(t, db, `DELETE FROM sessions WHERE id='remote'`)
	if err := db.QueryRow(`SELECT count(*) FROM refresh_secrets`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cascade lost during rebuild: %d %v", count, err)
	}
}
