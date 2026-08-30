# internal/compare

The offline capsule and A/B engine behind `ao-harness compare`. `Prepare`
freezes a data root into an immutable capsule. `Run` replays that capsule
into disposable roots, alternating legs, and reports paired deltas. It
imports neither the app nor the harness server.

## Invariants

- **A missing browser driver is a visible failure, never a fake result.**
  The caller supplies the `Runner`; the default `UnavailableRunner`
  errors. Do not add a synthetic fallback leg.
- **Prepare never reads the real app's live data.** It refuses a source
  at or under the `internal/appdirs` root or its config parent, refuses a
  source database that `os.SameFile` says IS the real one (a hard link is
  an alias with no path component to resolve), and refuses an offline
  copy still carrying `harness.lock` rather than guessing it is stale.
  `Run` applies the same overlap refusal to its base directory and its
  report path.
- **A capsule is immutable once published.** Publication is a rename
  after validation, and a failed rename restores the previous tree.
  `Load` verifies the manifest digest and every content hash before a run
  may touch a disposable target. An asset path with a symlinked component
  is refused before hashing, because a linked parent would read arbitrary
  host files.
- **Materializing changes nothing about the restored database.** No seed,
  no rewrite, no identifier remap. Both legs consume the same rows under
  the same ids, or the comparison is measuring the fixture.
- **A disposable root is deleted only against its own identity.** Each
  carries a `.compare-root-identity` token, and removal re-checks that
  token plus `os.SameFile` before it recurses.
- **A pair whose legs disagree is invalid, not a delta.** Both legs must
  report asset and build digests, agree on them, agree on the metric key
  SET, carry only finite values, and produce identical semantic text. Any
  mismatch marks BOTH legs invalid.
- **Below `BootstrapMinPairs` (8) complete pairs there is no confidence
  interval.** Paired deltas still print. Smaller samples do not
  manufacture confidence.
- **`Instrument` is `perf` or `none` only.** A leg measuring memory or
  correctness must not pay the sampler's cost.

## Footguns

The bootstrap RNG is seeded from the capsule digest, so an interval is
reproducible across runs of the same capsule. The report is rewritten
after every leg, so a crash keeps the pairs that finished. Semantic text
comes from the harness bridge, never from CDP page evaluation, which only
proves page ownership. Launch memory limits come from
[internal/harness/governor](../harness/governor/AGENTS.md).
