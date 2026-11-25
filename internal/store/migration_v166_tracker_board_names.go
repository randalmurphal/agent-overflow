package store

// Migration v166 gives each board a project lists the name and tracker
// project the person chose it by: project_tracker_boards.name and
// tracker_project (a Jira project's key, a Linear team's key), each at
// most maxTrackerBoardTextRunes characters, written by
// ReplaceProjectTrackerBoards from what the board picker showed. A board's
// name is otherwise only in its mirror, after the board's first structure
// read, so a listing no one has opened yet could not be named.
// ListTrackerBoardChoices answers the mirror's name once the board was
// read and this one until then. A listing stored before v166 holds empty
// text, which the next read of the board names.
//
// It also gives each connection the digest of the token its verification
// checked, tracker_connections.verified_token (empty or a 32-byte
// SHA-256), written by RecordTrackerConnectionVerification with the
// identity. A connection's identity counts as the token in its key file
// only while that file holds the verified token. A verification recorded
// before v166 names no token, so v166 clears it: the connection reads as
// not checked until its next check.
const trackerBoardNamesMigrationVersion = 166

const trackerBoardNamesV166SQL = `
ALTER TABLE project_tracker_boards ADD COLUMN name TEXT NOT NULL DEFAULT '' CHECK(length(name) <= 255);
ALTER TABLE project_tracker_boards ADD COLUMN tracker_project TEXT NOT NULL DEFAULT '' CHECK(length(tracker_project) <= 255);
ALTER TABLE tracker_connections ADD COLUMN verified_token BLOB NOT NULL DEFAULT x'' CHECK(length(verified_token) IN (0, 32));
UPDATE tracker_connections SET verified_account_id = '', verified_name = '', verified_at = 0;
`
