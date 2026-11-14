package store

// Migration v145 records, on a review comment a message claimed, the user
// row that carries it (claim_thread_id, claim_item_id) and what settles the
// claim (claim_settles). A claim commits before its message reaches the
// provider. A direct send or steer is delivered by its row, so the row's
// insert settles its claim in the same transaction (the trigger, 'row'). A
// queued message's row can land before the provider has it, so its claim
// settles when the provider confirms consumption ('confirm',
// settleDiffReviewClaimsTx). The link stays after settlement: a message the
// provider retires undelivered returns its comments to drafts by it
// (RetireUndeliveredUserRow). A claim still unsettled at boot names a
// message no provider confirmed, and ReleaseUnsettledDiffReviewClaims
// returns its comments to drafts.
const reviewClaimRowsMigrationVersion = 145

const reviewClaimRowsV145SQL = `ALTER TABLE diff_review_comments ADD COLUMN claim_thread_id TEXT NOT NULL DEFAULT '';
ALTER TABLE diff_review_comments ADD COLUMN claim_item_id TEXT NOT NULL DEFAULT '';
ALTER TABLE diff_review_comments ADD COLUMN claim_settles TEXT NOT NULL DEFAULT '' CHECK (claim_settles IN ('', 'row', 'confirm'));
CREATE INDEX idx_diff_review_comments_claim ON diff_review_comments(claim_thread_id, claim_item_id) WHERE claim_item_id <> '';
CREATE TRIGGER items_settle_diff_review_claims
AFTER INSERT ON items
WHEN NEW.kind = 'user_text'
BEGIN
  UPDATE diff_review_comments
     SET claim_settles = ''
   WHERE claim_item_id <> '' AND claim_thread_id = NEW.thread_id AND claim_item_id = NEW.id AND claim_settles = 'row';
END;`
