# `internal/uikeys`

Native `WebviewWindow` key maps shared by every window constructor.

`Browser` supplies reload, fullscreen, history-navigation suppression, and
native zoom suppression. Font scaling belongs to the frontend. Use
`BrowserWithReload` when reload must restore a bootstrap-bearing URL; its
callback is evaluated on each reload so a launcher may update the URL.

`WithDevTools` is opt-in because native and WSL launcher builds have different
devtools availability. Return fresh maps so one window cannot mutate another
window's bindings.
