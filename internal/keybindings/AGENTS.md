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

## The load-error contract

`Service.Get` returns a `LoadResult`, not `([]Keybinding, error)`:

```go
type LoadResult struct {
    Bindings  []Keybinding `json:"bindings"`
    LoadError string       `json:"loadError,omitempty"`
}
```

- `Bindings` is ALWAYS usable — `Defaults` merged with whatever
  overrides were readable. A missing file is not a failure (fresh
  install); an unreadable or malformed one falls back to `Defaults`.
- `LoadError` is non-empty only when the file existed and could not be
  read or parsed.

Why not an error pair: both halves are non-nil together in exactly the
case that matters, and that shape invites the reflexive
`if err != nil { return }` that discards a perfectly good binding list.
The failure is DATA here — it crosses the wire beside the bindings and
the frontend renders it (banner in Keybindings settings + a toast).
That matters because `Update` overwrites the user file wholesale: a
silently-swallowed read failure means the next edit in Settings
destroys the user's overrides with no warning. Reporting it is the
informed consent for that overwrite.

Consequences to keep in sync:

- `App.GetKeybindings` returns `(keybindings.LoadResult, error)` and
  its error covers service CONSTRUCTION only.
- The frontend mirror is
  `frontend/src/lib/stores/keybindings.svelte.ts`
  (`getKeybindingLoadError`), rendered by
  `components/settings/KeybindingsSettings.svelte`.
- Per-entry validation stays out of `LoadError`. A row with a broken
  chord is a frontend configuration issue (see the responsibility
  boundary below); `LoadError` answers only "was the file readable".

## The unbound sentinel

`Keybinding.Key == Unbound` (the empty string) is the persisted
representation of "this command is deliberately bound to nothing". It
is distinct from the entry being ABSENT:

| user file state | meaning |
|---|---|
| no entry for the default row | use the shipped chord |
| entry with a non-empty `key` | use the user's chord instead |
| entry with `key: ""` | suppress the shipped chord; nothing dispatches |

Why the empty string rather than a `unbound bool` field or a JSON
null: `key` already answers "which chord runs this command", so "no
chord" is a value of that field. A second field could disagree with
it, and a null would need `*string` on the wire plus a matching
frontend type for no extra expressiveness. `keybindings.Unbound` /
`IsUnbound` name it on the Go side; `UNBOUND_CHORD` / `isUnboundChord`
in `frontend/src/lib/stores/keybindings.svelte.ts` mirror them.

Rules that hold across the package:

- Keys are trimmed by `Update` and by `Merge`, so a whitespace-only
  key from a hand-edited file collapses onto the canonical sentinel
  instead of reaching the frontend as an unparseable chord.
- `Update` accepts the sentinel only when the entry names the default
  row it clears (`DefaultID` or `DefaultKey`). An empty key with no
  identity is a caller that dropped the chord, not a user who cleared
  one.
- `Merge` treats an unbound entry exactly like a rebind — it replaces
  its default row — except that an unbound entry matching NO default
  is dropped rather than appended. It silences nothing, and keeping it
  would render a chordless row nothing can restore.
- `Defaults` never contains the sentinel
  (`TestDefaultsUseValidChordSyntax` pins that), so every unbound row
  has a default chord to restore.

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
- Do NOT log-and-swallow a parse error in `readFile`. `Get` turns it
  into `LoadResult.LoadError` (see the load-error contract above);
  silently dropping the file would make a fresh install and a
  corrupted edit indistinguishable — to the code AND to the user
  whose next save overwrites it.
- Do NOT change the JSON tags on `Keybinding` without a coordinated
  frontend change — the bindings tree carries a generated mirror.
