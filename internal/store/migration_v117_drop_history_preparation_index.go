package store

// Background history sealing is removed, so nothing reads the partial index
// v108 built for it. Dropping it removes a write on every settled item.
const dropHistoryPreparationIndexV117SQL = `
DROP INDEX idx_items_history_preparation;
`
