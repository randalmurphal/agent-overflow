package store

// The v100 row-stamping triggers compared a resume carrier's
// transcript_root_id with `ancestors.id`, whose TEXT affinity kept
// idx_items_transcript_root from serving the value: every item written
// under a parent read and parsed the meta of every carrier in its thread,
// once per carrier leg. This migration reinstalls the three triggers with
// the affinity-free comparison (stampedRowIDsFor). The rows they stamp are
// unchanged.
var revTriggerCarrierProbeV118SQL = dropHistoryRevTriggersSQL + `
` + historyRevTriggersSQL
