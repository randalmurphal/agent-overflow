package store

// Migration v164 marks a pairing link minted by a computer pairing window
// (internal/pairbootstrap). Such a link's comparison number is the digits
// that window's key exchange derived, which only the window holds; the
// owner is shown those digits or none, never the number an ordinary
// redemption derives from the link and its key. `bootstrap` is 1 for those
// links and 0 for every other.
//
// Every existing row reads 0. A row an older build's window minted is either
// confirmed or canceled by now: startup retires every unconfirmed link
// before it serves (store.RetirePendingPairings), so no such row can still
// wait for a number.
//
// Startup's sweep of unactivated sessions (pendingLinkSessionsSQL) reads
// only those sessions, through idx_sessions_pending, and finds each one's
// link through idx_pairing_links_session, so it reads neither every live
// session nor the link history.
const pairingBootstrapMigrationVersion = 164

const pairingBootstrapV164SQL = `
ALTER TABLE pairing_links ADD COLUMN bootstrap INTEGER NOT NULL DEFAULT 0 CHECK(bootstrap IN (0,1));
CREATE INDEX idx_pairing_links_session ON pairing_links(session_id);
CREATE INDEX idx_sessions_pending ON sessions(activated_at) WHERE activated_at IS NULL AND revoked_at IS NULL;
`
