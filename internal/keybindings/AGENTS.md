# internal/keybindings/

Owns the persisted keybindings config (shipped `Defaults` + the user's
overrides) and the merge that produces the effective list the
frontend reads.

## Layout

- `keybindings.go` — `Keybinding` wire shape, `Defaults`,
  `Service{Get,Update,Reset}` (atomic JSON read/write under a private
  mutex, MaxCount-capped), and the pure `Merge(defaults, user)` used
  by `Service.Get`. `New(configDir)` falls back to
  `~/.agent-overflow/keybindings.json` when configDir is empty so an
  early-boot RPC can still resolve a writable path.

## Responsibility boundary

- What BELONGS here: defaults, the merge (DefaultID / DefaultKey /
  command-context identity resolution), atomic file IO, the
  per-process serialization mutex.
- What does NOT belong here: chord-string parsing (the frontend's
  `tryParseChord` is the authoritative validator), runtime dispatch /
  reverse-lookup, command registration. Those stay in the SPA where
  they consume the merged list.

## Anti-patterns

- Do NOT silently rewrite `DefaultID` / `DefaultKey` on a user entry.
  `Merge` already canonicalises these against the matched default;
  rewriting on Update would erase the legacy-migration identity for
  configs older than the DefaultID introduction.
- Do NOT log-and-swallow a parse error in `readFile`. A malformed
  file is reported via the error path so callers can decide to fall
  back to `Defaults` (the App-side binding does exactly that for
  `Get`); silently dropping the file from a fresh install vs a
  corrupted edit would be indistinguishable.
- Do NOT change the JSON tags on `Keybinding` without a coordinated
  frontend change — the bindings tree carries a generated mirror.
