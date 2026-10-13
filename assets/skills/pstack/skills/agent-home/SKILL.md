---
name: agent-home
description: "The home of a standing agent: standing instructions, rulings, verification, one-fact memory files, the injected MEMORY.md index and the hygiene loop that keeps it true. Load it at the start of every session of a standing agent and before writing to its home."
disable-model-invocation: false
---

# Agent home

You are a standing agent. Your session starts with a block that names your home (`<home>` below) and carries the text of `<home>/instructions.md` and `<home>/MEMORY.md` when they exist. This skill says what goes in the home and how to keep it true over time: current, short, and free of slop.

## First load

1. List `<home>`. Keep every file there: the `README.md` the app wrote, your scripts, your notes. Never delete or rewrite a file this skill does not name.
2. Create whichever of these is missing: `instructions.md`, `rulings.md`, `verification.md`, `memory/`, `MEMORY.md`. Seed `instructions.md` from what the user has told you in this thread. Give the others a heading and nothing else.
3. Call `thread_status` with no arguments. If no loop is labeled `agent-home hygiene`, register it (see [Hygiene loop](#hygiene-loop)).

## Layout

| Path | Holds | Written |
|---|---|---|
| `instructions.md` | Your standing instructions | By you, on a ruling |
| `rulings.md` | Every ruling the user gave you | By you, append-only |
| `verification.md` | What you check before calling work done | By you, when a miss is found |
| `memory/<slug>.md` | One fact per file | By you, when a fact changes a future session |
| `MEMORY.md` | The index of `memory/` | By the hygiene loop only |

The app reads `instructions.md` and `MEMORY.md` when a session starts or resumes, never while it runs. An edit reaches your next start or resume, not the session you are in.

### instructions.md

Line 1 is your role in one sentence with no heading marker, for example `Release lead for the payments service: carries merge requests from review to merge.` The app quotes that line on every message you send to another thread, so the receiver can tell a lead from a worker. Then:

- **Grants.** What the user allowed, each with its scope and date: `2026-10-13: may push branches under agent/, never main.` Ask the user before any outward or destructive action no grant covers.
- **Standing rules** the user gave you.
- **Reports to.** The user, or a lead thread by id.

Edit it by hand in the turn the user rules. No loop edits it. The session start injects at most 8 KiB of it and cuts the rest with a notice, so keep it under that.

### rulings.md

Append one line per ruling and never edit or delete one:

```markdown
- 2026-10-13 [thread <id>, item <id>]: <the ruling, in the user's terms>
```

A ruling that reverses an earlier one is a new line that says which. When a ruling changes a standing rule or a grant, update `instructions.md` in the same turn.

### verification.md

What you verify before you call anything done, and how: the checks you run, the evidence you require, and what counts as slop (a claim with no check behind it, a test run reported but not run, a summary standing in for evidence). When a miss gets past you, add the check that would have caught it.

Hand it to every worker and reviewer: pass `home_files: ["verification.md"]` on `thread_spawn` and on `thread_send`, plus the `rulings.md` or `memory/` files that bear on the task. The app prepends each file to the message under its name. Attached files total at most 32 KiB, and only a standing agent can attach them.

### memory/\<slug\>.md

One fact per file:

```markdown
---
name: <slug, the file name without .md>
description: <one line; the index shows it>
type: user | feedback | project | reference
source: <thread id and item id, or a date and where it came from>
last_verified: <YYYY-MM-DD>
---

<the fact>

Why: <feedback and project only>
How to apply: <feedback and project only>
```

- Write only what changes the actions of a future session. Never a procedural log, never a narration of what you did.
- The repository is the primary store for anything a repository doc should hold. Point at the doc; never copy it here.
- Update the file that covers a fact rather than add a near duplicate. Delete a file that turns out wrong.
- Name a source for every memory. One without a source shows in the index as unverified until you add one.
- Do not touch `MEMORY.md` when you write a memory. The next hygiene pass indexes it.

### MEMORY.md

The index the session start injects: one line per memory, grouped by type.

```markdown
# Memory

## User
- [Title](memory/<slug>.md): <description>

## Feedback

## Project

## Reference

<!-- agent-home:archive -->
## Archive
- [Title](memory/<slug>.md): <description>
```

Nothing from the line `<!-- agent-home:archive -->` down is injected. Keep the part above it under 4 KiB: the session start injects at most 4 KiB and cuts the rest with a notice. Only the hygiene loop writes this file, never at the moment you write a memory and never by hand, so its bytes stay the same between loop runs and the session's prompt cache holds.

## Hygiene loop

Register one loop on yourself with `thread_loop`: `label` `agent-home hygiene`, `every_seconds` 21600 (six hours; pick another interval if your memory changes faster or slower), `note` `Run the agent-home hygiene pass.` When it fires, load this skill and run the pass:

1. Read the frontmatter of every `memory/*.md`.
2. For each memory, check that its source still exists (`thread_show` the thread `around` the item) and that no later line in `rulings.md` contradicts it. A memory that fails either check gets `stale: <reason>` in its frontmatter and leaves the index. Set `last_verified` to today on every memory you checked.
3. Merge duplicates into one file and delete the rest.
4. Rewrite `MEMORY.md` from the frontmatter: one line per memory that is not stale, grouped by type. End the line of a memory with no source with `(unverified: no source)`.
5. While the part above the archive marker is over 4 KiB, move the line of the least recently verified memory below the marker.
6. Write nothing else. End the turn by stating what changed: memories marked stale, merged, archived and verified, and the index size. That is the wake's outcome.

A stale memory stays on disk with its reason until you delete it or its source confirms it again.
