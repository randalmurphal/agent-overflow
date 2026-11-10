# components/composer/

`ComposerInputSurface.svelte` is the shared editing core used by the thread
composer and `chat/UserMessageEditor.svelte`. Hosts own send behavior and
surrounding chrome; the shared surface owns text editing, selection, attachments,
mentions, slash commands, and keyboard behavior.

## Shared surface

- Add editing behavior to `ComposerInputSurface`, not separately to its hosts.
- Keep host callbacks explicit. Message editing must not inherit thread-send
  effects such as queueing, model selection, or follow changes.
- Preserve selection and composition behavior across controlled-value updates.
  Test IME and multiline edits when changing input events.
- Mention and slash menus are caret-owned popovers anchored on the input in
  both layouts.

## Attachments and sending

An attachment is either a persisted backend attachment or a pending local
upload. Keep the distinction explicit until upload succeeds. Removing,
retrying, or changing computer must invalidate stale upload completions.

Build every send with `utils/sendOptions.ts#buildSendOptions`. Create the
idempotency identity once and reuse it across uncertain transport outcomes.
Connection loss does not prove rejection, so keep an uncertain send retryable
with the same identity.

All send entry points use the same readiness predicate. Keyboard submit, the
Send button, queued sends, and accessibility actions must agree about empty
content, pending uploads, provider readiness, and active operations. Compact
Return inserts a newline; the Send button submits.

## Shell mode

A draft whose first character is `!`, and that holds no command output, is
a shell command: Send runs it through `stores/shellCommands.svelte.ts`
instead of sending it, and only the text is consumed. The composer decides
the mode and passes `shellPrefix` to the surface, which accents the `!`,
sets the text in monospace and keeps slash completion closed. A draft that
holds a command's output (a message taken back) is a message again, so it
is resent and never rerun. `ShellRunCard.svelte` shows the running command
above the composer. Keyboard submit and the button share
`deriveComposerSendState`'s `shell` input. The contract is in
[composer shell commands](../../../../../docs/specs/composer-shell-commands.md).

## Toolbar and background work

Composer controls register with the shared picker registry so keyboard commands,
compact rollups, and visible triggers invoke the same handlers. The density
ladder in `composerToolbarDensity.ts` preserves the model picker, context and
rate-limit meters, Send, and compact rollup. Keep those controls non-shrinking;
only the model label may ellipsize, and only at the tight rung below minimal.

Remote jobs use the shared background tray and retain their owning computer.
Closing or navigating the composer must not redirect a completion to the newly
focused pane.

Use the stepped working indicator. Do not add a continuously animated spinner.

The workspace strip resolves from the pane's `WorkspaceRef` and mounts only in
the desktop layout: machine, branch, worktree, cost. The project is the chat
header's crumb before the title. Under compact the chat header's facts line
(`chat/ChatHeaderFactsLine.svelte`) mounts the same pickers trigger-less behind
its own segments, and the usage chip sits at the right end of the activity
rail. `activityRailHost.svelte.ts` owns the rail's visibility, and
`densityLadder.ts` owns the measured density ladders of the toolbar and the
rail; keep every rung a single line and never clip controls or usage.
