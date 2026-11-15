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

Every chunk, the open's included, carries `key`: `{hash, units}`, the
32-bit FNV-1a hash of the patch text from offset 0 to the chunk's end over
its UTF-16 code units, and how many units that is (`internal/jstext`). The
text is what a JSON client receives for the bytes, each invalid UTF-8 byte
one U+FFFD; since no cut falls inside a valid UTF-8 sequence, the text of
the chunks in order is the whole patch's. The reader owns the key: it
records the key at each boundary beside what verifies the bytes before
it, and extends the start boundary's key with the chunk's text
(`gitdiff.Diff.Read`, `editsDiff.Read`), so a re-read answers the key it
answered first. An edits diff that reaches a boundary again with another
key has changed since it was opened. A packed chunk carries the plain
chunk's key.

`ReadReviewDiffPacked` reads the same chunk under the same rules, for a
backend that advertises `review-diff.packed.v1`, and answers it packed
where `jstext.PackText` packs it; the opens' first chunks stay plain. All
three answer one shape, `ReviewDiffChunk`. A browser stores a string at
two bytes per character once any character in it is past U+00FF, so one
such character doubles a chunk. Packed `data` holds the chunk's UTF-8
bytes one per character (U+0000..U+00FF), which a browser stores at one
byte each, and decoding those bytes gives exactly the text the plain read
delivers: each invalid UTF-8 byte becomes U+FFFD first, as JSON encoding
does. `wide` lists, for each line of `data` holding a byte past ASCII, the
index of the first such byte, so only those lines are decoded, each time
one is read. Packing needs a byte past ASCII, so a chunk is packed
exactly when `wide` is non-empty, and a plain chunk omits it. A chunk is
packed only when it holds a character past U+00FF and its lines with a
byte past ASCII are at most 1/16 of its bytes; any other chunk is
answered as the plain read answers it. The form depends on the chunk's
bytes alone. Offsets stay raw patch positions, so packed `data`'s length
differs from `nextOffset - offset` when the chunk held invalid bytes. A
packed chunk is at most seven JSON bytes per raw byte (`jstext.PackText`),
which keeps an 8 MiB chunk under the transport's read limit
(`gitdiff.MaxChunkBytes`).

There is no size limit on any diff source.

