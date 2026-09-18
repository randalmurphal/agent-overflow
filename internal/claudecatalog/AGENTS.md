# Claude catalogs

This package holds the process-wide Claude model and command catalogs captured
by the zero-token account probe. Account probing belongs to the application
layer; this package owns capture, cache lifetime, and model drift reporting.

Both catalogs use `provider.ProbeCacheKey`. Keep every key dimension because
the answer depends on the provider binary, account, working directory, and
environment. One initialize response fills both catalogs, so `Reset` must swap
them together under the same mutex. Do not spawn a provider from this package.

The package mutex guards only the two cache pointers. Take it to read a cache
and release it before calling that cache, whose own lock owns its entries.

`Models` also reports whether a probe answer backs the list. `Export` and
`Seed` are the model catalog's process boundary: the application layer persists
an export per account and seeds it at boot, and owns checking that the record
still describes the binary behind the key.
