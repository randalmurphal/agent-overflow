# Wails window integration

This package joins GUI-free window geometry, Wails window creation and events,
page-ticket delivery, and size-preserving reveal behavior. It is imported only
by GUI binaries.

Create app-shell windows through New. It disables unused automatic Wails event
forwarding while retaining native hooks and explicit ExecJS delivery.
RestoreAndTrack runs from ApplicationStarted, after Wails can materialize the
window synchronously. Restore normal bounds before deferred maximize or
fullscreen actions to avoid visible flashes and wrong-monitor placement.

Reveal calls Show, conditionally UnMinimise, and Focus. Do not call Restore,
which also exits maximized or fullscreen state.

DeliverPageTicket subscribes to WindowRuntimeReady, mints a fresh one-time
ticket for every document announcement, and injects pagehost.DeliveryScript.
Keep credentials out of navigation URLs. Re-announcements cover subscription
and reload races; repeated delivery of a spent ticket is harmless only after
the document already has its authenticated cookie.

All Wails geometry is in device-independent pixels. Keep placement and tracking
math in internal/windowgeom, persistence in caller-provided sinks, and ticket
vocabulary in internal/pagehost. Flush the tracker's in-memory state on close
instead of reading a window during teardown.

The no-GUI WSL backend must not import this package. Tests retain the guard
against direct zero-argument Window.Restore calls in Wails code.
