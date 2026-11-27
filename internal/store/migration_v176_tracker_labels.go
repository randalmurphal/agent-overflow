package store

// Migration v176 adds tracker_cache_issues.labels: the names of a card's
// labels, a JSON array in the card's order (MirrorCard.Labels), which the
// card writer stores with the card and a support rule's label arm reads
// (mirrorSupportSQL) without decoding the card's data.
//
// The backfill takes the names from each card whose data is the version
// the app writes (2), a label named by its name, else its ID, as the
// app's own decode reads them; a card of any other version holds none,
// which is what the app reads of it (its data is a miss until the card is
// read again). No release has shipped the tracker mirror.
const trackerLabelsV176SQL = `
ALTER TABLE tracker_cache_issues ADD COLUMN labels TEXT NOT NULL DEFAULT '[]';

UPDATE tracker_cache_issues SET labels = (
  SELECT json_group_array(coalesce(nullif(l.value ->> 'name', ''), l.value ->> 'id'))
    FROM json_each(CAST(data AS TEXT), '$.labels') l
   WHERE coalesce(nullif(l.value ->> 'name', ''), nullif(l.value ->> 'id', '')) IS NOT NULL)
 WHERE json_valid(CAST(data AS TEXT)) AND CAST(data AS TEXT) ->> '$.v' = 2
   AND json_type(CAST(data AS TEXT), '$.labels') = 'array';
`

const trackerLabelsMigrationVersion = 176
