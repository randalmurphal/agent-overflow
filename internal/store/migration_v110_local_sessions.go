package store

// Local channel credentials are process-bound; their durable attribution has no deadline.
const localSessionsV110SQL = `
CREATE TABLE sessions_new (
 id TEXT PRIMARY KEY,
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
 binding_class TEXT NOT NULL CHECK(binding_class IN ('loopback-only','device-bound','public')),
 scopes TEXT NOT NULL CHECK(json_valid(scopes)),
 signing_key_id TEXT NOT NULL REFERENCES signing_keys(id) ON DELETE CASCADE,
 created_at INTEGER NOT NULL,
 expires_at INTEGER NULL,
 revoked_at INTEGER NULL,
 last_seen_at INTEGER NOT NULL DEFAULT 0,
 activated_at INTEGER NULL,
 CHECK ((binding_class = 'loopback-only' AND expires_at IS NULL)
     OR (binding_class <> 'loopback-only' AND expires_at IS NOT NULL))
);
INSERT INTO sessions_new
 SELECT id,user_id,device_id,binding_class,scopes,signing_key_id,created_at,
 CASE WHEN binding_class='loopback-only' THEN NULL ELSE expires_at END,
 CASE WHEN binding_class='loopback-only' AND revoked_at IS NULL AND EXISTS (
 SELECT 1 FROM sessions newer WHERE newer.device_id=sessions.device_id
 AND newer.binding_class='loopback-only'
 AND (newer.created_at>sessions.created_at OR (newer.created_at=sessions.created_at AND newer.id>sessions.id))
 ) THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE revoked_at END,
 last_seen_at,activated_at FROM sessions;
DROP TABLE sessions;
ALTER TABLE sessions_new RENAME TO sessions;
CREATE INDEX idx_sessions_device ON sessions(device_id, created_at);
CREATE INDEX idx_sessions_user ON sessions(user_id, created_at);
CREATE UNIQUE INDEX idx_sessions_local ON sessions(device_id) WHERE binding_class='loopback-only' AND revoked_at IS NULL;
CREATE INDEX idx_sessions_live ON sessions(expires_at) WHERE revoked_at IS NULL;
`
