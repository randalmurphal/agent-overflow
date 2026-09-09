# `internal/compare`

Offline capsule preparation and paired A/B execution for `ao-harness compare`. It must not import the app or harness server.

- Refuse live app roots, aliases of the live database, locked sources, and overlapping output paths.
- Publish immutable capsules by rename and verify the manifest digest, every asset hash, and path containment before use.
- Materialize disposable roots without rewriting database content or identifiers.
- Delete a disposable root only after rechecking its identity token and `os.SameFile` relationship.
- A pair is valid only when both legs agree on assets, build identity, metric keys, finite values, and semantic text.
- Report paired deltas below `BootstrapMinPairs`; do not claim a confidence interval until the minimum complete-pair count is met.
- A missing browser runner is an explicit failure.

Operator-selected live-state and browser-profile capture exceptions are documented at the `ao-harness` call site and must remain explicit inputs.
