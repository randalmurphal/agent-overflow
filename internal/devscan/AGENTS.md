# `internal/devscan`

Discovers local development servers for preview routing. It combines attributed listening processes, configured ports, and recent successful observations without pretending those sources have equal confidence.

- Attribute ports from process and socket facts, never executable names alone.
- A listening socket is a candidate. Probe attributed and otherwise-seen ports
  for a document response before presenting them. A hand-allowed port remains
  visible by owner choice, but still gets a bounded scheme probe.
- Keep configured ports visible even while closed, and retain bounded recent observations so transient restarts do not reorder the UI.
- Preserve source and status in results; callers need to distinguish discovery, configuration, and observation.
- Keep enumeration platform-specific. Unsupported platforms return a clear capability result.
- Bound scan concurrency, probe time, response reads, result count, and retained observations.

The package discovers and probes only. Authentication, remote routing, settings persistence, and browser launch belong elsewhere.
