package store

// Migration v165 adds tracker_cache_issues.sprint_set, the digest of the
// sprints an issue's card names (mirrorSprintSet), part of the card's
// content, so a card that names the same sprints costs one comparison and
// no membership read or write. The digest is computed in Go, so an issue
// that holds memberships when the column lands is given
// mirrorSprintSetPartial, which no set digests to: the next write that
// admits its card replaces its sprints. An issue with none holds the empty
// digest, which is right for it.
const trackerSprintSetsMigrationVersion = 165

const trackerSprintSetsV165SQL = `
ALTER TABLE tracker_cache_issues ADD COLUMN sprint_set BLOB NOT NULL DEFAULT x'';
UPDATE tracker_cache_issues SET sprint_set = x'` + mirrorSprintSetPartial + `'
 WHERE EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m
                WHERE m.root_id = tracker_cache_issues.root_id AND m.issue_id = tracker_cache_issues.issue_id);
`
