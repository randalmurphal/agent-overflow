package store

// Migration v153 records how a late reply reached the caller apart from how
// the first answer did. Both shared delivered_how, so a late reply delivered
// inline after a queued wake hid that wake: the Agents row lost its link to
// the row that carried the answer.
//
// A row whose late reply was delivered before it gets that route copied; its
// first answer's route was overwritten then and stays as it reads.
const lateDeliveryRouteMigrationVersion = 153

const lateDeliveryRouteV153SQL = `
ALTER TABLE thread_requests ADD COLUMN late_delivered_how TEXT NOT NULL DEFAULT ''
    CHECK(late_delivered_how IN ('','inline','queued','draft'));
UPDATE thread_requests SET late_delivered_how = delivered_how WHERE late_delivered_at IS NOT NULL;
`
