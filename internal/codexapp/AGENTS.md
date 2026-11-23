# internal/codexapp/

Owns the small application-facing Codex leaf surface: the subagent stop,
the stop timeout of background work, cached skills listing, and cached
account-wide usage reads. A command's stop and a thread's clean are sent by
the app on the session that runs the command (`app_codex_bindings.go`).

The package does not own thread sends, rollback/provider-queue transactions,
account switching, credential homes, rate limits, or session lifecycle. Its
dependencies are typed lookups for live sessions plus the exact binary,
environment, context, and active-account values needed by its fallback reads.

Wails DTO projection and shutdown policy remain at the `internal/app` binding
boundary.
