# internal/stringsx/

Tiny string helpers shared across the codebase. Stdlib-only so every
package can depend on it without introducing a cycle.

## Layout

- `firstnonempty.go` — `FirstNonEmpty` (exact match) and
  `FirstNonEmptyTrimmed` (whitespace-stripped). Use the first for
  already-normalized inputs (JSON fields); use the second for user- or
  provider-sourced strings where leading/trailing whitespace counts as
  empty.
- `clip.go` — `Clip(s, max)` truncates by bytes for hard UI-field
  ceilings (diagnostics payloads, log redaction). Returns `""` for
  non-positive `max`.
- `joinnonempty.go` — `JoinNonEmpty(sep, parts...)` trims each part and
  joins the non-blank survivors with `sep`. Used for composing system
  prompts where any section may be empty.
- `ansi.go` — `SkipANSIEscape(s, i)` returns the index just past the
  ANSI/OSC escape at `s[i]` (CSI / OSC / charset / bare ESC; an
  unterminated sequence resumes past the ESC instead of swallowing the
  tail). Generic over `[]byte | string` so the claudetui PTY scan and the
  triage command-line de-ANSI share one escape skipper.

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
