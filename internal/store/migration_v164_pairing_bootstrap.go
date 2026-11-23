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
const pairingBootstrapMigrationVersion = 164

const pairingBootstrapV164SQL = `
ALTER TABLE pairing_links ADD COLUMN bootstrap INTEGER NOT NULL DEFAULT 0 CHECK(bootstrap IN (0,1));
`
