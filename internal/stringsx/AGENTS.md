# internal/stringsx/

Tiny string helpers shared across the codebase. Stdlib-only so every
package can depend on it without introducing a cycle.

## Layout

- `firstnonempty.go` — `FirstNonEmpty` (exact match) and
  `FirstNonEmptyTrimmed` (whitespace-stripped). Use the first for
  already-normalized inputs (JSON fields); use the second for user- or
  provider-sourced strings where leading/trailing whitespace counts as
  empty.

## Responsibility boundary

- What BELONGS here: stdlib-only string primitives that don't fit in
  `strings` but are used in more than one package.
- What does NOT belong here: anything that imports a non-stdlib
  dependency, or anything specific to one caller.

## Anti-patterns

- Do NOT import non-stdlib packages. The no-cycle guarantee depends on
  this.
- Do NOT accrete per-caller helpers. If only one package uses it, it
  belongs in that package.
