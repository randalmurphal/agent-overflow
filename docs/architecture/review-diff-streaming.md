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
compact storage (`WorkflowDiffLines`), never the whole file at once.
