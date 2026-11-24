# Codex model cache

`Cache` holds one Codex `model/list` result per binary path and caches
successful and failed results for separate TTLs. The entry records the
`Launch` (working directory and environment digest) it was read with; a read
with another launch replaces it, remembered capabilities included. `Peek`
returns only a fresh completed entry and never starts or joins a subprocess.
`KnownModel` retains observed capabilities across expiry and refresh failure
until `Reset` or another launch.

Subprocess selection and settings reactions belong to the application layer.
This package owns cache coordination, the `Lister` seam, and defensive copies.
A shared lookup belongs to no caller: it runs without the first caller's
cancellation, under the lister's own bound, and each caller waits on its own
context. Keep the short error TTL. Results started before `Reset` must not populate the
new cache generation.
