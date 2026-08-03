# internal/codexusage/

TTL'd, single-flighted cache around Codex's account-level token-usage
report (`account/usage/read`). Backs `GetCodexAccountUsage` and the
usage overlay's Codex-account section.

The read is never free: the app-server forwards it to the ChatGPT
backend, and when no Codex session is live the App has to start a whole
short-lived `codex app-server` process to ask. Without coalescing, every
render of the overlay section would pay that.

## Layout

- `codexusage.go` — `Cache` type, `New` / `NewWith` constructors,
  `Get` / `Invalidate`, the `Fetch` callback type, and the `cloneUsage`
  / `cloneInt64` defensive-copy helpers. `DefaultTTL` (5 min) and
  `DefaultErrorTTL` (30 s) are the only exported constants.

## Responsibility boundary

- What BELONGS here:
  - TTL + single-flight bookkeeping (entries + inflight maps under one
    mutex).
  - Deep defensive cloning, so a caller cannot mutate the cached entry
    through either the bucket slice or the optional summary pointers.
- What does NOT belong here:
  - Deciding HOW to read. The App supplies a `Fetch` closure because
    only it knows whether a live session's connection can be ridden
    (`app_codex_usage.go#readCodexAccountUsage`).
  - The wire shape and its parsing — `internal/provider/codex/account_usage.go`.
  - `*App` state or frontend-facing types.

## Two rules that are not stylistic

- **Errors are cached, and shared.** Unlike `internal/codexmodels`, a
  failed lookup populates an entry for `DefaultErrorTTL` and every
  concurrent caller receives the same error. This is what keeps a
  failure from becoming an empty report: the losers of a race must not
  see `AccountUsage{}` with a nil error, and a burst of renders must not
  spawn a burst of subprocesses re-learning the same failure. The short
  error TTL is what keeps recovery (and a binary upgrade that adds the
  method) from being masked.
- **The key carries the account, not just the binary.** Dropping the
  account dimension would serve one login's lifetime totals under
  another login's name after a switch. The App builds the key as
  `binary + "\x00" + accountID`.

## Anti-patterns

- Do NOT convert a cached error into an empty report here. "Nothing to
  report" is a decision about the CONTENT of a successful read, and it
  is made one layer up (`GetCodexAccountUsage` maps
  `codex.ErrAccountUsageUnavailable` and an empty report to a nil
  result); this package must not blur the two.
