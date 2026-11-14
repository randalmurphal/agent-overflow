package store

// Migration v149 records which message carried a request's answer when that
// message was not the request's own.
//
// One wake can answer several requests: a turn that consumed three sends
// ends once, and a restart interrupts every open request a thread made. The
// message is queued under one request's send id, the carrier's, and every
// other request it answers stores that request's token in `delivered_with`.
// Empty means the request's own message carried its answer, or none did.
// Without it a request delivered by another request's message could not say
// whether that message has reached the model yet, and a boot that restored
// the message into the composer could correct only the carrier's route.
//
// The partial index serves the lookup from a carrier to the requests that
// rode with it, which runs once per restored wake at boot.
const threadWakeCarrierMigrationVersion = 149

const threadWakeCarrierV149SQL = `
ALTER TABLE thread_requests ADD COLUMN delivered_with TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_thread_requests_delivered_with
    ON thread_requests(delivered_with)
 WHERE delivered_with <> '';
`
