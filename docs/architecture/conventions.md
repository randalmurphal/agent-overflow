# Engineering conventions

Read the section relevant to the code being changed. Root instructions
cover project-wide quality requirements; area guides identify local
contracts. [Change guide](how-to.md) routes common cross-area tasks, and
[Documentation maintenance](documentation.md) governs guides and comments.

## Code organization

| Kind | Target | Ceiling |
|---|---|---|
| Go source file | 500 lines | 800 lines |
| Svelte component | 300 lines | 500 lines |
| Function or method | 80 lines | 150 lines |

When approaching a target, look for cohesive responsibilities to extract.
Do not split by line count into forwarding wrappers or fragment one operation
across unrelated files. Keep the owner, callers and tests aligned after a split.

Use names that describe actual effects. A `parse` or `build` helper should
not hide I/O; a persistence operation should make its write visible in the
API. Avoid positional arguments with indistinguishable meanings when a typed
input would prevent mistakes.

## Errors and resource ownership

Return errors with enough operation context for the caller to handle them.
Use `%w` when callers need to inspect the underlying error with `errors.Is`
or `errors.As`; otherwise expose only the intended error contract. Keep
private diagnostic details separate from client-visible messages.

Expected failures from user input, files or providers return errors.
Reserve panics for programmer errors. Continuing after a failure requires
explicit handling and an observable result, not an empty catch or ignored
return value.

Give each goroutine, listener, timer, map and cache an owner and lifetime.
Cleanup must cover success, failure, cancellation and repeated close. A
cancelled waiter must release its own registration without cancelling shared
work another caller still needs. Put non-obvious ownership contracts on the
owning API; do not add a guide entry for each resource.

## Tests

Use synchronization that observes the event being tested. Prefer channels,
`testing/synctest` or injected clocks to fixed sleeps and polling. JavaScript
timer-driven unit tests use fake timers. A timeout bounds a test; it does not
prove the operation completed. Test-only callbacks or clocks should be
introduced only when a suitable existing observation point is unavailable.

Use temporary roots and explicit fixtures. Tests must not depend on shared
machine state. Session-capable fixtures follow
[provider isolation](../../internal/kerneltest/AGENTS.md).

Test state transitions as well as steady states: repeated calls, enable then
disable, replacement during pending work, cancellation and late completion.
For a bug fix, choose assertions that fail for the original defect. For a
shared API change, cover affected callers and rejected inputs. Match the
validation environment to the claim: a DOM emulator does not establish
browser layout, scroll geometry or native webview behavior.

## SQL and persistence

Use bound parameters for values. Keep projections narrow, especially when
rows carry large payloads. Choose indexes for actual query predicates and
ordering; verify important plans rather than adding an index to every column.

Put durable integrity rules in the schema when SQLite can enforce them.
Shipped migrations are immutable. Migration tests must show that the intended
schema change took effect and preserve supported existing data. Connection,
restore and history rules belong to the
[store documentation](../../internal/store/AGENTS.md).

## Performance

Identify the work caused by an update: parsing, allocation, query count,
subprocesses, derived state and rendering. Update only affected units when
data streams. Bound queues and retained state at their owning lifetime, and
avoid reading full payloads to calculate previews.

Measure changes in the runtime where the claimed improvement matters. Keep
functional behavior and the applicable
[performance decisions](../decisions.md#performance-and-memory) intact.

## Frontend conventions

Svelte reactive state belongs in `.svelte.ts`; pure helpers use `.ts`.
Derive values in the script and keep templates focused on rendering. Follow
the [frontend guide](../../frontend/AGENTS.md) for shared components, state
ownership and styling, and [Development](development.md#generated-artifacts)
when changing generated bindings or build inputs.
