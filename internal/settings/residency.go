package settings

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
)

// Tiered residency: ONE service, three homes (docs/specs/remote-access.md §6,
// "Phase-4 design decisions").
//
// The wire keeps `GetSettings` / `UpdateSettings` and the `Settings` struct
// keeps every field. What moved is where each field's value actually lives:
//
//   - HOST tier stays in settings.json. It configures the backend before
//     identity or the database matter, and must be hand-editable when the UI
//     is unreachable.
//   - USER tier lives in `ui_state` under the reserved scope `user:default`.
//     Real user identities land in phase 8; until then the scope string is the
//     contract, and nothing else may name it.
//   - DEVICE tier lives in the CALLING connection's own `ui_state` bucket —
//     `device:<id>` for a paired device, `client:<id>` for a screen on the
//     local page channel. internal/app derives that string from the
//     connection and hands it here; this package never guesses one.
//
// The split is driven entirely by tierByKey, so retiering a key relocates its
// storage and nothing else has to be told.
//
// STORAGE FORMAT. One `ui_state` row per settings key, the key spelled
// EXACTLY as the settings JSON key ("fontSize", "confirmDelete", …) and the
// value the JSON encoding of the typed field. The store stays opaque: typed
// validation happens in this package before the write, which is what §6's
// "typed validation over the same store" means.
//
// The exact spelling cannot collide with the frontend's own appStorage keys,
// which share these buckets. Every appStorage key is either namespaced with a
// colon — `sidebar:width`, `sidebar:collapsed`, `sidebar:collapsedProjects`,
// `sidebar:expandedDiscussions`, `sidebar:threadListVisibleLimits`,
// `workflows:overlay`, `reviewScope:<threadID>`, `branch-mru:<projectID>` —
// or one of the three flat legacy names `paneLayout`, `reviewTreeVisible` and
// `reviewTreeWidth`. No settings JSON key contains a colon, and none of those
// three names is a Settings field (`paneLayout` is retired, and the review
// pair never was one). A future settings key would have to be spelled
// `paneLayout`, `reviewTreeVisible` or `reviewTreeWidth` to collide, which is
// what the tier map's review makes visible.
//
// A store-less Service keeps every tier in the file, which is the pre-phase-4
// behaviour. That is not a fallback nobody meant: main.go and main_desktop.go
// construct one before the database exists to read the bind and window
// settings at boot, and a settings file is a complete answer for host keys.

// UserScope is the reserved `ui_state` scope holding the user tier until
// phase 8 introduces real user identities (§6). Exported because the app
// layer's scope derivation must keep `user:` out of the buckets a connection
// can name, and two spellings of one contract is one too many.
const UserScope = "user:default"

// TierStore is the `ui_state` residency behind the user and device tiers.
// *store.Store satisfies it; this package declares the interface so it stays
// free of a database dependency.
type TierStore interface {
	GetUIState(scope string) (map[string]string, error)
	SetUIState(scope string, entries map[string]string) error
}

// AttachTierStore moves the user and device tiers out of settings.json and
// into store, then seeds them from whatever the file still holds.
//
// backendBucket is the `ui_state` bucket of the BACKEND MACHINE's own screen
// (`client:<EnsureClientIDIn dbDir>`). It is where the file's device-tier
// values are seeded, and where a device-tier write with no caller behind it
// lands — a write nobody can attribute is still a write somebody made, and
// dropping it would be worse than putting it on this machine's own screen.
//
// Called once, at boot, before anything reads settings. Seeding failures log
// and continue: the worst case is a preference reverting to its default,
// which is not worth failing a boot over.
func (s *Service) AttachTierStore(store TierStore, backendBucket string) {
	if store == nil {
		return
	}
	s.mu.Lock()
	s.store = store
	s.backendBucket = backendBucket
	s.mu.Unlock()
	// The cached snapshot predates the store, so its user half came from the
	// file. Drop it rather than serve it.
	s.InvalidateTierCache()

	s.seedTiers()
}

// InvalidateTierCache drops the cached snapshot, so the next read rebuilds the
// user tier from the store.
//
// The FILE half of that cache invalidates itself — Get compares the file's
// size and modification time on every read — but a `ui_state` row can move
// without the file moving at all. So a writer that reaches the table AROUND
// this service owes the service this call. There is exactly one: the harness
// reset, which drops every scope wholesale (Store.ClearUIState).
func (s *Service) InvalidateTierCache() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cached = nil
	s.cachedState = fileState{}
}

// Caller is this service seen from one connection: the same settings, with
// the device tier resolved out of THAT caller's bucket, over THAT caller's
// device-class defaults (classdefaults.go).
//
// The bucket is empty for a caller with no device of its own — an in-process
// saga, a test, a tool holding the launch credential. Such a caller reads the
// class defaults, because it has no screen whose preferences it could be
// asking for, and its writes land in the backend machine's own bucket.
type Caller struct {
	svc    *Service
	bucket string
	class  DeviceClass
}

