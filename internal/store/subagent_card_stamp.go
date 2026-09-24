package store

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sync/atomic"
)

// cardState is what a note may do to a stamp.
type cardState uint8

const (
	// cardLive is a clean stamp: notes keep its values exact.
	cardLive cardState = iota
	// cardInert is a readTime stamp: reads walk it, and a note only
	// marks it reached.
	cardInert
	// cardRecompute waits for the next flush to recompute it.
	cardRecompute
)

// cardStamp is one stamp's accumulator, shared by every card of the
// thread that reaches it.
type cardStamp struct {
	id    string
	state cardState
	// exists reports a stamp row; gen is its generation.
	exists bool
	gen    int64
	// stored is the row as last read or written, values the accumulator.
	stored, values subagentStampValues
	// carrier reports a carrier: its own children count toward no card of
	// it, only toward its tray.
	carrier bool
	// reached marks a stamp a note reached since the last flush.
	reached bool
	// openedBy is the root whose resume prompt opened this carrier's card
	// in memory (notePrompt), until its first stamp is written.
	openedBy string
	// rounds reports a live root with resume rounds. A row at or after lo
	// lands in its last round, whose carrier stamp is round (nil when the
	// round's prompt names none). roundID names that carrier until
	// linkRounds finds its accumulator.
	rounds  bool
	lo      TimelineCursor
	round   *cardStamp
	roundID string
}

func (st *cardStamp) pending() bool {
	return st.state == cardRecompute || st.reached || (st.state == cardLive && st.values != st.stored)
}

// takes reports whether a note that reaches the stamp changes its values.
func (st *cardStamp) takes() bool {
	switch st.state {
	case cardInert:
		st.reached = true
		return false
	case cardRecompute:
		return false
	}
	st.reached = true
	return true
}

// roundFor is the stamp whose card a row at pos counts toward under the
// root st: st itself, its last round's carrier, or nil. A row before the
// last round's prompt makes the root recompute.
func (st *cardStamp) roundFor(pos TimelineCursor) *cardStamp {
	if !st.rounds {
		return st
	}
	if cursorBefore(pos, st.lo) {
		st.state = cardRecompute
		return nil
	}
	return st.round
}

// addRow is a round's card taking one more row.
func (st *cardStamp) addRow(r subagentRow) {
	if st == nil || !st.takes() {
		return
	}
	v := &st.values
	v.Count = validInt(int(v.Count.Int64) + 1)
	if r.previewable() && r.beats(v.PickTurn, v.PickItem, v.PickID) {
		r.pick(v)
	}
	if r.newerThan(v.NewestTurn, v.NewestItem) {
		v.NewestTurn, v.NewestItem = validInt(r.turn), validInt(r.index)
	}
}

// changedRow is a round's card seeing a counted row's summary change.
func (st *cardStamp) changedRow(r subagentRow) {
	if st == nil || !st.takes() {
		return
	}
	v := &st.values
	switch {
	case v.PickID.Valid && v.PickID.String == r.id:
		if !r.previewable() {
			st.state = cardRecompute
			return
		}
		r.pick(v)
	case r.previewable() && r.beats(v.PickTurn, v.PickItem, v.PickID):
		r.pick(v)
	}
}

// trayRow is the parent's tray seeing a direct child written; changed
// reports a summary change of a row already stored.
func (st *cardStamp) trayRow(r subagentRow, changed bool) {
	if st == nil || !st.takes() {
		return
	}
	v := &st.values
	switch {
	case changed && v.ToolID.Valid && v.ToolID.String == r.id:
		if !r.toolable() {
			st.state = cardRecompute
			return
		}
		r.tool(v)
	case r.toolable() && r.beats(v.ToolTurn, v.ToolItem, v.ToolID):
		r.tool(v)
	}
}

// seedCardStamp turns a stored stamp into an accumulator: a clean stamp
// is live, a readTime one inert, and a dirty one, an unstamped carrier
// and an unstamped anchor with children recompute. An unstamped anchor
// without children is live from zero, as a new anchor is.
func seedCardStamp(q sqlQueryer, threadID string, target subagentStampTarget) (*cardStamp, error) {
	st := &cardStamp{id: target.id, carrier: target.root != ""}
	switch {
	case target.stamped && target.stored.State == aggStateClean:
		st.state, st.exists, st.gen = cardLive, true, target.gen
		st.stored, st.values = target.stored, target.stored
	case target.stamped && target.stored.State == aggStateReadTime:
		st.state, st.exists, st.gen = cardInert, true, target.gen
		st.stored, st.values = target.stored, target.stored
	case target.stamped || st.carrier:
		st.state = cardRecompute
	default:
		var hasChild bool
		if err := q.QueryRow(`SELECT `+aggHasChildSQL("?1", "?2", ""), threadID, target.id).Scan(&hasChild); err != nil {
			return nil, fmt.Errorf("store: probe subagent children %s/%s: %w", threadID, target.id, err)
		}
		if hasChild {
			st.state = cardRecompute
			break
		}
		st.state = cardLive
		st.stored = subagentStampValues{State: aggStateClean}
		st.values = st.stored
	}
	return st, nil
}

