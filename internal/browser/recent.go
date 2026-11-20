package browser

import (
	"net/url"
	"strconv"
	"strings"
)

// A thread's recent addresses, for the pane's address suggestions. In memory
// only and bounded: the addresses an open (Open, OpenFile, which the address
// bar's navigation and the agent's browser_open and browser_open_file share)
// loaded, newest first, one entry per address. A link followed inside a page,
// a history move, a reload and a restore are not opens and add nothing.
//
// Only a committed load is recorded: an engine's failed navigation is an
// error and records nothing, and a page left on a blank or internal document
// (WebKitGTK reports about:blank after a failed load) has no address to
// offer. Nothing is kept for a private jar. A jar switch, closing every page
// (Clear site data, turning the browser off, changing whether site data
// persists: forgetAllPages) and closing the thread (delete, archive) forget
// the list, and every forget goes through forgetBrowsingLocked or
// forgetAllBrowsingLocked, which forget the closed tabs with it and advance
// the Manager's forget generation; a thread's close detaches its lifetime,
// browsing and all (thread_life.go).
//
// The generation rides each thread's state push (CompanionEvent.RecentGen):
// a client that cached a list drops it when the token changes, which a round
// trip of jar names (worktree, private, worktree) cannot hide. It is
// Manager-wide, so it never repeats after a thread's entry is deleted; a
// forget on another thread costs a client one re-read at its next push.
// It crosses as a string token, the Manager's launch identity and the
// count (recentGenTokenLocked): a JSON number past 2^53 loses its low
// digits in JavaScript, and a restarted backend, counting again, never
// repeats a token a client still holds.

// maxRecentAddresses bounds a thread's list.
const maxRecentAddresses = 50

// maxRecentAddressBytes is the longest address kept; a longer one (a page
// carrying state in its URL) is not something to offer back.
const maxRecentAddressBytes = 2 << 10

// maxRecentTitleBytes bounds a kept title.
const maxRecentTitleBytes = 200

// RecentAddress is one address a thread's page loaded, with the title it had.
type RecentAddress struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

// recordableAddress reports whether a loaded address is worth offering
// again: a web address with a host or a file, within the length bound.
func recordableAddress(raw string) bool {
	if raw == "" || len(raw) > maxRecentAddressBytes {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return parsed.Host != ""
	case "file":
		return parsed.Path != "" || parsed.Opaque != ""
	}
	return false
}

// recordRecentAddress records the address p shows after an open. It is kept
// only while p is still its thread lifetime's live page in its current jar
// and that jar is not private, so an open that a jar switch, the close of
// every page or the thread's close overtook records nothing.
func (m *Manager) recordRecentAddress(p *managedPage, info PageInfo) {
	if !recordableAddress(info.URL) {
		return
	}
	entry := RecentAddress{URL: info.URL, Title: truncateUTF8(info.Title, maxRecentTitleBytes)}
	m.mu.Lock()
	defer m.mu.Unlock()
	if live, _ := m.livePageLocked(p.id); live != p || !m.jarCurrentLocked(p) {
		return
	}
	tb := p.owner
	if jarKindOf(tb.session) == JarPrivate {
		return
	}
	previous := tb.browsing.recent
	next := make([]RecentAddress, 0, min(len(previous)+1, maxRecentAddresses))
	next = append(next, entry)
	for _, kept := range previous {
		if len(next) == maxRecentAddresses {
			break
		}
		if kept.URL != entry.URL {
			next = append(next, kept)
		}
	}
	tb.browsing.recent = next
}

// forgetBrowsingLocked forgets tb's browsing history, its recent addresses
// and its closed tabs (companion_tabs.go), by replacing the object that
// holds them, which voids every claim on the old one (tabClaim), and
// advances the forget generation the address cache reads. The caller holds
// m.mu.
func (m *Manager) forgetBrowsingLocked(tb *threadBrowser) {
	tb.browsing = &browsing{}
	m.recentGen++
}

// forgetAllBrowsingLocked forgets every thread lifetime's browsing history
// and advances the forget generation, answering the threads that had any.
// The caller holds m.mu.
func (m *Manager) forgetAllBrowsingLocked() []ThreadID {
	var threads []ThreadID
	for threadID, tb := range m.threads {
		if len(tb.browsing.recent) > 0 || closedCountLocked(tb) > 0 {
			threads = append(threads, threadID)
		}
		tb.browsing = &browsing{}
	}
	m.recentGen++
	return threads
}

// recentGenTokenLocked is the forget generation as a state push carries it.
// The caller holds m.mu.
func (m *Manager) recentGenTokenLocked() string {
	return m.recentLaunch + "." + strconv.FormatUint(m.recentGen, 10)
}

// RecentAddresses answers the recent addresses of caller's thread as it is
// now, newest first.
func (m *Manager) RecentAddresses(caller Caller) []RecentAddress {
	m.mu.Lock()
	defer m.mu.Unlock()
	tb := m.currentLocked(caller.ThreadID)
	if tb == nil {
		return []RecentAddress{}
	}
	kept := tb.browsing.recent
	return append(make([]RecentAddress, 0, len(kept)), kept...)
}