// For binds the service to one caller's device bucket AND the class of screen
// behind it. `For("", DeviceDesktop)` is the backend's own view, which is what
// the bucket-free Service methods use.
//
// The class is a parameter rather than a second method because a bucket and a
// class are two halves of one answer to "which screen is this?", and the bug
// this wave exists to prevent is exactly the two travelling apart — a paired
// phone read through a call that named its bucket and forgot its class would
// silently answer desktop defaults. Every caller therefore states one; a
// caller that genuinely cannot name the screen passes DeviceDesktop, which is
// what every device resolved as before the table existed.
func (s *Service) For(bucket string, class DeviceClass) Caller {
	return Caller{svc: s, bucket: bucket, class: class}
}

// BackendScreen binds the service to the BACKEND MACHINE's own screen — the
// bucket AttachTierStore was given.
//
// It is for backend code that acts on that screen and no other: today, the
// host-side OS-notification sender, which presents on the machine this
// process runs on (in-process on macOS and Linux, bridged to the Windows
// launcher on WSL — one machine either way). `For("")` would answer device
// DEFAULTS there, silently ignoring the preferences the user set on the very
// screen being interrupted.
//
// It is NOT a general-purpose "the device tier, globally". A key that has no
// screen behind it does not belong in the device tier at all (§6's retiering
// rule, and the note on backgroundGitFetch in tier.go). Reach for this only
// when the caller can name the screen it is acting on and that screen is
// this machine's.
//
// A store-less service, or one whose backend bucket never resolved, answers
// the same defaults `For("", DeviceDesktop)` would: no bucket, no
// preferences, no failure.
//
// The class is DESKTOP because that is what this screen is: the machine
// running the backend, presenting in its own window (or through the Windows
// launcher, which is the same machine).
func (s *Service) BackendScreen() Caller {
	s.mu.RLock()
	bucket := s.backendBucket
	s.mu.RUnlock()
	return s.For(bucket, DeviceDesktop)
}

// Get returns the settings this caller sees: the shared host+user snapshot
// with its class defaults and then its own device slice overlaid.
func (c Caller) Get() Settings {
	return c.svc.getFor(c.bucket, c.class)
}

// Update applies a partial patch, routing each key to its tier's storage, and
// returns the caller's own view of the result.
func (c Caller) Update(patch map[string]any) (Settings, error) {
	return c.svc.update(c.bucket, c.class, patch)
}

// AddRecentWorkspace pushes a workspace path onto this caller's recent list.
func (c Caller) AddRecentWorkspace(path string) {
	c.svc.addRecentWorkspace(c.bucket, c.class, path)
}

// getFor is Get plus the caller's class defaults and then its device overlay.
// The host+user half is the cached in-memory snapshot; the class half is a
// table lookup; the device half is one `ui_state` read.
//
// The two device layers are applied in that order and never merged: the class
// row is what a screen of this kind gets, the bucket's rows are what THIS
// screen chose, and a choice outranks a class. sanitizeLoadedSettings runs
// once at the end, over the merged result, so a class default is clamped by
// exactly the rules a hand-edited row is.
//
// Deliberately NOT cached per bucket. The rows a bucket holds are written by
// this package AND by the frontend's appStorage through store.SetUIState, so a
// cache here would need an invalidation edge from a package that knows nothing
// about settings — a staleness bug in exchange for saving one indexed SELECT
// on a table with a handful of rows, on an RPC the UI issues at page load and
// on `settings:updated`. Backend logic never takes this path at all: it reads
// Get(), which touches no database and belongs to no screen.
//
// A STORE-LESS service applies neither layer. That is the pre-database boot
// reader (main.go, main_desktop.go), where the device tier still lives in
// settings.json — so the file's own value is the screen's write, and a class
// row applied over the top would outrank it. No store, no residency, no
// class.
func (s *Service) getFor(bucket string, class DeviceClass) Settings {
	current := s.Get()
	s.mu.RLock()
	store := s.store
	s.mu.RUnlock()
	rows := classOverrides(class)
	if store == nil || (bucket == "" && len(rows) == 0) {
		// Nothing to overlay: the shared snapshot is already sanitized, so
		// re-running the pass here would be work with no possible effect.
		return current
	}
	applyRows(&current, rows, TierDevice)
	if bucket != "" {
		current = overlayScope(current, store, bucket, TierDevice)
	}
	return sanitizeLoadedSettings(current)
}

// fileResident reports whether a settings key persists in settings.json.
// With no store attached every key does, which is the pre-phase-4 service.
// Must be called with s.mu held.
func (s *Service) fileResident(key string) bool {
	if s.store == nil {
		return true
	}
	tier, ok := TierForKey(key)
	// An unclassified key is host-tier and false, the same fail-closed answer
	// TierForKey gives everywhere else: a key nobody placed keeps persisting
	// where every key used to.
	return !ok || tier == TierHost
}

// overlayScope returns current with the rows of one `ui_state` scope applied
// over it, keeping only keys belonging to want.
//
// A read failure logs and answers the input unchanged: a device whose bucket
// is briefly unreadable renders on defaults rather than failing the RPC that
// asked.
func overlayScope(current Settings, store TierStore, scope string, want Tier) Settings {
	rows, err := store.GetUIState(scope)
	if err != nil {
		log.Printf("settings: read %s tier from %q: %v", want, scope, err)
		return current
	}
	applyRows(&current, rows, want)
	return current
}

