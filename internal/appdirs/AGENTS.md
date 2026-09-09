# Application directories

This package provides the shared fallback for the app-managed directory root.
Boot-time settings and the offline CLI must resolve through it so they agree
with the application.

Keep flags and overrides out of this package. Callers own explicit data-root
semantics and the policy for resolution failure.
