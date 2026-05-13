# internal/errorsx/

Stdlib-only error-aggregation and wrapping helpers shared across the
codebase. Two functions today; the package exists so the app layer
doesn't carry these primitives inline.

## Surface

| Symbol | Purpose |
|---|---|
| `Append(errs, err) []error` | Append `err` to `errs` when non-nil; pass through unchanged otherwise. Used by lifecycle teardown loops that collect independent failures for a final `errors.Join`. |
| `WrapLifecycle(action, cause) error` | Returns nil for nil cause; otherwise `fmt.Errorf("%s: %w", action, cause)`. Used to label step failures during App startup/shutdown so toasts read as "close store after logger init failure: <cause>" rather than the raw inner error. |

## Responsibility boundary

- What BELONGS here: tiny error-shape helpers that are useful in more
  than one package and don't need any non-stdlib dependency.
- What does NOT belong here: domain-specific error wrapping (provider
  errors live in `internal/provider/`, store errors stay in
  `internal/store/`), retry helpers (those need timing primitives and
  belong with the caller).

## Anti-patterns

- Do NOT import non-stdlib packages. The no-cycle guarantee depends
  on this.
- Do NOT accrete per-caller helpers. If only one package uses it, it
  belongs in that package.
