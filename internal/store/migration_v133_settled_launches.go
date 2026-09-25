package store

// Migration v133 drops the triggers that made a settled background launch
// live again when its ending sibling left the thread: its delete
// (trg_items_revive_bg_launch_on_completion_delete, v126) and its move to
// a holder (trg_items_revive_bg_launch_on_completion_move, v127). A launch
// settles once (background_settle_triggers.go). No row needs a fix: a
// launch a revive left live is live to the boot recovery sweep, which
// settles it as it settles every launch the previous process owned.
const settledLaunchesV133SQL = `DROP TRIGGER IF EXISTS trg_items_revive_bg_launch_on_completion_delete;
DROP TRIGGER IF EXISTS trg_items_revive_bg_launch_on_completion_move;`
