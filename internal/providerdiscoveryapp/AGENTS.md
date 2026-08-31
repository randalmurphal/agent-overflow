# internal/providerdiscoveryapp

Owns bounded provider identity/model caches and application coordination for
provider discovery: separate Claude and Codex zero-token probes, provider
binary status detection, probe-enriched Claude catalogs, live Codex models,
and custom-environment cache invalidation.

Managed-account credential stability, adoption, and rotation remain in
`provideraccountapp` behind the injected account-probe runner. Session start,
send, revert, review, provider events, and rate-limit persistence remain with
their existing root/session owners. Do not merge Claude and Codex wire
transactions into one generic probe path; their request and side-effect shapes
are intentionally separate.

Tests must inject probe functions. Never spawn a real provider binary or read a
real provider home from this package.
