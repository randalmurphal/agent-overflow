# filepreview

Generated HTML and nearby assets, mounted only on transport's independent
preview origin. The App owns the manager; it passes the authenticated session
and transport host-presence proof. No paths are mounted on the SPA mux.

- `files.go` chooses the workspace root for an in-workspace page, otherwise
  that page's containing directory. `editor.ResolvePath` supplies the shared
  canonical path/UNC rules. `os.Root` enforces containment on every open,
  including symlinks changed after validation. Do not replace it with a
  stat/realpath prefix check.
- Refuse hidden components, traversal, backslashes, directory listings and
  non-regular files. Unix opens are nonblocking before descriptor validation:
  checking a path then opening it lets a FIFO replacement hang the handler.
- Serve GET/HEAD with `http.ServeContent`; assets stream, ranges work, and
  neither files nor rendered documents are retained in memory. Never enable
  directory listing, write methods, executable handlers or credential forwarding.
- Service-worker script requests are refused so they cannot survive a directory
  retiring and intercept a later preview on a recycled origin. Ordinary page
  scripts and workers remain usable. Browser coverage runs real scripts/assets,
  reloads and a refused `navigator.serviceWorker.register`.
- `manager.go` caps open directories at sixteen, retiring the least recently
  opened. A new directory receives a fresh gateway/grant book even if its port
  was used before. Shutdown closes gateways and root descriptors. A sharing
  policy change closes remote entries while preserving on-host pages.
- Remote previews always use TLS sources. Only authenticated host presence
  may select literal-loopback HTTP; local paired sessions still retain their
  session principal for revocation. Never let a wire argument choose locality.

Architecture and practical browser certificate limits:
[`file-previews.md`](../../docs/architecture/file-previews.md).
