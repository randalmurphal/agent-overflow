# Browser pane keyboard and focus

How AO's chords and keyboard focus work while a browser pane shows a
native page. The pane's chords and the product contract are in
[browser-tools.md](browser-tools.md#the-panes-chrome); the find bar's use of
Escape is in [pane find](pane-find.md#browser-pane-find).

The page is a native view beside the SPA's, and the window has one keyboard
focus. Once the user clicks into page content every key goes to the engine,
so AO's chords have to be taken back at the engine.

## Chords in the page

- The App reduces the effective keybindings to an `AcceleratorSet`
  (`internal/keybindings/accelerator.go`, the Go mirror of the frontend chord
  grammar with `mod` resolved per platform) at startup and after every
  keybindings write. Only chords with ctrl, meta, or alt are in it: bare and
  shift-only keys are typing and always reach the page.
- Each engine gates key events on it before its document sees them: the
  WKWebView subclass's `performKeyEquivalent:`/`keyDown:` (only while the page
  view is first responder), a capture-phase `GtkEventControllerKey` on the
  WebKitGTK view, and WebView2's `AcceleratorKeyPressed` in the launcher, which
  matches in its own process against the set the backend ships in an
  `accelerators` directive and answers with an `accelerator` report.
- A bound chord is swallowed by the engine and hands keyboard focus to the
  app's webview inside the key event, before the next key and before the
  chord is routed: the Manager does it for every chord it claims
  (`keyChord`, through `engineAppFocus` on WebKitGTK and WKWebView), and the
  launcher does it where it claims for WebView2 (`acceleratorPressed`,
  focusing its window, whose WebView2 controller takes the focus), only
  while that window is in the foreground. A chord AO does not claim, and a
  bare Escape, leave focus in the page. No command focuses anything for
  this; the command puts DOM focus where it wants it (the find bar's input,
  the palette), and the keys typed next reach it.

## Replay in the app

The chord reaches the frontend as an `accelerator` event on
`browser:companion-state`, carrying the bound spelling (Shift glyph variants
are normalized by the match) and the presenting mount (`mount`). Every host
client receives the event, and only the one holding that mount replays it;
a chord in a page no pane shows is dropped. The client
(`browserPageChords.ts`) acts in that mount's pane or nowhere: when a pane
promoted over it, or a compact screen that no longer shows it, means the
keyboard's commands would not reach it, the chord is dropped rather than
resolved against another pane. Otherwise it focuses the thread's browser
companion and replays the chord as a window keydown, so `when` gates,
rebinds and the palette all behave as if the SPA had been focused. A bound
chord whose `when` fails there is a no-op: the page never gets it back,
which is the price of answering synchronously. The replay leaves DOM focus
where the SPA last had it, so a surface the chord opens returns focus there
when it closes: focus stays in the app, and the page gets it back from a
click.

## The app reload chord

The desktop and Windows launcher windows bind `mod+r` to reloading the app
before the SPA sees the key. While the browser pane is focused and the
effective keybindings run a command for `mod+r` there, the SPA asks over the
webview's raw message channel for the chord. The window then hands it over,
and the SPA replays it as a keydown through the keybinding dispatcher, as it
does a chord claimed in the page. The chord is asked for whether or not the
pane has attached, and one the pane still reserves does nothing when nothing
runs it; a chord handed over after the pane stopped reserving it goes back
and reloads the app (`internal/uikeys/pagechords.go`,
`frontend/src/lib/native/shellChords.svelte.ts`). `mod+shift+r` always
reloads the app. The `--connect` window hosts no native page and keeps the
plain binding.

## Escape

A bare Escape is never AO's in the page: WebKitGTK and WKWebView offer it to
the gate, which always lets it through, and while a pane shows the page the
Manager announces each press as an `escape` event (thread, page id and the
presenting mount) on `browser:companion-state` for the pane's find, which
closes only that client's find bar. A key held down is not announced
(WKWebView's `isARepeat`; on GTK 4, which reports no repeat, a press while
the view has not seen its release or lost focus), nor is a press during an
input method composition (the view's preedit on GTK, the responder's
`hasMarkedText` on macOS). The native code only gathers those facts;
`pageEscapeAnnounced` decides. An Escape with a modifier is an ordinary
chord.

## Hidden pages

A page that is not visible never holds keyboard focus. Each way a pane stops
showing a page (a tab switch, the pane closing, a compact screen change, the
companion hidden) is the engine's hide of that page, and the hide, like the
close of a page, gives focus the page held to the app's webview through the
same code as a claimed chord's handoff (`ao_wk_focus_app`,
`ao_wkv_focus_app`, and the launcher's `releaseFocus`). Showing a page takes
no focus.

## Focus intents

The pane hands the keyboard to the page the way a browser does after the
address bar's Enter or Escape, as a focus intent: a transient request that
one page take the keyboard, named by the pane mount and a number per mount
(`focus_intent.go`, `focusBrowserPage` in the frontend store, host window
only). Being presented, visible or in the foreground makes a page eligible;
none of them overrides the user's next focus choice.

- The store withdraws the intent on a focus or pointer press outside the
  page area (the composer, the address bar again), a newer deferred focus
  anywhere in the window (`utils/focusIntent.ts`, which a terminal's focus
  once it mounts also registers with), another active tab, or the pane
  unmounting, whatever the arm's answer was and however long it took. It
  posts the withdrawal to the Windows launcher directly (`ao:pane-focus:`,
  beside the pane geometry) and sends `focus-page-withdraw`.
- The `focus-page` action arms it in `Manager.FocusCompanionPage`, which
  applies it only while the mount presents the named page, waiting through
  a restore or a layout until the presentation pass shows it
  (`applyFocusIntent`, never `ShowPage`), and drops it once withdrawn,
  superseded, its tab closed or ten seconds old (`FocusIntentMaxAge`).
- The engine checks the page is shown and the intent live where its hides
  run, immediately before moving focus: `ao_wk_view_focus_presented` grabs
  GTK focus on the WebKitGTK view, `ao_wkv_view_focus_presented` makes the
  WKWebView first responder, and the launcher applies a `focus` directive
  with `ICoreWebView2Controller::MoveFocus` (programmatic) only while it
  shows the page, its window is in the foreground and it has not heard the
  intent withdrawn or replaced (`focusDecision`, `FocusIntents`), so AO
  never takes the OS focus from another application. A directive for a page
  the launcher's own presentation hides waits and is applied once at the
  page's next show in the directive's mount, under the same checks and the
  same age bound; an applied intent delivered again moves nothing.
- Nothing undoes a focus that landed, so one can still land in the instant
  between the user's choice elsewhere and the withdrawal reaching where
  focus is applied: one loopback RPC from the SPA to the backend on Linux
  and macOS, one WebView2 web message from the SPA to the launcher on
  Windows.
- The outcome reaches the pane as a Kind `focus` event naming the intent; a
  refusal (the launcher's `focus-failed` report) takes the pane's action
  banner unless the intent is no longer the latest or a newer action's
  banner is up.

## The presenting mount

Only the host's own window mounts a pane (`BrowserCompanionPaneAttach`
refuses any other connection; the host window is defined in
[transport.md](transport.md#page-bootstrap)): the native view lives in that
window. A browser tab on the host offers and opens no pane, as a connected
browser does. Both openers, a state push (which the header button's show and
new tab produce) and the reconcile on a thread mount, go through
`openBrowserCompanion`, which asks `canUseNativeBrowser`; a mod-clicked link
asks it before minting a page, and no layout restore or companion stash
reopens a browser pane. It holds only while the thread's owner is the home
computer and no other computer claims the same ownership epoch
(`resolveThreadBackend`), and a user action asks it again as it is sent,
after any wait. A thread's pages are presented in one mount, its earliest
attached live one (`presenterLocked`), until that mount detaches; the
presentation, the pages' size, and the routed chord and Escape all name it.
