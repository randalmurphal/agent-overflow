# internal/workflow/memory/

Campaign memory: what a run TREE learned, so later waves stop relearning it.
This package owns the pure half: what one note is, where the log lives, how a
line is appended and read back, and how the injected digest renders. It is
stdlib plus `internal/untrustedtext` (the one quoting rule; notes are
model-authored text entering another model's prompt).

It holds no app, engine, store, or provider concern and never learns a run's
shape. Which tree a note belongs to and what provenance it carries are the
app's answers (`app_workflow_memory.go`).

## Why the tree is keyed by the ROOT run

A campaign is the run TREE, not the run. The live case this came out of is a
recursive spine that calls itself for the next wave and fans out to lanes that
each call a child workflow; keying memory by the writing run would give every
lane its own log that nothing else ever reads, which is precisely the failure
being fixed. `wave` is the writing run's `call_depth` (the engine's own
counter, read off the row), so a note says how far down the call chain it was
written without this package or the app maintaining a parallel one.

Storage is `<configDir>/workflow-memory/<root-run-id>/notes.ndjson`. It is
deliberately NOT in the repository or a worktree: a campaign's memory is not
part of its deliverable, so putting it on the branch would make every lane's
merge carry it and every discard delete it.

## The kind vocabulary is closed

`pattern | warning | learning | handoff`. A bad kind is refused loudly (a CLI
usage error before the round trip, an envelope validation finding after it),
exactly as a bad envelope status is. The closed set is what keeps the log
greppable and the digest groupable; a kind nobody can group by is a note nobody
reads.

`handoff` is the one kind with a mechanical privilege: it claims injection
budget ahead of every other kind, because it exists specifically to reach the
next element and losing one to a budget defeats it. Nothing else is ranked.

**`ruling` as an operator-only kind is deliberately absent** and deferred
pending its own scope conversation. `TestRulingIsNotAKind` is there to make
adding it a conscious edit rather than a drive-by.

## Provenance is stamped, never supplied

`Draft` is what an author gives (kind, text, files). `Note` is what lands on
disk (a draft plus `Provenance` and `At`). `NewNote` is the only constructor and
takes provenance as a PARAMETER, so there is no shape a caller could put a
forged one in:

- the CLI wire (`WorkflowAgentMemoryInput`) has no provenance field at all;
- the envelope's `memory` entries are closed objects, and post-validation
  reports `provenance` / `at` / `wave` as `property is not allowed` rather than
  ignoring them. An element told a field is merely ignored keeps sending it.

## Bounds

| Bound | Value | Why |
|---|---|---|
| `MaxTextBytes` | 4 KiB | A note is a lesson; the narrative file is where an element writes what it did. Also keeps one appended line inside a single bounded write. |
| `MaxFiles` / `MaxFilePathBytes` | 20 / 512 B | A pointer at evidence, not an inventory of a change. |
| `def.MaxEnvelopeMemoryNotes` | 20 | Per ENVELOPE, not per tree. |
| `DefaultInjectionBytes` | 8 KiB | The whole rendered block. |
| `DigestEntryRunes` / `DigestEntryFiles` | 320 / 5 | One digest entry. ~20 notes fit the budget. |
| `MaxLineBytes` | 64 KiB | A line past this stops the scanner, which would silently lose the rest of the log, so it is a read FAILURE, not a skip. |

**There is no per-tree total.** A campaign legitimately accrues hundreds of
notes and the log is the record; what is bounded is the INJECTION.

## Aging is the budget and newest-first ordering, and nothing else

No curation gate, no human graduation step, no decay score. Every note is
eligible the moment it is written. The prior art this was specified against
failed in exactly the two ways those mechanisms fail: a heavy knowledge
subsystem nobody used, and a human-graduation gate that made agent notes a
write-only log.

`Render` has two ordering axes answering different questions:

- **Selection** (what survives the budget): every `handoff` newest-first, then
  everything else newest-first.
- **Rendering**: grouped by kind in `Kinds` order, newest-first inside each
  group, because a reader scanning for "what went wrong here before" wants the
  warnings together.

Entries fall off **whole**. A digest ending mid-note is a lesson half-learned,
which is worse than one never seen. The header states what it holds (`12 of 117
notes`) and names the log on every render, including when it holds everything:
an element that needs depth on one note should open the log rather than ask for
a bigger budget. An EMPTY campaign still gets a block naming the path. An
element on wave one has to learn the mechanism exists before it writes the first
note.

The budget reservation is a deliberate over-estimate: the header is measured at
its longest possible form (`headerLine(total, total, path)`, whose shown-count
digits can never exceed the total's) and all four group headings are reserved
whether or not their groups end up present. Under-reserving would break the
budget the block promises.

## Crash safety

`Append` is one `O_APPEND` write of one whole line. Appending through a
temp-and-rename would mean reading the whole log back and rewriting it, turning
every note into a read-modify-write over a file that grows all campaign.

A crash can therefore leave a torn FINAL line, and two things follow:

- `ReadNotes` skips it and REPORTS it (`Skipped{Line, Reason}`), never fatal.
  The accumulated memory of a whole campaign must survive the crash that
  truncated one note. The reason never carries the line's content, which may be
  arbitrary bytes. Callers log the skip and `memory list` counts it in its
  header; nothing about it is silent.
- `Append` heals the log: a file whose last byte is not a newline gets one
  prepended to the next line, so the tear costs ONE note rather than welding
  every later note onto the wreckage. Without this the first torn line poisons
  the log forever, which is what the first version did.

A well-formed JSON object that is not a note (an unknown kind, blank text) is
reported like a torn line rather than rendered: the digest groups by kind, and a
note with no group is one no reader would ever be shown.

## What lives elsewhere

| Concern | Where |
|---|---|
| Which tree a run writes into, provenance resolution, appends, digest rendering for a prompt | `app_workflow_memory.go` |
| The `memory add` / `memory list` bound methods and their row-level authorization | `app_workflow_agent_bindings.go` |
| The envelope's `memory` control field: schema fragment, post-validation, the strip seam | `internal/workflow/def/envelope_memory.go` |
| The prompt section stating the channel per `access` | `internal/workflow/runner` (`writeMemorySection`) |
| The CLI verb | `internal/aocli/exec_memory.go` |
| The curator phase (content, not machinery) | `internal/workflow/starters/content/port-one-task/` |
