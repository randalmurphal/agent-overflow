package store

// The attachment's original thread names its storage path. Logical owners
// keep that path alive independently, including after the original is deleted.
const attachmentOwnersV109SQL = `
CREATE TABLE attachments_new (
 id TEXT PRIMARY KEY, thread_id TEXT NOT NULL, filename TEXT NOT NULL,
 mime_type TEXT NOT NULL, size INTEGER NOT NULL, relative_path TEXT NOT NULL,
 created_at INTEGER NOT NULL, thumbnail_data BLOB, thumbnail_mime TEXT,
 kind TEXT NOT NULL DEFAULT 'image'
);
INSERT INTO attachments_new SELECT id,thread_id,filename,mime_type,size,relative_path,created_at,thumbnail_data,thumbnail_mime,kind FROM attachments;
DROP TABLE attachments;
ALTER TABLE attachments_new RENAME TO attachments;
CREATE INDEX idx_attachments_thread ON attachments(thread_id);
CREATE TABLE attachment_owners (
 thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
 attachment_id TEXT NOT NULL REFERENCES attachments(id) ON DELETE CASCADE,
 PRIMARY KEY(thread_id,attachment_id)
);
CREATE INDEX idx_attachment_owners_attachment ON attachment_owners(attachment_id,thread_id);
INSERT INTO attachment_owners SELECT thread_id,id FROM attachments;
INSERT OR IGNORE INTO attachment_owners
 SELECT i.thread_id,a.id FROM timeline_items i,
 json_each(CASE WHEN json_valid(i.meta) THEN i.meta ELSE '{}' END,'$.attachments') ref
 JOIN attachments a ON a.id=CASE WHEN ref.type='object' THEN json_extract(ref.value,'$.id') WHEN ref.type='text' THEN ref.value END;
` + attachmentOwnerTriggersSQL

const attachmentOwnerTriggersSQL = `
CREATE TRIGGER trg_attachments_initial_owner AFTER INSERT ON attachments BEGIN
 INSERT INTO attachment_owners(thread_id,attachment_id) VALUES(NEW.thread_id,NEW.id);
END;
CREATE TRIGGER trg_attachment_owners_gc AFTER DELETE ON attachment_owners BEGIN
 DELETE FROM attachments WHERE id=OLD.attachment_id AND NOT EXISTS (
 SELECT 1 FROM attachment_owners WHERE attachment_id=OLD.attachment_id);
END;
`

const dropAttachmentOwnerTriggersSQL = `DROP TRIGGER IF EXISTS trg_attachments_initial_owner;
DROP TRIGGER IF EXISTS trg_attachment_owners_gc;`
