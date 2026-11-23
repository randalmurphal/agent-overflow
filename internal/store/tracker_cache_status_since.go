package store

// MirrorStatusEvidence is what one read tells of when an issue's status
// began, for the version of the issue it read: the card's status, creation
// and update times (UpdatedAt, the version), the latest status change the
// read's history holds (LatestTo empty for none, LatestAt when it was),
// whether that history is complete from the issue's creation (Complete),
// whether it is final through UpdatedAt (Final: no change at or before
// the version can be missing from it), and the tracker's settle margin in
// milliseconds (Settle), how long after a change its history may lack it.
// All times are the tracker's clock. A ticket's read and a history read
// each give it; a card read gives none.
type MirrorStatusEvidence struct {
	Status               string
	CreatedAt, UpdatedAt int64
	LatestTo             string
	LatestAt             int64
	Complete, Final      bool
	Settle               int64
}

// mirrorStatusResult is a status time and how sure it is: a final result
// decides the version's time; a provisional one shows until a final one
// is stored for the version, and never replaces one.
type mirrorStatusResult int

const (
	mirrorStatusProvisional mirrorStatusResult = 1
	mirrorStatusFinal       mirrorStatusResult = 2
)

// MirrorStatusSince is the one rule for when an issue's status began, from
// the evidence of one read; zero is unknown. From a final read:
//   - the latest status change went to the card's status: its time;
//   - it went to another status (the card and the history disagree):
//     unknown;
//   - there is none and the history is complete from creation: the
//     creation;
//   - otherwise unknown (Linear's history folds its opening interval, so
//     it is never complete from creation).
//
// A read that is not final may lack the newest change, so its result is
// provisional: the latest change's time when it went to the card's status
// no more than the settle margin before the version's update time, which
// bounds the error by the margin (the true time is no later than the
// update), and otherwise unknown: an older change may be a stay the issue
// left and came back from since. The store writes a result only for the
// version it was read for (mirrorStatusWriteSQL).
func MirrorStatusSince(e MirrorStatusEvidence) (since int64, final bool) {
	r, since := mirrorStatusSince(e)
	return since, r == mirrorStatusFinal
}

func mirrorStatusSince(e MirrorStatusEvidence) (mirrorStatusResult, int64) {
	if !e.Final {
		if e.LatestTo != "" && e.LatestTo == e.Status && e.LatestAt >= e.UpdatedAt-e.Settle {
			return mirrorStatusProvisional, e.LatestAt
		}
		return mirrorStatusProvisional, 0
	}
	switch {
	case e.LatestTo != "" && e.LatestTo == e.Status:
		return mirrorStatusFinal, e.LatestAt
	case e.LatestTo != "":
		return mirrorStatusFinal, 0
	case e.Complete:
		return mirrorStatusFinal, e.CreatedAt
	}
	return mirrorStatusFinal, 0
}

// mirrorStatusWriteSQL is the one way a status result is written to the
// issue row being updated: the values of status_since and
// status_final_for for a result of kind (a mirrorStatusResult, zero for
// none) giving since, read for version (an update time), each an SQL
// expression. A result applies only while the row's card is of that
// version, so evidence read for an update the card has moved past writes
// nothing; a final one decides the version's time, and a provisional one
// shows only while no final one is stored for the version. A card write
// keeps the time while the status stays and is no evidence
// (mirrorStatusKeptBy): the row's status_final_for then names an earlier
// version, so the issue is due history and its tickets lack a final time.
func mirrorStatusWriteSQL(kind, since, version string) (sinceTo, finalForTo string) {
	applies := `tracker_cache_issues.updated_at = ` + version
	sinceTo = `CASE WHEN ` + applies + ` AND (` + kind + ` = 2 OR ` + kind + ` = 1 AND tracker_cache_issues.status_final_for < ` + version + `)
	    THEN ` + since + ` ELSE tracker_cache_issues.status_since END`
	finalForTo = `iif(` + applies + ` AND ` + kind + ` = 2, ` + version + `, tracker_cache_issues.status_final_for)`
	return sinceTo, finalForTo
}

// mirrorStatusKeptBy is what a card write leaves of the status time: the
// stored time when the card it brings has the stored card's status, and
// zero when it has another, the stored time being that of a status the
// issue left. The time stays final only for the update it was decided for
// (status_final_for), so a card of a later update shows it until a read
// for that update decides.
func mirrorStatusKeptBy(status string) string {
	return `status_since = iif(` + status + ` IS tracker_cache_issues.status_id, tracker_cache_issues.status_since, 0),
	    status_final_for = iif(` + status + ` IS tracker_cache_issues.status_id, tracker_cache_issues.status_final_for, 0)`
}
