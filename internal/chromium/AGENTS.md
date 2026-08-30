# internal/chromium/

Managed Chrome-for-Testing artifact installation shared by the general browser
and the design screenshot renderer.

- Keep downloads HTTPS-only, bounded, zip-slip/symlink safe, and cached by
  validated version segment.
- `ArtifactChrome` and `ArtifactHeadlessShell` may share manifest resolution,
  but have separate cache directories and executable layouts.
- This package never launches a browser process and never owns browsing state.
- `Installer.BinaryPath` is an explicit executable override used by the E2E
  harness to reuse Playwright Chromium; validate it as executable and never
  silently fall back to a download when it is invalid.
- Tests use loopback fixture archives with `AllowInsecureScheme`; production
  must never set it.
