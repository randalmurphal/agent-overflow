# Logging setup

This package writes bounded structured NDJSON logs for provider I/O and workflow
engine events. Callers choose the base directory and whether an optional logger
is enabled.

Create private directories and files and keep size-based rotation bounded.
Preserve errors from setup and writes. `LogProviderEvent` deliberately records
raw provider payloads, compacting valid JSON and quoting invalid JSON so one
event remains one NDJSON line. Callers must not pass credentials or unrelated
secrets as metadata.

`NewProviderEventLogger` is enabled only by the `provider` or `all` debug
topic. `NewEngineEventLogger` is always on. `PruneOlderThan` removes eligible
daily log files and leaves unrelated files alone. Tests use temporary roots.

Every shell installs `Output` as the process log output. It retains the most
recent lines in a fixed byte budget so an error report can include what led up
to a failure (`RecentLogLinesBefore`); retained lines carry whatever the log
does, so they are read only by host-scoped callers.
