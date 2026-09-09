# Appearance asset watchers

This package watches flat theme, spinner, and installation-name directories.
The private core owns fsnotify, trailing-edge debounce, directory re-arming, and
self-write suppression; concept-specific types own filename policy.

Do not expose watcher internals or block the event loop on consumers. Directory
removal, recreation, and atomic replacement must continue to re-arm correctly.
Theme suppression is a bounded window around app-owned writes. The device-name
watcher does not suppress its own write because its debounced event propagates
to peers.

Application lifecycle, event channels, and degraded-startup reporting remain in
internal/app.
