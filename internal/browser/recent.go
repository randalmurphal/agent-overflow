package browser

import (
	"net/url"
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
// the list.

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
// only while p is still the thread's live page in its thread's current jar
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
	if jarKindOf(m.sessionLocked(p.owner)) == JarPrivate {
		return
	}
	previous := m.recent[p.owner]
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
	if m.recent == nil {
		m.recent = make(map[string][]RecentAddress)
	}
	m.recent[p.owner] = next
}

// RecentAddresses answers the thread's recent addresses, newest first.
func (m *Manager) RecentAddresses(access Access) []RecentAddress {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.recent[access.ThreadID]
	return append(make([]RecentAddress, 0, len(kept)), kept...)
}
