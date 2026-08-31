# internal/claudecatalog/

Owns the process-wide Claude model and slash-command answers captured from the
zero-token account probe. Both catalogs are keyed by `provider.ProbeCacheKey`
and reset together because one initialize response fills them together.

Provider account probing remains in the application layer. This package owns
only capture semantics, cache lifetime, and model-catalog drift reporting.

The process-global state is deliberate: both underlying caches are bounded by
probe identity, and their answer depends on the provider binary, account,
workdir, and environment rather than on an `App` instance. `Reset` exists for
tests and swaps both catalogs under the same mutex.
