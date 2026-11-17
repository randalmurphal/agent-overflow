package store

// Migration v153 refills the thread-find candidate text of every user row
// that carries a composer shell command's output.
//
// The row's command block shows the command its meta holds, which the
// typed text may no longer hold (a message taken back to the composer and
// edited). Candidate text written before v153 read only the text, and one
// whose text joined a `!command` between other parts matched every query;
// find_candidate now holds each meta command and narrows the joined text.
// The rows are those idx_items_attachment_refs and
// idx_import_history_items_attachment_refs list, so the fix reads the rows
// with attachments, not the table. find_text is no read's column and no
// guard's: the write moves no revision and every copy a fork shows or a
// holder keeps is a row of these tables.
const findTextShellCommandsMigrationVersion = 153

const findTextShellCommandsV153SQL = `
UPDATE items SET find_text = find_candidate(kind, summary, meta, tool_name, NULL)
 WHERE instr(meta, '"attachments"') > 0 AND kind = 'user_text' AND instr(meta, '"shell"') > 0;
UPDATE import_history_items SET find_text = find_candidate(kind, summary, meta, tool_name, NULL)
 WHERE instr(meta, '"attachments"') > 0 AND kind = 'user_text' AND instr(meta, '"shell"') > 0;
`
