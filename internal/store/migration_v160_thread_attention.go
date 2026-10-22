package store

// Migration v160 records the attention an agent flags on its thread
// (docs/specs/standing-agents.md, "Attention"): its kind, the short reason
// it gave and when it flagged it. The flag shows until the thread is read
// past attention_at, as an unread completion does; with no flag the kind is
// empty and attention_at NULL.
const threadAttentionMigrationVersion = 160

const threadAttentionV160SQL = `
ALTER TABLE threads ADD COLUMN attention_kind TEXT NOT NULL DEFAULT ''
    CHECK(attention_kind IN ('', 'decision', 'review', 'problem'));
ALTER TABLE threads ADD COLUMN attention_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN attention_at INTEGER;
`
