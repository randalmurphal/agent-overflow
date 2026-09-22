package store

const transcriptRootIndexV112SQL = `
CREATE INDEX IF NOT EXISTS idx_items_transcript_root
    ON items(thread_id, json_extract(meta, '$.transcript_root_id'))
 WHERE json_extract(meta, '$.transcript_root_id') IS NOT NULL;
`
