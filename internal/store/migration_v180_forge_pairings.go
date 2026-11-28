package store

// Migration v180 adds tracker_forge_pairings: who a person said a forge
// account is in a connection's tracker, one row per connection, forge
// host and login. person is the tracker person, or empty for an account
// that is no person's (a bot), whose acts credit then leaves out;
// person_name is the person's name as picked, which names them where the
// mirror has not read them. Credit reads a pairing before the name match
// (tracker_cache_forge_people), and the match is not made for a paired
// account.
//
// The rows are the person's settings, an application record like the
// boards a project lists: kept through a restore with the snapshot's
// connections, and removed with their connection.
const forgePairingsV180SQL = `
CREATE TABLE tracker_forge_pairings (
 connection_id TEXT NOT NULL REFERENCES tracker_connections(id) ON DELETE CASCADE,
 host TEXT NOT NULL CHECK(host <> ''),
 login TEXT NOT NULL CHECK(login <> ''),
 person TEXT NOT NULL DEFAULT '',
 person_name TEXT NOT NULL DEFAULT '' CHECK(person <> '' OR person_name = ''),
 PRIMARY KEY (connection_id, host, login)
) WITHOUT ROWID;
`

const forgePairingsMigrationVersion = 180
