# internal/claudemodels/

Merges the model rows the Claude CLI reports into AO's hand-maintained
Claude catalog, and holds the merged result per probe identity.

The rows arrive free: the zero-token account probe's `initialize`
control_response carries a `models` array, and `claude.ProbeConfig.OnModels`
hands it here. **Nothing in this package spawns a process.** `list_models`
exists as a separate control_request and is deliberately not used: it returns
the same array for the price of a second subprocess.

## What the wire is (and is not)

Captured from claude 2.1.219 in
`docs/references/fixtures/claude/initialize_models_20260802.json`, five rows:

- It is the CLI's own **picker shortlist**: aliases (`sonnet`, `opus[1m]`), the
  `default` pointer, and canonical ids share one `value` space.
- It carries **no context windows** and **no older models** (opus-4.x and
  sonnet-4-6 are absent on 2.1.219 and still work).
- `displayName` names a **row**, not a model. "Default (recommended)" and
  "Opus (1M context)" are two rows for `claude-opus-5`.
- `[1m]` is baked into id strings inconsistently: `opus[1m]` resolves to
  `claude-opus-5[1m]`, but `claude-fable-5[1m]` resolves to a marker-less
  `claude-fable-5`.

So it is an enrichment source, not a catalog. The catalog stays hand-maintained
in `internal/provider/models.go`.

## Merge policy

In priority order (each rule earns its place from the shape above):

1. **Nothing in the catalog is dropped or reordered.** Wire ABSENCE carries no
   information (the shortlist omits still-usable models), and catalog order
   decides the fallback model for new threads.
2. **The catalog owns context windows, and a row's `displayName` never
   becomes a model name.** The wire reports no windows at all, and its
   display names name ROWS. For a model the catalog knows, the catalog's
   name stands; for a wire-only model (rule 4) the name is DERIVED FROM THE
   SLUG — `claude-fable-5-1` becomes "Claude Fable 5.1" — because the row
   name the CLI ships for it ("Fable") is not per-model and would sit in
   the picker next to the catalog's "Claude Fable 5" as a second, vaguer
   entry for what looks like the same thing.
3. **The wire owns capability flags** for the models it lists: fast-mode
   support and the reasoning-effort set. It is the running binary's answer; a
   catalog that disagrees is stale. Every override is reported as drift.
4. **Wire-only models are added**, so a model the CLI ships before we list it
   is selectable immediately. Their context windows come from the closest
   catalog FAMILY by progressive trailing-segment trim (the shape
   `internal/usagecost` uses to price an unseen model), with a two-segment
   floor: a one-segment prefix would match `claude-` and hand every unknown
   model the first catalog entry's windows. No family match means
   standard-200k only, widened to 1M only when a wire row proves the tier by
   carrying `[1m]`.
5. **Effort defaults never move up.** When the wire drops the tier the catalog
   defaulted to, the merge steps DOWN to the highest remaining tier below it.
   Silently promoting a model to a costlier tier is the one failure mode that
   spends the user's money.

## Drift, not toasts

Every disagreement and every fallback produces a `Drift` line, logged once per
DISTINCT report per probe identity (`Catalog.Store` returns nil for a repeat).
It is a maintenance signal for whoever edits the catalog. None of it is
actionable by the user, and none of it degrades the session they are starting,
so it is never a toast and never blocks anything.

`DriftDisabled` is reported and nothing else: the CLI's schema has a `disabled`
flag (an org's Zero Data Retention setting excluding a model) but no capture
has ever carried it, and hiding a working model on an unverified field is the
worse failure.

## supportsAutoMode is three-state end to end

The wire's per-model answer about `--permission-mode auto` is a `*bool`
on both `claude.WireModel` and `ModelInfo.SupportsAutoMode`, never a
`Capabilities` marker: nil means "nobody said". That third state is
load-bearing: the 2026-08-02 capture itself omits the key on the Haiku
row, and the catalog never states it, so a two-state carrier would
manufacture explicit denials for every unlisted model. The consumer
contract (pinned by the frontend AccessToggle): restrict Auto ONLY on
an explicit wire `false`; unknown behaves exactly like true, because
mis-disabling a working mode is the worse failure. The merge copies the
value on both the enrich path and the wire-only path, and reports no drift
line, because the catalog deliberately has no opinion to disagree with.

## Deliberately not consumed

- **`description` / `promoListPrice`**: prose and pricing for the CLI's own
  picker. `ModelInfo` has no field for either, and adding one is UI work.
- **`supportsAdaptiveThinking`**: no AO surface consumes it.

## CLI-version gating (t3-improvements §2.5)

AO gates the Codex CLI on a minimum version and deliberately does not gate
Claude (`internal/provider/detect.go`). §2.5 proposed adding hand-maintained
PER-MODEL minimums so an old CLI could not offer a model it would reject at
spawn. **That is not being built, and this package is why:** the running
binary's own model list is a better answer than a version table maintained by
hand. A model the CLI lists is a model the CLI has.

What that does NOT license is the inverse. Wire absence is ambiguous (older
models are absent from a shortlist that still runs them), so the catalog keeps
listing them and no gate is derived from absence. The existing whole-provider
Codex gate is untouched; it guards protocol features, not model availability.

## Identity

`Catalog` is keyed by `provider.ProbeCacheKey`, the same key the account probe
memoizes under, so a model list can never outlive the binary, account, workdir,
or custom environment that produced it, and one environment's answer can never
be served to another.

It deliberately does NOT share the probe cache's TTL or its invalidations. A
model list has no correctness deadline; dropping it on a recheck would make
wire-only models vanish from an open picker for the seconds a re-probe takes.
Every probe replaces the entry wholesale, and the map is capped instead.

## Anti-patterns

- Do NOT let the wire subtract. Removing a model, or treating a miss as
  authoritative, breaks the one hard requirement: no working model disappears
  from the picker.
- Do NOT spawn anything from here. If a caller wants fresher models, it probes.
- Do NOT surface drift to the user. It is a note to the maintainer.
- Do NOT infer capability from an alias string. `[1m]` presence is evidence;
  its absence is not, and no other id substring means anything.