A diff's two sides are resolved once, by `gitdiff.Sides`, for the patch and
for every file read from the same diff. A PR's commit run
(`OpenPRCommitDiff(first, last)`) is the two-tree diff from `first`'s first
parent (the empty tree for a root commit) to `last`, both in the commit
list `ListPRCommits` answers at the same revision, `first` no newer than
`last`; one commit is that commit's own change, and a run of several
covering the whole list is refused in favor of `OpenPRDiff`
(`gitdiff.CommitRangeSides`). The list (`ListPRCommits`, and the branch
scope's `ListBranchCommits`) answers `{commits, total}`: the newest 300
commits and the range's real count, from `git rev-list --count` within 5
seconds only when the list is cut short, or `null` when that count fails.
The list is complete exactly when `total` equals the number of commits. A
run may start at the oldest listed commit of a cut list and is then read
(it is not the whole PR), and a commit past the list is refused. A run is
a two-tree diff, so one holding a merge of the base branch shows what
that merge brought in.

## Reading a diff's files

Every git-backed open pins its two sides and keeps the pin on the server:

| Scope | Old side | New side |
|---|---|---|
| commit, PR, PR run | commit or empty tree | commit |
| workspace, commit dialog | `HEAD` commit at the open, or the empty tree | working tree |
| branch | merge-base commit | working tree |

Both are object IDs the open already had; pinning writes nothing to the
user's object database. A working-tree side is pinned by the open's
snapshot: each path it changes, with its mode and, for a regular file, the
blob ID and size the snapshot recorded. The snapshot hashes a changed
file's bytes as they are on disk, without filters, so a read proves a
file by hashing its bytes the same way.

The open reserves a record and answers its `filesHandle` with the first
chunk at once; pinning settles the record in the background under its own
5 second bound, parses git's raw output as it arrives and stops git after
5000 paths. The pin holds the snapshot until it exits, so a diff that
closes first does not remove it; the last holder does. A record is as
strict as a diff id: it belongs to the connection that opened it, a read
rechecks the scope the open required, and the owner's teardown, the
record's eviction, the harness reset and shutdown cancel and join its
pin, which then publishes nothing. The store holds at most 64 records,
least recently used first out, counted from reservation, and at most
20000 paths and 8 MiB of names across all records: a settling pin that
needs room evicts least recently used records, never itself, until it
fits. Only a pin past 5000 paths or 8 MiB of names on its own fails with a
sentence (`app_review_diff_records.go`); an evicted record's reads answer
`review_diff_files_gone`.

`GetReviewDiffFileSizes` (up to 100 files) and `GetReviewDiffImage` (one
file, both sides) take the workspace, the handle and each file's `path`
(and a rename's `oldPath`). A read of a pending record waits for its pin,
within the caller's context. A handle the caller's connection does not
hold, in that workspace, answers `review_diff_files_gone` ("This diff's
files are no longer held; reload the diff."), whoever else holds it; a
record whose pin failed answers `review_diff_files_unreadable` ("This
diff's files cannot be read: <why>."), which reopening would not change. Each path is one literal name: no empty, `.` or
`..` segment, no backslash, no NUL; spaces at either end and pathspec
magic are part of the name. Each side it exists on must be a regular file
the other side holds differently; a directory, a symlink, a submodule or
a path the diff does not change is refused with
`review_diff_file_refused` (the reason), which neither a retry nor a
reload changes. A committed side is read as
its blob, its size from the tree entry first. A working-tree side is read
through the workspace as a root, so a symlink swapped in cannot lead
outside it, its type and size taken from the opened file; bytes that are
not the recorded blob answer `review_diff_files_changed` ("This file
changed since the diff was read; reload the diff."). Sizes of a
working-tree side are the recorded ones, so only an image read answers
it. Each file of a size batch fails alone, with its `error` and `code`.
Every origin, a connected browser included, gets the code and sentence;
the client branches on the code (`reviewDiffFiles.ts`).

A side over 25 MiB answers its size and `overCap` and none of it is read;
bytes that are not an image a browser displays answer `notImage`. A side
holding a Git LFS pointer answers `unavailable` ("Stored with Git LFS; the
image is not in the repository."), and a working-tree file with a
`filter` attribute ("This file passes through a git filter, so its image
is not shown."); nothing is downloaded or smudged. Image bytes are held in
`reviewimage.Store`, keyed by the blob the record names: one 100 MiB
budget covers held entries, loads in flight and entries a response is
still sending. A load reserves its size before it reads, concurrent loads
of one key share one read, an entry being sent is freed only when its last
reader closes, and a load that finds no room waits up to 30 seconds, then
refuses with a sentence. A panicking load releases what it holds, and a
load that finishes after a reset publishes nothing. Entries expire after
10 minutes. They are served once per ticket from the local-image route,
where an SVG gets an attachment disposition and a policy that allows no
script.

The edits scope has no handle: `GetEditDiffImage` takes the path and an
edit's payload ID, or the turn index for the turn's last edit of the path
(found by scanning the turn's patches from the end), and serves only that
edit's recorded snapshot. Every read reads the snapshot row; the image is
held under the SHA-256 of its compressed bytes, so a hit skips only the
decompression and a replaced or cleared snapshot is never served from
the cache. A load reserves the snapshot capture limit (4 MiB, enforced by
`PutEditFileSnapshot`) first, decompresses into a buffer of the recorded
size and shrinks the reservation to it. The old side is
always `unavailable` ("An edit does not record the file before it."), as
is the new side of an edit that recorded nothing ("This edit's file
content was not recorded.") or a path the turn does not edit ("This turn
does not edit this file."); the file on disk is never read.

## Holding the patch

`ReviewDiffSource` (`frontend/src/lib/stores/reviewDiffStream.ts`) reads the
whole patch into a `PatchParser`. The store (`utils/patchStore.ts`) keeps
typed-array indexes over the chunk strings: line starts, one kind byte per
line and the hunk table. The strings, their residency and their reads
again are `utils/patchChunks.ts`'s, addressed by chunk number. Files are
published as they complete, so a pane with nothing on screen shows the
diff as it arrives.

After the open's chunk, `ReviewDiffSource` reads with `ReadReviewDiffPacked`
when the diff's backend advertises `review-diff.packed.v1`, else with
`ReadReviewDiff`, so a store holds a chunk in one of two forms: plain (the
text) or packed (its UTF-8 bytes), as each answer's `wide` marks say. A wide line is a line of packed text
holding a byte past ASCII; the store keeps one bit per line for it, set from
the chunk's `wide` marks as the chunk is indexed. Every other line reads as
it is stored. A wide line's pieces are joined, each packed piece decoded
on its own (`utils/packedText.ts`): no chunk ends inside a character. The
parser checks each chunk's span once before storing any of it: a packed
chunk has marks at bytes past ASCII and its key, and starts and ends
between characters, or the read fails. The comment source key is the backend's: each chunk carries `key`,
the FNV-1a hash and UTF-16 length of the patch text through its end, and
the parser takes it without reading the text. A chunk without a key (a
backend older than it) is hashed as it arrives, continuing from the key
before it; a packed chunk without one fails the read, and a key that is
not an unsigned 32-bit hash with a count the chunk can have fails it too:
for a plain chunk the count so far plus its length, for a packed chunk no
less than the count so far and no more than that plus its bytes.
The highlight key hashes one file's decoded text. Neither depends on how
the chunks arrived. Positions and sizes the
store keeps (`PatchBody.textSize`, `lineSize`) count stored characters: at
least the text's UTF-16 length and at most its UTF-8 length, used only as
size bounds.

Patch text read from a diff counts against one budget shared by every
surface (`utils/patchMemory.svelte.ts`): 256 MiB, or 64 MiB in compact
layout. The budget counts what the engine holds: a packed chunk its length,
a plain chunk two bytes per character when any character in it is past
Latin-1 and one otherwise. Past it, chunk text is evicted; line indexes, kinds and hunks stay,
so file lists, row counts, line numbers and anchors never wait for text.
Eviction takes text nobody has read first, the latest to arrive first, so a
large diff keeps its beginning. Read text goes after, least recently read
first. Pinned text (a line still arriving, a range a reader holds) is not
evicted and does not count while it is held; a chunk pinned twice counts
once, and pinned text that is evicted, waiting to be read again, counts
nothing.

The store releases the handle when the patch ends with all of its text
held. A read that
evicted anything keeps the handle, and the store reads evicted chunks
through it, one at a time in the order asked, each in the form it first
arrived in; a chunk read again in another form or length is lost. The owner of the read (the
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
