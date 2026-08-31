// Package entityid mints the identifiers of entities whose uniqueness has
// to hold beyond one backend.
//
// A client may hold several backends at once — one browser profile, one
// phone, one desktop attached to both a laptop and a home server
// (docs/specs/remote-access.md §10). Thread and project ids are what such
// a client keys its stores, its IndexedDB replica and its deep links by,
// and leaving those stores un-keyed by backend is only safe BECAUSE the
// ids are random UUIDs: two backends that both minted "t-1" would collide
// in every one of those maps, silently, with the wrong thread's content
// under the right thread's id.
//
// The format is therefore a contract rather than an implementation
// detail. Mint here — never from a counter, a slug, a path hash, or a
// short id — and the contract holds by construction at every site.
//
// Out of scope: ids that are only ever addressed THROUGH the thread that
// owns them (items, turns, payloads). They inherit that thread's
// uniqueness, and re-stating the rule for them would blur where it
// actually bites.
package entityid

import "github.com/google/uuid"

// New returns a fresh id, unique across backends and across machines.
func New() string {
	return uuid.NewString()
}

// Valid reports whether id could have come from New: a random (version 4)
// UUID in the canonical lowercase hyphenated form.
//
// The canonical-form check is deliberate. uuid.Parse also accepts the
// braced and urn: spellings, and those compare unequal to the string the
// backend stored, so accepting them here would let an id that no map can
// match pass as valid.
func Valid(id string) bool {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return false
	}
	return parsed.Version() == 4 && parsed.String() == id
}
