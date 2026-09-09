# Codex model cache

`Cache` coalesces Codex `model/list` calls per binary and caches successful
and failed results for separate TTLs. `Peek` returns only a fresh completed
entry and never starts or joins a subprocess. `KnownModel` retains observed
capabilities across expiry and refresh failure until `Reset`.

Subprocess selection and settings reactions belong to the application layer.
This package owns cache coordination, the `Lister` seam, and defensive copies.
Keep the short error TTL. Results started before `Reset` must not populate the
new cache generation.
