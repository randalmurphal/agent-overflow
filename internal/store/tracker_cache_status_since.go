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
// left and came back from since. Evidence of a change after the version
// is of a later version, and gives no result. The store writes a result
// only to a record of the version and status it was read for
// (mirrorStatusWriteSQL).
func MirrorStatusSince(e MirrorStatusEvidence) (since int64, final bool) {
	r, since := mirrorStatusSince(e)
	return since, r == mirrorStatusFinal
}

func mirrorStatusSince(e MirrorStatusEvidence) (mirrorStatusResult, int64) {
	if e.LatestTo != "" && e.LatestAt > e.UpdatedAt {
		return 0, 0
	}
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
// issue row being updated: the values of status_since, status_final_for
// and status_read_at for a result of kind (a mirrorStatusResult, zero for
// none) giving since, read for version (an update time) of a card in
// status by the read that began at readAt (an order time), each an SQL
// expression. A result applies only while the row's card is of that
// version and that status, checked by the statement that writes it, so
// evidence read for an update the card has moved past, or of a card
// another read of the same update replaced, writes nothing. Two read
// orders the row holds order the evidence:
//   - status_floor_at, the read of the card write that gave the row its
//     version or status (mirrorStatusKeptBy): evidence from a read that
//     began before it, final or provisional, may be of a stay the issue
//     left and came back to within one update time, and does not enter;
//   - status_read_at, the read behind the provisional result the row
//     holds, at or after the floor: a provisional result enters only from
//     a read no older, and moves it, so evidence from an older read never
//     replaces a newer read's, whichever writer brought either and whether
//     or not a body is held. A newer read of the same value moves the
//     order alone, which no writer reports as a change.
//
// A final result decides the version's time against the floor alone: it
// replaces a provisional one from a newer read, and once stored no
// provisional one applies for the version. A history read that leaves
// its issue due nothing more (history_for at the version, no wait)
// brings a final result, so ordering finals by the floor alone leaves no
// issue without its final time. The history writers are serialized per
// connection by the service (its history lock); a ticket read is not, so
// the order is kept here, in the row, for every writer.
//
// A card write keeps the time while the status stays and is no evidence
// (mirrorStatusKeptBy): the row's status_final_for then names an earlier
// version, so the issue is due history and its tickets lack a final time.
func mirrorStatusWriteSQL(kind, since, version, status, readAt string) (sinceTo, finalForTo, readTo string) {
	i := `tracker_cache_issues.`
	applies := i + `updated_at = ` + version + ` AND ` + i + `status_id IS ` + status
	final := `(` + applies + ` AND ` + kind + ` = 2 AND ` + readAt + ` >= ` + i + `status_floor_at)`
	provisional := `(` + applies + ` AND ` + kind + ` = 1 AND ` + i + `status_final_for < ` + version + ` AND ` + readAt + ` >= ` + i + `status_read_at)`
	sinceTo = `iif(` + final + ` OR ` + provisional + `, ` + since + `, ` + i + `status_since)`
	finalForTo = `iif(` + final + `, ` + version + `, ` + i + `status_final_for)`
	readTo = `iif(` + provisional + `, ` + readAt + `, ` + i + `status_read_at)`
	return sinceTo, finalForTo, readTo
}

// mirrorStatusKeptBy is what a card write of update time updated in
// status, read from readAt, leaves of the status time: the stored time
// when the card has the stored card's status, and zero when it has
// another, the stored time being that of a status the issue left. The
// time stays final only for the update it was decided for
// (status_final_for), so a card of a later update shows it until a read
// for that update decides; a card of a later update is due a history read
// by its update time. A card of the same update in another status (a
// later read of a change within the tracker's millisecond) clears a time
// a read decided for that update, so the write makes the issue due a
// history read again (history_for, the version arm of mirrorHistoryDue)
// and moves its receipt, which supersedes a history read listed before
// it: that history was read for the status the issue left. A write that
// moves the version or the status sets both read orders of the status
// evidence (mirrorStatusWriteSQL) to its own read: evidence from a read
// before it does not enter, and the issue it leaves without a final time
// for its version is due a history read, which begins after it.
func mirrorStatusKeptBy(status, updated, readAt string) string {
	i := `tracker_cache_issues.`
	moved := `(` + updated + ` = ` + i + `updated_at AND ` + status + ` IS NOT ` + i + `status_id)`
	stay := `(` + updated + ` IS NOT ` + i + `updated_at OR ` + status + ` IS NOT ` + i + `status_id)`
	return `status_since = iif(` + status + ` IS ` + i + `status_id, ` + i + `status_since, 0),
	    status_final_for = iif(` + status + ` IS ` + i + `status_id, ` + i + `status_final_for, 0),
	    status_floor_at = iif(` + stay + `, ` + readAt + `, ` + i + `status_floor_at),
	    status_read_at = iif(` + stay + `, ` + readAt + `, ` + i + `status_read_at),
	    history_for = iif(` + moved + `, 0, ` + i + `history_for),
	    history_receipt = ` + i + `history_receipt + ` + moved
}
