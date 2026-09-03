# File attachments

Status: design signed off 2026-09-02, implementation in progress on
`main`. The transfer carrier on `main` is transitional; see
[Transfer carrier](#transfer-carrier).

## Goal

Drop any file onto the composer and the agent can read it. Today the
composer accepts images only (png/jpeg/webp/gif, 10 MiB, 8 per turn)
and refuses everything else with "Unsupported file type". After this
change every file type is accepted; images keep their native delivery
(Claude base64 block, Codex `localImage`) and everything else is copied
to the attachments root and referenced by path in the prompt.

## Decisions

- **Always copy, never reference the original.** A browser drop carries
  bytes and a name, never a source path, and a path would be meaningless
  from a remote client anyway. The agent gets a snapshot under the
  attachments root on every platform and every client, one code path.
  Files already inside the workspace are referenced with `@` mentions,
  which is a different gesture. (t3-code made the same call; its
  Electron desktop never reads the native path either.)
- **Two kinds: `image` and `file`.** The server decides the kind. A
  declared image MIME or image extension must validate as an image
  (signature, dimensions) exactly as today, or the upload is rejected;
  nothing else is ever reclassified as an image. Everything else is a
  `file`, including image formats no provider ingests (heic, bmp, svg).
- **Files never enter the provider attachment slice.** Images are bound
  positionally to `[Image #N]` markers on both sides of the wire; files
  have no marker and no slot. They reach the agent as one line each,
  appended to the provider content after a blank line:

  ```
  [Attached file "report.pdf" (application/pdf, 1.2 MB) is saved at: /home/u/.config/agent-overflow/attachments/<thread>/<id>/report.pdf]
  ```

  The line is provider-agnostic and goes through the one envelope
  (`resolveUserMessageEnvelope`), so send, steer, queued flush, resend,
  and claude-tui all get it. It is on `providerContent` only; the
  persisted `content` and the timeline row carry the attachment in
  `meta`, not as text.
- **Images do not get a path line.** Existing image turns keep their
  exact wire shape.
- **Claude gets `--add-dir <attachmentsRoot>` on every spawn** (headless
  and claude-tui), so Read on an attachment never prompts. The root is a
  process-constant path, so the flag rides `claude.Config` the way
  `ProjectsDir` does and never participates in restart diffing. Codex's
  sandbox reads anywhere and needs nothing.
- **Limits.** Images 10 MiB (unchanged), files 50 MiB, 8 attachments per
  turn total (unchanged). Both caps are backend constants mirrored in
  the frontend.
- **Paste stays image-only.** Copying a file in a file manager and
  pasting is not supported; the clipboard `File` path is per-platform
  and was ruled not worth it.
- **Drop only, no picker button.** Same as today.

## Non-goals

- Native path drop through Wails `EnableFileDrop` (desktop-only, needs
  Windows launcher path translation, gives the same gesture two
  meanings). Do not re-propose.
- Clipboard paste of non-image files.
- Native document blocks (PDF) on either provider. Claude Code's stream
  input has none that the CLI is known to honor; Codex has none.
- Reading files in the file chip (preview, open). A chip shows name,
  size, and a type glyph; that is all.

## Design

### Store

- `attachments.kind TEXT NOT NULL DEFAULT 'image'`, new migration.
  `store.Attachment.Kind` (`json:"kind"`), both positional scans and the
  INSERT updated.
- Disk layout. Images stay `<root>/<thread>/<id><ext>`. Files are
  `<root>/<thread>/<id>/<name>` where `<name>` is the sanitized original
  filename (separators, control bytes, leading dots stripped; bounded
  length; `file` when nothing survives). The agent-facing path then
  carries the real filename, and `cp` keeps it. `Delete` removes the
  `<id>` directory for a file.
- `Upload` derives the kind with the rule above. File uploads store the
  declared MIME or `application/octet-stream` when empty; no signature
  check beyond the image rule.
- `GetAttachmentData` and `GetAttachmentThumbnail` refuse `file` kind.
  The images-only allowlist existed so the directory was safe to serve;
  that guarantee now lives on the kind, not the directory.
- Cross-thread draft clone (`cloneUserMessageAttachmentsForDraft`)
  copies the file on disk instead of round-tripping bytes through base64
  `Upload`; a new store method does the copy plus INSERT with the same
  tmp-then-rename invariant. Applies to images too.
- Thumbnail generation is never attempted for files.

### Send path

- `resolveSendMessageAttachments` returns images (provider slice, in
  order) and the full persisted list. Files are excluded from the
  provider slice and collected separately.
- `resolveUserMessageEnvelope` appends the file lines to
  `providerContent`. The count cap applies to the union.
- `usermessage.AttachmentMeta` gains `kind`. Old rows without it are
  images.
- `provider.ImageAttachment` is unchanged; only images are built into
  it. `SplitContentByImageMarkers(content, imageCount)` keeps its
  contract because the frontend numbers markers over images only.
- Thread-title context lists every attachment by name; the image path
  list already filters by MIME.

### Provider

- `claude.Config.AdditionalDirs []string` → `--add-dir <dir>` per entry
  in `buildArgs`; stamped by the app at spawn beside `ProjectsDir` with
  the attachments root. Same flag on the claude-tui launcher.
- Codex: no change. `buildTurnInput` only ever sees images.

### Frontend

- `Attachment.kind: 'image' | 'file'` on the TS type and on the meta
  parse. `attachmentFromMeta` accepts files (any MIME, size ≤ file cap);
  a row missing `kind` is an image.
- Composer accepts any dropped file. Images take the existing path
  (compress, placeholder, tile). Files get a chip in the attachment row
  (type glyph, name, size, remove) and no textarea placeholder.
- Placeholder numbering, reconciliation, renumbering, and
  `ensureImagePlaceholders` operate on the image subset of
  `draft.attachments`; deleting `[Image #2]` deletes the second image,
  never a file. A file is removed only through its chip.
- Timeline user row renders file chips below the image grid, same
  glyph/name/size, no click action.
- A send waits for in-flight uploads before dispatch; nothing visible
  changes on the send control.
- Copy: "Drop files to attach"; count/overflow toasts say "attachments".
- `PendingAttachment` (dead type) is deleted.

### Transfer carrier

`main` carries bytes as base64 through the `UploadAttachment` WS RPC.
The WS frame cap is 75 MiB on both sides, so a 50 MiB file (~67 MiB
encoded) fits, at the cost of one transient string of that size on each
side. That is accepted as transitional: the `remote-access` branch
already replaced this RPC with a ticketed streamed `PUT
/attachments/upload` (wave 6b, `internal/transport/attachmentroutes.go`)
and a streaming store. When that branch rebases over this work, the
file kind rides the same route with no design change: the ticket fixes
filename, MIME, and byte count; the store's kind rule runs on the
streamed prefix for images and on the declared type for files; the
50 MiB cap replaces the route's 10 MiB body bound for `file` tickets.

## Verification

- Store: kind derivation, file layout, delete of a file directory, data
  and thumbnail refusal for files, clone-by-copy.
- Send: a mixed turn (image, file, image) produces `[Image #1]`,
  `[Image #2]` bound to the two images and one file line; persisted
  meta has three entries with kinds; queued flush and resend carry the
  same line.
- Claude args include `--add-dir <root>`; Codex turn input has no file.
- Frontend: drop of a `.pdf` uploads with the right MIME, shows a chip,
  no placeholder; deleting an image placeholder in a mixed draft leaves
  the file; timeline renders the file chip; send waits for an upload in
  flight.
- Live: drop a PDF on a Claude thread and a Codex thread, the agent
  reads it without a permission prompt.