// cardStampOf is a recomputed stamp as an accumulator.
func cardStampOf(m subagentFamilyMember) *cardStamp {
	st := &cardStamp{id: m.id, carrier: m.root != "", exists: true, gen: m.gen,
		stored: m.values, values: m.values, rounds: m.rounds, lo: m.lo, roundID: m.roundID}
	if m.values.State != aggStateClean {
		st.state = cardInert
	}
	return st
}

// noteInsert feeds a committed counted row to the card's accumulators.
func (c *SubagentCard) noteInsert(r subagentRow) {
	if c.orphan {
		return
	}
	pos := r.position()
	for i, st := range c.levels {
		if r.prompt && i == 0 && st.id == r.parentID {
			c.notePrompt(st, r)
			continue
		}
		if !st.rounds {
			st.addRow(r)
			continue
		}
		if st.state != cardLive {
			continue
		}
		round := st.roundFor(pos)
		if st.state != cardLive {
			continue
		}
		v := &st.values
		v.TranscriptCount = validInt(int(v.TranscriptCount.Int64) + 1)
		if r.newerThan(v.TranscriptNewestTurn, v.TranscriptNewestItem) {
			v.TranscriptNewestTurn, v.TranscriptNewestItem = validInt(r.turn), validInt(r.index)
		}
		round.addRow(r)
	}
	if r.prompt && c.tray != nil && c.tray.carrier {
		// A round resumed from a carrier: a shape the recompute decides.
		c.tray.state = cardRecompute
		if r.carrier != "" {
			c.t.seeds[r.carrier] = struct{}{}
		}
	}
	c.tray.trayRow(r, false)
}

// notePrompt feeds a resume prompt written directly under the root st:
// it opens the root's next round. The root's card keeps its count, its
// transcript takes the prompt, and the carrier the prompt names opens its
// card with the prompt as its one row, a first stamp the flush writes
// only while the row is still that root's childless, unstamped carrier
// (flushSubagentCarrierInsertSQL). A prompt stored before a row the root
// already took, one naming the root itself, one naming a carrier the
// thread already holds, and one under a stamp that is not live recompute
// at the next flush.
func (c *SubagentCard) notePrompt(st *cardStamp, r subagentRow) {
	// A round can change which agent keeps the root's cards live.
	c.t.unresolve()
	v := &st.values
	if st.state != cardLive || r.carrier == st.id || c.t.stamps[r.carrier] != nil ||
		!r.newerThan(v.NewestTurn, v.NewestItem) || !r.newerThan(v.TranscriptNewestTurn, v.TranscriptNewestItem) {
		st.state, st.reached = cardRecompute, false
		if r.carrier != "" {
			c.t.seeds[r.carrier] = struct{}{}
		}
		return
	}
	base := v.Count.Int64
	if v.TranscriptCount.Valid {
		base = v.TranscriptCount.Int64
	}
	v.Count = validInt(int(v.Count.Int64))
	v.TranscriptCount = validInt(int(base) + 1)
	v.TranscriptNewestTurn, v.TranscriptNewestItem = validInt(r.turn), validInt(r.index)
	st.rounds, st.lo, st.round = true, r.position(), nil
	if r.carrier == "" {
		return
	}
	opened := subagentStampValues{State: aggStateClean}
	round := &cardStamp{id: r.carrier, state: cardLive, carrier: true, openedBy: st.id, stored: opened}
	round.values = opened
	round.values.Count = validInt(1)
	round.values.NewestTurn, round.values.NewestItem = validInt(r.turn), validInt(r.index)
	c.t.stamps[r.carrier] = round
	st.round = round
}

// noteChange feeds a committed summary change of a counted preview-kind
// row to the card's accumulators.
func (c *SubagentCard) noteChange(r subagentRow) {
	if c.orphan {
		return
	}
	for _, st := range c.levels {
		target := st
		if st.rounds {
			if st.state != cardLive {
				continue
			}
			target = st.roundFor(r.position())
		}
		target.changedRow(r)
	}
	c.tray.trayRow(r, true)
}

// Generations.

// subagentGenSeq issues stamp generations. It starts at a random point so
// that a generation read before a restart is not issued again after it.
var subagentGenSeq = func() *atomic.Int64 {
	var seq atomic.Int64
	var seed [8]byte
	if _, err := rand.Read(seed[:]); err == nil {
		seq.Store(int64(binary.LittleEndian.Uint64(seed[:]) >> 2))
	}
	return &seq
}()

// newSubagentGen is a generation no stamp row has held.
func newSubagentGen() int64 { return subagentGenSeq.Add(1) }
