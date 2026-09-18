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

Every model answer carries its `provider.CatalogProvenance`, so a client can
tell the shipped placeholder from an answer the installed binary gave. A Claude
probe's committed catalog is exported to `Deps.RememberClaudeCatalog` in
`AfterAdopt`, filed under the adopted account; a nil dep is a supported wiring.

Tests must inject probe functions. Never spawn a real provider binary or read a
real provider home from this package.

Transfer acceptance checks the selected binary and a fresh provider-specific
account probe before destination preparation. The five-minute identity cache is
for display, never proof that a move can retire its source. Claude's probe uses
the account manager's canonical credential/rotation transaction; Codex reads
`account/read` without refreshing tokens, usage, threads or model calls and
accepts a custom endpoint that explicitly requires no OpenAI authentication.
