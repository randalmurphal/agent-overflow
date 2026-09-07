package store

const ownDevicesV90SQL = `
ALTER TABLE pairing_links ADD COLUMN purpose TEXT NOT NULL DEFAULT '' CHECK(purpose IN ('','own-device','own-introduction'));
ALTER TABLE pairing_links ADD COLUMN expected_key TEXT NOT NULL DEFAULT '';
ALTER TABLE pairing_links ADD COLUMN member_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE pairing_links ADD COLUMN sponsor_key TEXT NOT NULL DEFAULT '';
CREATE TABLE own_devices (
 key_thumbprint TEXT PRIMARY KEY,
 member TEXT NOT NULL CHECK(json_valid(member)),
 generation INTEGER NOT NULL CHECK(generation>0),
 removed INTEGER NOT NULL CHECK(removed IN (0,1))
);
CREATE TABLE own_device_sessions (
 session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
 key_thumbprint TEXT NOT NULL REFERENCES own_devices(key_thumbprint),
 generation INTEGER NOT NULL
);
CREATE INDEX idx_own_device_sessions_key ON own_device_sessions(key_thumbprint);
CREATE INDEX idx_pairing_introduction_key ON pairing_links(expected_key) WHERE purpose='own-introduction';
`
