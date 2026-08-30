# components/composer/

## The surface is shared, the host is not

`ComposerInputSurface.svelte` is the editing core: the textarea plus
everything whose job is getting text and attachments into a draft
(completion menus, image placeholders, uploads, terminal chips, command
highlight). It owns none of the send decision, the thread lifecycle, or
the pending prompt panels, which stay with the host. Both
`Composer.svelte` and chat's `UserMessageEditor.svelte` host it, so a new
place that edits a message extends the surface rather than growing a
parallel one. `composerInputSurface.ts` holds the prop and handle types so
a host can name what it holds without importing the component's chunk.

`Composer.svelte` stays a shell. The send and interrupt flow lives in
`composerSend.ts`, deliberately holding nothing reactive: each call
captures the current thread id and draft snapshot, then delegates back to
the pane and draft store. Drag, drop, paste and upload live in
`composerUploads.svelte.ts`, which carries a per-thread guard so a slow
upload cannot land in the wrong pane.

## Rail visibility is one predicate

`activityRailHost.svelte.ts` owns the background-tasks controller, the
shared 1Hz clock, and the rail's visibility predicate. Visibility is
HOST-owned because it is load-bearing geometry: the composer mounts the
rail if and only if `railVisible`, and renders a transparent
height-reservation spacer as the exact complement, so exactly one of the
two holds the row at all times. That is what keeps the composer's measured
height, and the timeline padding it drives through `--composer-height`,
constant across turn start, turn completion and background-task end, so
the last message never jumps. Both branches flip in the same reactive
flush, so there is no one-frame double-height blip.

Do not re-derive "is the rail showing" anywhere else. The spacer once used
its own predicate without the background term and stacked a phantom second
row whenever a background task outlived its turn.

Call `createActivityRailHost` from component init (the clock uses runes),
`mount()` from `onMount`, and dispose its return value in `onDestroy`.

## The working indicator is stepped, not animated

`WorkingSprite.svelte` translates a horizontal strip PNG inside a
one-frame clipping window, stepped once per frame, with no timer and no
lifecycle JS. `transform` is compositable, so Blink runs it off the main
thread. The previous inline `background-position-x` write from a
wall-clock timer was the single most expensive thing in the renderer:
163.0ms of main-thread work per 5s at 25 frames/s, against 0.0ms now
(2026-08-23). Layer-promoting the old write still cost 95.4ms. Phase comes
from `utils/ambientPhase.ts`, so a remount lands mid-cycle on the same
beat every other ambient indicator shares. Any new indicator here follows
the same shape.
