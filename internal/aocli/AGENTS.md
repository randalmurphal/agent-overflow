# internal/aocli/

Offline command routing and presentation for the `ao` binary. Keep provider,
transport, persistence, and app lifecycle concerns out of this package. Command
functions accept arguments and writers directly so tests do not spawn subprocesses.

Workflow definition behavior belongs to `internal/workflow/def`; this package
only discovers scopes, calls that API, and renders CLI results.
