package store

import "testing"

// v164 marks the links a computer pairing window mints. Every link the store
// already holds was minted by an ordinary surface or has been settled, so it
// reads 0, and the mark admits only 0 and 1.
func TestPairingBootstrapMigrationMarksNoExistingLink(t *testing.T) {
	db := migrateThrough(t, pairingBootstrapMigrationVersion-1)
	mustExec(t, db, `INSERT INTO users(id,display_name,role,created_at) VALUES('u','Owner','owner',1)`)
	mustExec(t, db, `INSERT INTO pairing_links (id, user_id, token_hash, scopes, binding_class, device_class, created_at, expires_at, redeemed_at, confirmed_at)
		VALUES ('pl-open', 'u', randomblob(32), '[]', 'device-bound', 'desktop', 1, 2, NULL, NULL),
		       ('pl-confirmed', 'u', randomblob(32), '[]', 'device-bound', 'desktop', 1, 2, 2, 3)`)

	migrateFrom(t, db, pairingBootstrapMigrationVersion-1)

	assertRows(t, db, `SELECT id || ':' || bootstrap FROM pairing_links ORDER BY id`, "pl-confirmed:0,pl-open:0")
	mustExec(t, db, `UPDATE pairing_links SET bootstrap = 1 WHERE id = 'pl-open'`)
	if _, err := db.Exec(`UPDATE pairing_links SET bootstrap = 2 WHERE id = 'pl-open'`); err == nil {
		t.Fatal("bootstrap = 2 was accepted, want the CHECK to refuse it")
	}
}
