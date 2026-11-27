package store

// Migration v175 adds tracker_cache_issues.labels: the names of a card's
// labels, a JSON array in the card's order (MirrorCard.Labels), which the
// card writer stores with the card and a support rule's label arm reads
// (mirrorSupportSQL) without decoding the card's data.
//
// The backfill takes the names from each card whose data is the version
// the app writes (2), a label named by its name, else its ID, as the
// app's own decode reads them; a card of any other version holds none,
// which is what the app reads of it (its data is a miss until the card is
// read again). No release has shipped the tracker mirror.
//
// A card's type and labels are inputs of the section totals under a
// support rule, so trg_tracker_cache_totals_support advances the totals
// revision of every board with a row of an issue whose type or labels
// changed, as the status trigger does for its status: support moves no
// row between sections, so the revision stays.
const trackerLabelsV175SQL = `
ALTER TABLE tracker_cache_issues ADD COLUMN labels TEXT NOT NULL DEFAULT '[]';

UPDATE tracker_cache_issues SET labels = (
  SELECT json_group_array(coalesce(nullif(l.value ->> 'name', ''), l.value ->> 'id'))
    FROM json_each(CAST(data AS TEXT), '$.labels') l
   WHERE coalesce(nullif(l.value ->> 'name', ''), nullif(l.value ->> 'id', '')) IS NOT NULL)
 WHERE json_valid(CAST(data AS TEXT)) AND CAST(data AS TEXT) ->> '$.v' = 2
   AND json_type(CAST(data AS TEXT), '$.labels') = 'array';

CREATE TRIGGER trg_tracker_cache_totals_support AFTER UPDATE OF type_id, labels ON tracker_cache_issues
 WHEN OLD.type_id IS NOT NEW.type_id OR OLD.labels IS NOT NEW.labels BEGIN
 UPDATE tracker_cache_boards SET totals_revision = totals_revision + 1 WHERE root_id = NEW.root_id
  AND board_id IN (SELECT board_id FROM tracker_cache_board_issues WHERE root_id = NEW.root_id AND issue_id = NEW.issue_id);
END;
`

const trackerLabelsMigrationVersion = 175
