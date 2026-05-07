# internal/platform

Small runtime-environment probes shared by packages that need host-specific
behavior.

## Ownership

- Keep this package narrow. It is for facts about the current process runtime,
  not for policy decisions or launch behavior.
- Prefer pure helpers with injected OS reads for tests. Cache only values that
  cannot change during the process lifetime.
- Do not import higher-level packages from here. Platform probes sit below
  editor, browser-opening, launcher, and transport code.

## Testing

- Tests should inject filesystem/env readers rather than depending on the
  developer machine.
- When adding a probe, cover false-on-read-error behavior explicitly.
