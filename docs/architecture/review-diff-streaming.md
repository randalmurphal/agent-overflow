# Review diff streaming and patch memory

How the review surfaces read a diff of any size and hold its text within a
memory budget. Covers the review pane (every scope) and the workflow gate
diff.

## Reading a diff

An `Open*Diff` call (`internal/app/app_review_diff_handles.go`) returns the
first chunk of the patch and, when the patch continues, a handle. The handle
belongs to the connection that opened it and records the scope its `Open*`
method required; `ReadReviewDiff(id, offset, maxBytes)` requires that scope
again on every read. The backend holds no copy of the patch. A read at the
offset where the previous read ended continues one git process; a read
anywhere else runs git again and skips to the offset, verifying the skipped
bytes against the hash recorded when they were first read
(`internal/gitdiff/stream.go`). A read can start only at 0 or at an offset
an earlier read ended at. Edits diffs join persisted payloads instead of
running git and follow the same contract (`app_edit_diffs.go`).

Chunks end after their last newline; a line longer than the read splits at
a UTF-8 boundary (`gitdiff.ChunkCut`). Reading the same offset with a window
no larger than the first read's returns the same chunk, which is how evicted
text is read again.

There is no size limit on any diff source.

A diff's two sides are resolved once, by `gitdiff.Sides`, for the patch and
for every file read from the same diff. A PR's commit run
(`OpenPRCommitDiff(first, last)`) is the two-tree diff from `first`'s first
parent (the empty tree for a root commit) to `last`, both in the commit
list `ListPRCommits` answers at the same revision, `first` no newer than
`last`; one commit is that commit's own change, and a run of several
covering the whole list is refused in favor of `OpenPRDiff`
(`gitdiff.CommitRangeSides`). The list holds the newest 300 commits of a
longer PR: a run may start at the oldest listed commit and is then read
(it is not the whole PR), and a commit past the list is refused. A run is
a two-tree diff, so one holding a merge of the base branch shows what
that merge brought in.

## Reading a diff's files

Every git-backed open answers what pins its two sides (`ReviewDiffOpened`):

| Scope | `sides.old` | `sides.new` |
|---|---|---|
| commit, PR, PR run | commit or empty tree | commit |
| workspace, commit dialog | `HEAD` commit at the open, or the empty tree | working tree |
| branch | merge-base commit | working tree |

Both are full object IDs the open already had; pinning writes nothing to
the user's object database. A working-tree new side (`sides.new` empty)
also answers `worktreeBlobs`: each changed regular file's blob ID and size
in the open's snapshot (at most 5000; `worktreeBlobsTruncated` past it).
When the pin cannot be read, `sidesError` says why and the patch still
reads.

`GetReviewDiffFileSizes` (up to 100 files) and `GetReviewDiffImage` (one
file, both sides) take the open's `sides` and a `ReviewDiffFile`: its path,
a rename's `oldPath`, and on a working-tree side the `newBlob` the open
pinned. Each pinned ID must be a full ID of a commit or tree. Each path is
one literal, canonical path whose entry on each side it exists on is a
regular file the other side holds differently; a directory, a symlink, a
submodule or a path the diff does not change is refused. A committed side
is read as its blob, its size from the tree entry first; a working-tree
side is read from disk and its blob ID computed, and bytes that are not
the pinned blob are "changed since the diff was read". Sizes of a
working-tree side are the file's size on disk; the image read proves
content. A failed size read leaves the patch on screen; each file of a
size batch fails alone.

A side over 25 MiB answers its size and `overCap` and none of it is read;
bytes that are not an image a browser displays answer `notImage`. Image
bytes are held in `reviewimage.Store`, keyed by blob ID: one 100 MiB budget
covers held entries, loads in flight and entries a response is still
sending. A load reserves its size before it reads, concurrent loads of
one blob share one read, an entry being sent is freed only when its last
reader closes, and a load that finds no room waits up to 30 seconds, then
refuses with a sentence. Entries expire after 10 minutes; the harness
reset clears them. They are served once per ticket from the local-image
route, where an SVG gets an attachment disposition and a policy that
allows no script.

The edits scope has no git sides: `GetEditDiffImage` takes the path and
an edit's payload ID, or the turn index for the turn's last edit of the
path, and serves only the content the edit recorded. The old side is
always `unavailable` (a sentence), and so is the new side of an edit that
recorded no content; the file on disk is never read.

## Holding the patch

`ReviewDiffSource` (`frontend/src/lib/stores/reviewDiffStream.ts`) reads the
whole patch into a `PatchParser`. The store (`utils/patchStore.ts`) keeps
the chunk strings plus typed-array indexes: line starts, one kind byte per
line and the hunk table. Files are published as they complete, so a pane
with nothing on screen shows the diff as it arrives.

Patch text read from a diff counts against one budget shared by every
surface (`utils/patchMemory.svelte.ts`): 256 MiB, or 64 MiB in compact
layout. Past it, chunk text is evicted; line indexes, kinds and hunks stay,
so file lists, row counts, line numbers and anchors never wait for text.
Eviction takes text nobody has read first, the latest to arrive first, so a
large diff keeps its beginning. Read text goes after, least recently read
first. Pinned text (a line still arriving, a range a reader holds) is not
evicted and does not count.

A read whose text all fit releases its handle at the end. A read that
evicted anything keeps the handle, and the store reads evicted chunks
through it, one at a time in the order asked. The owner of the read (the
pane, the gate diff) disposes it when its files go, which releases the
handle and the budget. When evicted text can no longer be read (the handle
closed with its connection, or the diff changed), the store reports it lost
once and the owner reads the diff again.

## Reading text that may be evicted

- Render paths show placeholders. `BlockRowsCache` builds a block over
  evicted lines with empty `pending` rows, reads the lines again and builds
  the block with them pinned, so a block completes even when the text on
  screen passes the budget. Rows own copies of their text (`ownText`), so a
  kept block holds no chunk.
- Exact readers (highlight requests, edit verification, merged edit files,
  PR comment excerpts, the blocks of an expanded gate diff file) read
  inside `whenResident`, which reads evicted text again and pins it while
  the reader runs.
- A synchronous derivation must not read text that can be evicted: it would
  request the text and run again whenever the text returns. Read what it
  needs beforehand, as context expansion records hunk headings in its state
  (`readHunkHeadings`).

## Diffs taller than the browser

A browser caps an element's height (Chromium and WebKit near 33.5M px,
Firefox near 17.9M px), which a diff passes near 900K lines. The review
diff body and an expanded gate diff file render through
`LongListVirtualizer` (`frontend/src/lib/components/virtual/`), which holds
a range of rows short of every cap and moves it as the reader scrolls
toward its ends or jumps elsewhere. Scrolling across a move keeps the line
being read in place, and the file tree, comment jumps and reading-position
restores reach any row. The gate diff renders a file in blocks read from
compact storage (`PatchLines`), never the whole file at once.
