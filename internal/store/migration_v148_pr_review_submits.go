package store

// Migration v148 records the outcome of each PR review submit by the
// submit id its client minted for the draft set, so a submit retried
// after its answer was lost answers the recorded outcome instead of
// posting the review again (RecordPRReviewSubmit). The owner columns
// name the drafts' owner, a thread or a PR, as diff_review_comments does.
const prReviewSubmitsMigrationVersion = 148

const prReviewSubmitsV148SQL = `CREATE TABLE pr_review_submits (
 submit_id TEXT PRIMARY KEY CHECK(submit_id <> ''),
 pr_key TEXT NOT NULL CHECK(pr_key <> ''),
 owner_thread_id TEXT NOT NULL DEFAULT '',
 owner_pr_key TEXT NOT NULL DEFAULT '',
 result TEXT NOT NULL CHECK(json_valid(result)),
 created_at INTEGER NOT NULL,
 CHECK((owner_thread_id = '') <> (owner_pr_key = ''))
);
CREATE INDEX idx_pr_review_submits_created ON pr_review_submits(created_at);`
