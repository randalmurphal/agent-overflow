package trackertools

import (
	"fmt"
	"slices"
	"sync"
)

// Read marks. An edit that replaces a long text (a description, a rich
// text field, a text a read cut) writes what the agent has of it back
// whole. What this keeps true: such an edit is sent only when the session
// was given that text whole at the ticket's current version, or wrote it
// itself in a write the tracker confirmed, and when the edit carries the
// text it replaces, so the tracker refuses it as stale when the text
// changed after the call planned (plan.go). A mark changes through two
// events only: an answer the session was given (shown), and a write of
// the session's call that the tracker confirmed (wrote). When a write
// applies, the mark moves to the version the write's refresh read and
// holds only the texts that write confirmed; every other text of the
// ticket is unread at that version. A session that ended has no marks,
// and nothing recreates them: a read records nothing for a session the
// host no longer holds (Host.Live), checked under the lock that EndSession
// takes after the host let the session go, and a write moves only a mark
// that is there.
//
// The server owns the marks, in memory, per session (Caller.Session): for
// each ticket, by connection and issue, the update time of the ticket the
// session last read, and for each text the read cut or showed, how much
// of it from its start the session was given, by the first read and each
// continuation of that same version. A first read starts the ticket's
// mark over; a continuation of the same version adds what it showed to
// the text it continues. A session holds at most maxMarkedTickets, the
// oldest read going first, and its marks go when its capability ends
// (EndSession). The check (textCheck) is made in a write's plan, before
// anything is sent.

// maxMarkedTickets bounds the tickets one session's marks hold.
const maxMarkedTickets = 256

// The texts a mark names: the description, and a field's value item.
const textDescription = "d"

func fieldText(field string, item int) string { return fmt.Sprintf("v %s %d", field, item) }

type markKey struct{ connection, issue string }

// textMark is how much of a text the session was given from its start,
// and the text's length then; own is a text the session wrote itself,
// which it has whole however the tracker stored it.
type textMark struct {
	given, length int
	own           bool
}

// whole reports a text the session has all of, at length now.
func (m textMark) whole(length int) bool { return m.own || m.given >= m.length && m.length == length }

type readMark struct {
	updated int64
	texts   map[string]textMark
	// wrote reports a mark a write of the session moved (wrote): a text
	// it does not hold changed under the session's read, not cut from it.
	wrote bool
}

// givenText is a piece of a text an answer showed: the text, the bytes
// from and to, and the text's length.
type givenText struct {
	text     string
	from, to int
	length   int
}

type sessionMarks struct {
	order []markKey // oldest read first
	marks map[markKey]*readMark
}

type marks struct {
	mu       sync.Mutex
	sessions map[string]*sessionMarks
	// live reports whether the host still holds a caller's session.
	live func(Caller) bool
}

func (m *marks) end(session string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, session)
}

// session is session's marks, made when it has none. m.mu is held.
func (m *marks) session(session string) *sessionMarks {
	if m.sessions == nil {
		m.sessions = map[string]*sessionMarks{}
	}
	sm := m.sessions[session]
	if sm == nil {
		sm = &sessionMarks{marks: map[markKey]*readMark{}}
		m.sessions[session] = sm
	}
	return sm
}

// touch is k's mark, the newest read, made fresh at updated when start is
// set or the mark is of another version. m.mu is held.
func (sm *sessionMarks) touch(k markKey, updated int64, start bool) *readMark {
	if i := slices.Index(sm.order, k); i >= 0 {
		sm.order = slices.Delete(sm.order, i, i+1)
	}
	sm.order = append(sm.order, k)
	mk := sm.marks[k]
	if start || mk == nil || mk.updated != updated {
		mk = &readMark{updated: updated, texts: map[string]textMark{}}
		sm.marks[k] = mk
	}
	for len(sm.order) > maxMarkedTickets {
		delete(sm.marks, sm.order[0])
		sm.order = sm.order[1:]
	}
	return mk
}

// shown records what an answer of the ticket k at update time updated
// gave: a first read (first) starts the mark over; a continuation adds
// to the texts it continues. A piece that does not start where the
// session's part of its text ends adds nothing.
func (m *marks) shown(caller Caller, k markKey, updated int64, given []givenText, first bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.live(caller) {
		return
	}
	mk := m.session(caller.Session).touch(k, updated, first)
	for _, g := range given {
		tm := mk.texts[g.text]
		tm.length = g.length
		if g.from <= tm.given {
			tm.given = max(tm.given, g.to)
		}
		mk.texts[g.text] = tm
	}
}

// wrote records that a write of the session's call, which the tracker
// confirmed, wrote texts whole and left ticket k at updated: a mark of
// the version the call planned from moves to it, holding those texts
// alone, since the agent wrote them and any other may have changed.
func (m *marks) wrote(caller Caller, k markKey, from, updated int64, texts []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Only a mark there moves: a session that ended has none.
	sm := m.sessions[caller.Session]
	if sm == nil {
		return
	}
	mk := sm.marks[k]
	if mk == nil || mk.updated != from {
		return
	}
	mk.updated, mk.wrote = updated, true
	mk.texts = make(map[string]textMark, len(texts))
	for _, t := range texts {
		mk.texts[t] = textMark{own: true}
	}
}

// partOf reports whether a read of ticket k at update time updated gave
// the session some of field's values (texts, as the ticket holds them now)
// and not all of them whole: a value cut, or one after the read's cut.
func (m *marks) partOf(session string, k markKey, updated int64, field string, texts []string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	sm := m.sessions[session]
	if sm == nil {
		return false
	}
	mk := sm.marks[k]
	if mk == nil || mk.updated != updated {
		return false
	}
	seen, all := false, true
	for j, t := range texts {
		tm, ok := mk.texts[fieldText(field, j)]
		seen = seen || ok
		all = all && ok && tm.whole(len(t))
	}
	return seen && !all
}

// textCheck refuses an edit of text (named to the model as what, such as
// "description") on ticket k, shown as key, at update time updated, when
// the session was not given it whole, length bytes, at that version; a
// text that is empty now loses nothing and passes.
func (m *marks) textCheck(session string, k markKey, key string, updated int64, text, what string, length int) error {
	if length == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var mk *readMark
	if sm := m.sessions[session]; sm != nil {
		mk = sm.marks[k]
	}
	switch {
	case mk == nil:
		return invalidf("Read %s before you change its %s.", key, what)
	case mk.updated != updated:
		return invalidf("%s changed since you read it; read it again before you change its %s.", key, what)
	}
	tm, ok := mk.texts[text]
	switch {
	case !ok && mk.wrote:
		return invalidf("%s changed since you read it; read it again before you change its %s.", key, what)
	case !ok || !tm.whole(length):
		return invalidf("You have read only part of %s's %s; read the rest of it through more before you change it.", key, what)
	}
	return nil
}
