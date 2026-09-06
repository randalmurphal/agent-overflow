# Computer routes

Credential-free HTTPS origins and certificate fingerprints only. This package
normalizes and bounds advertisements; it does not establish trust, dial, store
credentials or select a route. A client must verify both TLS and backend ID
before sending credentials to an alternative address. Never accept a query,
userinfo, path, malformed pin or implicit TLS downgrade.

Current advertisements take precedence for the same origin. Missing routes
remain remembered within the fixed bound, including when an older host omits
the field. The original paired endpoint is retained separately by the client.
Contract: [computer-routes.md](../../docs/architecture/computer-routes.md).

`RepairCandidates` permits a new address under an existing private pin, or a
new port under the same WebPKI hostname. A different domain with a valid public
certificate and a matching public backend ID is not proof of the old computer.
Keep the same trust boundary in frontend `transport/computerRoute.ts`.
