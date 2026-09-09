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
- Mention and slash menus are caret-owned popovers and remain anchored in
  compact mode rather than becoming sheets.

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

## Toolbar and background work

Composer controls register with the shared picker registry so keyboard commands,
compact rollups, and visible triggers invoke the same handlers. The density
ladder in `composerToolbarDensity.ts` preserves the model picker, context and
rate-limit meters, Send, and compact rollup. Keep those controls non-shrinking;
only the minimal model label may ellipsize.

Remote jobs use the shared background tray and retain their owning computer.
Closing or navigating the composer must not redirect a completion to the newly
focused pane.

Use the stepped working indicator. Do not add a continuously animated spinner.

The workspace strip resolves from the pane's `WorkspaceRef`. Its nested pickers
must preserve computer, project, and worktree ownership and remain mounted when
represented by the compact combined trigger.
