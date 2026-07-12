# internal/aocli/

Offline command routing and presentation for the `ao` binary. Keep provider,
transport, persistence, and app lifecycle concerns out of this package. Command
functions accept arguments and writers directly so tests do not spawn subprocesses.

Workflow definition behavior belongs to `internal/workflow/def`; this package
discovers scopes, loads project profiles for binding checks, copies embedded
starter sources, calls the workflow APIs, and renders CLI results. Starter
content and embedding belong to `internal/workflow/starters`.
