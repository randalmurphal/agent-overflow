package settings

// The persisted-write chokepoint.
//
// Every mutation this package persists goes through `mutate`. It is the only
// caller of writeSparse, the only place the read cache is restamped, and the
// only place a change is announced. Before it, five methods repeated the same
// load / write / restamp dance and nothing announced anything, so
// `settings:updated` would have been five separate emit sites — one of which a
// later mutator would inevitably forget.
//
// TestOnlyMutateWritesSettings fails if a second writeSparse call site
// appears, which is what keeps the chokepoint true rather than customary.

// TierChange is one tier's share of a settings write: the tier, and the keys
// belonging to it that the write actually moved. A write touching keys of two
// tiers produces two of these, because the tiers are separately authorized
// (§6) and separately stored (residency.go) — one event per tier is what
// keeps the storage split invisible on the wire. A device-tier frame prompts
// every attached client to re-read, and each one gets its own values, which
// is exactly right.
type TierChange struct {
	Tier Tier     `json:"tier"`
	Keys []string `json:"keys"`
}

// ChangeObserver is notified after a persisted settings write that actually
// changed something. It runs on the calling goroutine with the service lock
// RELEASED, so an observer is free to read settings back — the `settings:updated`
// broadcast is exactly such a caller by construction.
type ChangeObserver func(changes []TierChange)

// SetChangeObserver installs the single observer notified after each persisted
// change. Passing nil clears it. Wired once at startup by internal/app; the
// service does not fan out, because one broadcast chokepoint is the whole
// point of having one write chokepoint.
func (s *Service) SetChangeObserver(observe ChangeObserver) {
	s.observerMu.Lock()
	defer s.observerMu.Unlock()
	s.observer = observe
}

func (s *Service) changeObserver() ChangeObserver {
	s.observerMu.RLock()
	defer s.observerMu.RUnlock()
	return s.observer
}

// mutate is the one persisted-write path.
//
// It loads the current settings for THIS caller under the write lock, hands
// them to apply, persists whatever apply returns into each key's own storage
// (residency.go), restamps the read cache, and — after releasing the lock —
// announces the keys the write actually moved. A write that changed nothing
// announces nothing: `settings:updated` follows the same rule as
// `thread:updated`, and a repeat save of an unchanged form must not make every
// attached client re-read.
//
// bucket names the caller's device-tier `ui_state` scope, empty for a call
// with no connection behind it (which resolves to the backend machine's own
// screen). The SAME scope is used for the pre-read and the write, so a
// device-tier value that did not move is not reported as if it had.
//
// apply owns its own validation. mutate deliberately does NOT validate on its
// behalf: Update and the provider-environment mutators validate the whole
// struct, while AddRecentWorkspace and the remote-endpoint CRUD validate only
// the fields they touch, and hoisting whole-struct validation here would make
// a hand-edited out-of-range value block writes that succeed today.
//
// An apply that reports an error leaves every store, the cache, and the
// observer untouched. A write spanning two stores is not one transaction —
// SQLite and a JSON file cannot be — so a failure part-way through returns the
// error without restamping the cache or announcing anything; the next read
// reloads from storage and sees whatever landed.
func (s *Service) mutate(bucket string, apply func(current Settings) (Settings, error)) (Settings, error) {
	var changes []TierChange
	result, err := func() (Settings, error) {
		s.mu.Lock()
		defer s.mu.Unlock()

		deviceScope := bucket
		if deviceScope == "" {
			deviceScope = s.backendBucket
		}
		current := s.loadFromFile()
		if s.store != nil && deviceScope != "" {
			current = sanitizeLoadedSettings(overlayScope(current, s.store, deviceScope, TierDevice))
		}
		// Probed BEFORE apply runs: apply is allowed to edit the value it is
		// handed in place (UpdateRemoteEndpoint assigns into the endpoint
		// slice), and a projection taken afterwards would compare the result
		// against itself.
		before, err := keyValues(current)
		if err != nil {
			return Settings{}, err
		}

		next, err := apply(current)
		if err != nil {
			return Settings{}, err
		}
		after, err := keyValues(next)
		if err != nil {
			return Settings{}, err
		}
		moved := changedKeys(before, after)
		fileKeys, userKeys, deviceKeys := s.routeKeys(moved)

		// The file is written unless this write moved ONLY keys that no
		// longer live in it. A font size must not fsync settings.json; a
		// write that moved nothing still writes, because that is how a first
		// save creates the file and how a stamped schema version lands.
		if len(moved) == 0 || len(fileKeys) > 0 {
			if err := s.writeSparse(next); err != nil {
				return Settings{}, err
			}
		}
		if len(userKeys) > 0 {
			if err := s.persistScope(s.store, UserScope, userKeys, after); err != nil {
				return Settings{}, err
			}
		}
		if len(deviceKeys) > 0 {
			if err := s.persistScope(s.store, deviceScope, deviceKeys, after); err != nil {
				return Settings{}, err
			}
		}

		// Cache the SHARED half only. next carries one caller's device slice,
		// and handing that to every later Get would make one screen's font
		// size the backend's answer for all of them.
		snapshot := next
		if s.store != nil {
			applyRows(&snapshot, defaultKeyValues(), TierDevice)
		}
		s.cached = &snapshot
		s.cachedState = readFileState(s.path)

		changes = groupByTier(moved)
		return next, nil
	}()
	if err != nil {
		return Settings{}, err
	}
	if len(changes) > 0 {
		if observe := s.changeObserver(); observe != nil {
			observe(changes)
		}
	}
	return result, nil
}

// routeKeys splits the keys a write moved by where each one persists. Must be
// called with s.mu held.
func (s *Service) routeKeys(moved []string) (file, user, device []string) {
	for _, key := range moved {
		if s.fileResident(key) {
			file = append(file, key)
			continue
		}
		if tier, _ := TierForKey(key); tier == TierUser {
			user = append(user, key)
			continue
		}
		device = append(device, key)
	}
	return file, user, device
}

// groupByTier folds a sorted key list into one entry per tier, in host → user
// → device order so the emitted sequence is deterministic. Keys stay sorted
// within their tier because the input is.
func groupByTier(keys []string) []TierChange {
	if len(keys) == 0 {
		return nil
	}
	byTier := make(map[Tier][]string, 3)
	for _, key := range keys {
		tier, _ := TierForKey(key)
		byTier[tier] = append(byTier[tier], key)
	}
	changes := make([]TierChange, 0, len(byTier))
	for _, tier := range []Tier{TierHost, TierUser, TierDevice} {
		if tierKeys := byTier[tier]; len(tierKeys) > 0 {
			changes = append(changes, TierChange{Tier: tier, Keys: tierKeys})
		}
	}
	return changes
}
