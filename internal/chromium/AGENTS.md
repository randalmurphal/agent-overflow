# internal/chromium/

Managed Chrome-for-Testing artifact installation shared by the general browser
and the design screenshot renderer.

- Keep downloads HTTPS-only, bounded, zip-slip/symlink safe, and cached by
  validated version segment.
- `ArtifactChrome` and `ArtifactHeadlessShell` may share manifest resolution,
  but have separate cache directories and executable layouts.
- This package never launches a browser process and never owns browsing state.
- Tests use loopback fixture archives with `AllowInsecureScheme`; production
  must never set it.
