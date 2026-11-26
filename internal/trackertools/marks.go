package trackertools

import (
	"fmt"
	"slices"
	"sync"
)

// Read marks. An edit that replaces a long text (a description, a rich
// text field, a text a read cut) writes what the agent has of it back
// whole, so it is made only from a read of the ticket as it is now that
// gave the agent all of it. What this keeps true: no such edit is sent
// when this session never read the ticket, when the ticket changed since
// the read (another person's change would be overwritten), or when the
// read gave part of the text and the agent did not read the rest through
// more (the rest would be lost).
//
// The server owns the marks, in memory, per session (Caller.Session): for
// each ticket, by connection and issue, the update time of the ticket the
// session last read, and for each text the read cut or showed, how much
// of it from its start the session was given, by the first read and each
// continuation of that same version. A first read starts the ticket's
// mark over; a continuation of the same version adds what it showed to
// the text it continues. A session holds at most maxMarkedTickets, the
// oldest read going first, and its marks go when its capability ends
// (EndSession). The one check (textCheck) is made in a write's pre-read,
// before anything is sent; the call's own edit of a text marks it whole
// at the update time the write left.

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
func (m *marks) shown(session string, k markKey, updated int64, given []givenText, first bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mk := m.session(session).touch(k, updated, first)
	for _, g := range given {
		tm := mk.texts[g.text]
		tm.length = g.length
		if g.from <= tm.given {
			tm.given = max(tm.given, g.to)
		}
		mk.texts[g.text] = tm
	}
}

// wrote records that the session's own write of texts left ticket k at
// updated: a mark of the version the write was made from moves to it,
// those texts whole, since the agent wrote them.
func (m *marks) wrote(session string, k markKey, from, updated int64, texts []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sm := m.session(session)
	mk := sm.marks[k]
	if mk == nil || mk.updated != from {
		return
	}
	mk.updated = updated
	for _, t := range texts {
		mk.texts[t] = textMark{own: true}
	}
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
	if tm, ok := mk.texts[text]; !ok || !tm.whole(length) {
		return invalidf("You have read only part of %s's %s; read the rest of it through more before you change it.", key, what)
	}
	return nil
}
