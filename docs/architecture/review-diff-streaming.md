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
for every blob read from the same diff. A PR's commit run
(`OpenPRCommitDiff(first, last)`) is the two-tree diff from `first`'s first
parent (the empty tree for a root commit) to `last`, both in the commit
list `ListPRCommits` answers at the same revision, `first` no newer than
`last`; one commit is that commit's own change, and a run of several
covering the whole list is refused in favor of `OpenPRDiff`
(`gitdiff.CommitRangeSides`).

## Reading a diff's files

The patch says a file is binary (`Binary files ... differ`) and names each
side's blob on its `index <old>..<new>` line, all zeros for a side that does
not exist; a pure rename or mode change has no index line.
`GetReviewDiffFileSizes` (up to 100 files) and `GetReviewDiffImage` (one
file, both sides) take the diff's source in its Open call's terms
(`ReviewDiffSource`: workspace, branch, commit, or pr with its revision and
run) and the file as its patch named it (`ReviewDiffFileRef`: paths and the
index line). Each call resolves the diff's sides again, lists the requested
paths the diff changes (`git diff --raw --no-renames`, literal pathspecs)
and serves a side only when that path changes and its blob is the one the
index line named, so a worktree file rewritten since the patch was read, or
a path the diff does not touch, is refused. A file with no index line must
hold one blob on both sides. A failed size read leaves the patch on screen;
each file of a size batch fails alone.

A side over 25 MiB answers its size and `overCap` without reading it; bytes
that are not an image a browser displays answer `notImage`. Image bytes are
held in a memory LRU keyed by blob ID (100 MiB, 10 minutes, cleared by the
harness reset) and served once per ticket from the local-image route,
where an SVG gets an attachment disposition and a policy that allows no
script. The edits scope has no git sides: `GetEditDiffImage` serves the new
side its context expansion would serve, verified against the file's patch,
and never an old side.

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
