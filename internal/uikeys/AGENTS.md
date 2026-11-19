# `internal/uikeys`

Native `WebviewWindow` key maps shared by every window constructor.

`Browser` supplies reload, fullscreen, history-navigation suppression, and
native zoom suppression. Font scaling belongs to the frontend. Use
`BrowserWithReload` when reload must restore a bootstrap-bearing URL; its
callback is evaluated on each reload so a launcher may update the URL.

`WithDevTools` is opt-in because native and WSL launcher builds have different
devtools availability. Return fresh maps so one window cannot mutate another
window's bindings.

`PageChords` routes the reload chord to the SPA while the SPA asks for it
(its browser pane is focused and its effective bindings run the chord). A
window that hosts the native browser pane wraps its bindings
with it, feeds it raw messages only from its own page's origin
(`TrustedPage`), and calls `DocumentLoaded` on each runtime-ready event.
Raw messages arrive on a goroutine each, so each report names its
document's number and its own; only the current document's newest
report counts.
