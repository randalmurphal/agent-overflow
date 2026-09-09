# Workflow project profiles

This package parses and validates project workflow profiles and resolves their
secret bindings. It does not execute processes, schedule work, persist runs, or
depend on the workflow engine.

- Loading never resolves secrets. Callers explicitly use `ResolveSecrets` only
  at the process boundary that needs them.
- Secret values must not appear in errors, logs, formatted profiles, or test
  diagnostics.
- `ResolvedSecrets` owns both process-environment construction and cleanup.
  Preserve replacement and unset behavior across repeated resolution.
- Binding names use `[a-z0-9-]+`. Capacity names additionally permit the
  provider-prefixed namespace accepted by validation.
- `max_fan_out_width` is an admission ceiling, not a resource capacity.
- The legacy `worktree_setup` field remains decode-only for compatibility. Do
  not restore execution or advertise it in schemas and examples.
- Keep validation findings aggregated and field-addressable for editor and CLI
  consumers.
