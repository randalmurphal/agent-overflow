# Appearance asset watchers

This package watches flat theme, spinner, sound, prompt, and installation-name
directories. The private core owns fsnotify, trailing-edge debounce, directory
re-arming, and self-write suppression; concept-specific types own filename
policy.

Do not expose watcher internals or block the event loop on consumers. Directory
removal, recreation, and atomic replacement must continue to re-arm correctly.
Theme and sound suppression is a bounded window around app-owned writes. The
prompt watcher does not suppress: the app writes a prompt file only to create
or delete an entry, and the reload that echo causes changes nothing. The
device-name watcher does not suppress its own write because its debounced
event propagates to peers.

Application lifecycle, event channels, and degraded-startup reporting remain in
internal/app.