// applyRows decodes the rows belonging to want onto target.
//
// One object unmarshal for the whole batch, because json.Unmarshal into an
// existing struct writes only the fields the object names. A batch that fails
// is retried key by key so ONE hand-edited row cannot cost every other
// preference in the bucket.
func applyRows(target *Settings, rows map[string]string, want Tier) {
	if len(rows) == 0 {
		return
	}
	object := make(map[string]json.RawMessage, len(rows))
	for key, value := range rows {
		if tier, ok := TierForKey(key); !ok || tier != want {
			continue
		}
		object[key] = json.RawMessage(value)
	}
	if len(object) == 0 {
		return
	}
	if err := decodeObject(target, object); err == nil {
		return
	}
	for key, value := range object {
		if err := decodeObject(target, map[string]json.RawMessage{key: value}); err != nil {
			log.Printf("settings: drop unreadable %s row %q: %v", want, key, err)
		}
	}
}

func decodeObject(target *Settings, object map[string]json.RawMessage) error {
	encoded, err := json.Marshal(object)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}

// persistScope writes the changed keys of one tier into its scope. Values are
// taken from the post-write per-key projection, so what lands in a row is
// exactly what the validated struct encodes.
//
// A key restored to its default writes a row holding the default rather than
// deleting one. That is the opposite of settings.json's sparse rule and it is
// deliberate: an absent row is what SEEDING treats as "not migrated yet", so
// deleting one would let the next boot re-seed the stale file value over a
// preference the user just cleared.
func (s *Service) persistScope(store TierStore, scope string, keys []string, values map[string]string) error {
	if scope == "" {
		return fmt.Errorf("settings: no %d-key scope to write to", len(keys))
	}
	entries := make(map[string]string, len(keys))
	for _, key := range keys {
		entries[key] = values[key]
	}
	if err := store.SetUIState(scope, entries); err != nil {
		return fmt.Errorf("settings: persist %q: %w", scope, err)
	}
	return nil
}

// seedTiers is the one-shot move of the user- and device-tier values that
// settings.json still holds into their new scopes: user values into
// UserScope, device values into the backend machine's own screen bucket.
// Other devices start from the defaults, exactly as §6 says.
//
// The pattern migrateUIStateFromSettings set: never overwrite an existing
// row, log and continue on every failure. Idempotent by that rule alone — a
// key with a row is a key already migrated — so no marker is needed and a
// re-seed cannot resurrect a value the user has since changed.
//
// The file's copies are deliberately left where they are. loadFromFile
// ignores a non-host key once a store is attached, and the next write that
// moves a host key drops them for free, because writeSparse emits only what
// is still file-resident.
func (s *Service) seedTiers() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("settings: read %s for tier seeding: %v", s.path, err)
		}
		return
	}
	// The raw object names what the FILE actually carries; the typed load
	// beside it sanitizes those values, so a hand-edited file cannot seed a
	// value Update would have refused.
	var raw map[string]json.RawMessage
	loaded := copyDefaults()
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Printf("settings: parse %s for tier seeding: %v", s.path, err)
		return
	}
	if err := json.Unmarshal(data, &loaded); err != nil {
		log.Printf("settings: decode %s for tier seeding: %v", s.path, err)
		return
	}
	values, err := keyValues(sanitizeLoadedSettings(loaded))
	if err != nil {
		log.Printf("settings: project file for tier seeding: %v", err)
		return
	}

	s.mu.RLock()
	store, backendBucket := s.store, s.backendBucket
	s.mu.RUnlock()
	if store == nil {
		return
	}

	defaults := defaultKeyValues()
	for _, target := range []struct {
		tier  Tier
		scope string
	}{
		{TierUser, UserScope},
		{TierDevice, backendBucket},
	} {
		if target.scope == "" {
			log.Printf("settings: no scope to seed the %s tier into", target.tier)
			continue
		}
		existing, err := store.GetUIState(target.scope)
		if err != nil {
			log.Printf("settings: read %q before seeding: %v", target.scope, err)
			continue
		}
		entries := make(map[string]string, len(raw))
		for key := range raw {
			if tier, ok := TierForKey(key); !ok || tier != target.tier {
				continue
			}
			if _, taken := existing[key]; taken {
				continue
			}
			value, ok := values[key]
			if !ok || value == defaults[key] {
				continue
			}
			entries[key] = value
		}
		if len(entries) == 0 {
			continue
		}
		if err := store.SetUIState(target.scope, entries); err != nil {
			log.Printf("settings: seed the %s tier into %q: %v", target.tier, target.scope, err)
			continue
		}
		log.Printf("settings: seeded %d %s-tier key(s) into %s", len(entries), target.tier, target.scope)
	}

	// The snapshot taken before seeding held defaults for everything just
	// written.
	s.mu.Lock()
	s.cached = nil
	s.mu.Unlock()
}
