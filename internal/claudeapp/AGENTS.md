# internal/claudeapp/

Owns application-facing Claude controls that operate on an already-running
session and the filesystem-backed skills listing. Session creation, account
probing, credential rotation, live configuration, and rate limits remain in
their existing owners.

The dependency seam is deliberately typed and small: resolve a live Claude
session, or resolve the Claude configuration store. Wails DTO projection and
shutdown policy stay at the `internal/app` binding boundary.
