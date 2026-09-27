package store

// Migration v134 indexes a row's provider item id. A settled assistant
// snapshot that never streamed, which is every text and thinking block of
// a background agent, is matched to its row by that id
// (FindStreamItemByProviderItemID). Without the index the lookup read the
// whole turn, decoding each row's meta, once per block.
const providerItemIndexMigrationVersion = 134

const providerItemIndexV134SQL = `CREATE INDEX idx_items_provider_item
    ON items(thread_id, json_extract(meta, '$.provider_item_id'), turn_index)
 WHERE json_extract(meta, '$.provider_item_id') IS NOT NULL`
