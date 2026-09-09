# Claude catalogs

This package holds the process-wide Claude model and command catalogs captured
by the zero-token account probe. Account probing belongs to the application
layer; this package owns capture, cache lifetime, and model drift reporting.

Both catalogs use `provider.ProbeCacheKey`. Keep every key dimension because
the answer depends on the provider binary, account, working directory, and
environment. One initialize response fills both catalogs, so `Reset` must swap
them together under the same mutex. Do not spawn a provider from this package.
