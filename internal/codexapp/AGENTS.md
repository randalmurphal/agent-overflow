# internal/codexapp/

Owns the small application-facing Codex leaf surface: background-terminal
controls, cached skills listing, and cached account-wide usage reads.

The package does not own thread sends, rollback/provider-queue transactions,
account switching, credential homes, rate limits, or session lifecycle. Its
dependencies are typed lookups for live sessions plus the exact binary,
environment, context, and active-account values needed by its fallback reads.

Wails DTO projection and shutdown policy remain at the `internal/app` binding
boundary.
