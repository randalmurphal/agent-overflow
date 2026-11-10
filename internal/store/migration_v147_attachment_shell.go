package store

// Migration v147 lets a `file` attachment record that its bytes are the
// output of a shell command the person ran from the composer: the command,
// its exit code and duration, as JSON (AttachmentShell). NULL for every
// other attachment, so no existing row changes meaning.
//
// A plain ADD COLUMN with no CHECK, so the FK-parent `attachments` table is
// not rebuilt; validateAttachment is the rule every writer applies.
const attachmentShellMigrationVersion = 147

const attachmentShellV147SQL = `ALTER TABLE attachments ADD COLUMN shell TEXT;`
