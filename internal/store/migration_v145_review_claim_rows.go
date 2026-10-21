package store

// Migration v145 records, on a review comment a message claimed, the user
// row that carries it (claim_thread_id, claim_item_id) until that row lands.
// A claim commits before its row: a queued message's row lands only when
// the provider echoes it. The row's insert clears the record in its own
// transaction (the trigger), so a record left at boot names a message whose
// row never landed, and ReleaseUnsettledDiffReviewClaims returns its
// comments to drafts.
const reviewClaimRowsMigrationVersion = 145

const reviewClaimRowsV145SQL = `ALTER TABLE diff_review_comments ADD COLUMN claim_thread_id TEXT NOT NULL DEFAULT '';
ALTER TABLE diff_review_comments ADD COLUMN claim_item_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_diff_review_comments_claim ON diff_review_comments(claim_thread_id, claim_item_id) WHERE claim_item_id <> '';
CREATE TRIGGER items_settle_diff_review_claims
AFTER INSERT ON items
WHEN NEW.kind = 'user_text'
BEGIN
  UPDATE diff_review_comments
     SET claim_thread_id = '', claim_item_id = ''
   WHERE claim_item_id <> '' AND claim_thread_id = NEW.thread_id AND claim_item_id = NEW.id;
END;`
