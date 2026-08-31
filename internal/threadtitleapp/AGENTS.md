# internal/threadtitleapp/

Application coordination for automatic and user-triggered thread-title generation.

The service owns the per-thread in-flight claim, auto/heal/regeneration policy,
prompt/context and image-path gathering, result sanitization, and the
compare-and-swap persistence. `main` injects the provider generator and projects
the two callbacks onto Wails events and Claude peer-session naming.

The generator is the only process-capable boundary. Package tests must inject a
fake and must never resolve or spawn a provider binary.
