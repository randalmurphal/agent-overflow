---
name: agent-home
description: "The home of a standing agent: standing instructions, rulings, verification, one-fact memory files and the MEMORY.md index the app delivers. Load it at the start of every session of a standing agent and before writing to its home."
disable-model-invocation: false
---

# Agent home

You are a standing agent. Your session starts with a block that names your home (`<home>` below), and the app puts the text of `<home>/instructions.md` and `<home>/MEMORY.md` in your context when the thread starts and after each compaction. This skill says what goes in the home and how to keep it true over time: current, short, and free of slop.

## First load

1. List `<home>`. Keep every file there: the `README.md` the app wrote, your scripts, your notes. Never delete or rewrite a file this skill does not name.
2. Create whichever of these is missing: `instructions.md`, `rulings.md`, `verification.md`, `memory/`, `MEMORY.md`. Seed `instructions.md` from what the user has told you in this thread. Give the others a heading and nothing else.

## Layout

| Path | Holds | Written |
|---|---|---|
| `instructions.md` | Your standing instructions | By you, on a ruling |
| `rulings.md` | Every ruling the user gave you | By you, append-only |
| `verification.md` | What you check before calling work done | By you, when a miss is found |
| `memory/<slug>.md` | One fact per file | By you, when a fact changes a future session |
| `MEMORY.md` | The index of `memory/` | By you, with every memory change |

The app reads `instructions.md` and `MEMORY.md` when a new thread starts and after each compaction, never while you work and not when a thread resumes. An edit reaches your context at the next compaction or new thread, not now: keep the change in mind yourself until then. When you do not see either file in your context, read it from `<home>` before you act. The app removes control and invisible formatting characters before it measures either file against its cap, reads at most the first 1 MiB of a file (the rest is not read, and the delivered text says so), and reads only regular files: a link out of the home, a directory or a pipe in a file's place is reported, not read.

### instructions.md

Line 1 is your role in one sentence with no heading marker, for example `Release lead for the payments service: carries merge requests from review to merge.` The app quotes that line on every message you send to another thread, so the receiver can tell a lead from a worker. It takes the first non-blank line, after a leading frontmatter block and past rule lines such as `---`, and quotes at most 200 characters of it. A frontmatter block that is never closed is not skipped, so its first key line becomes the role: close it or leave it out. Then:

- **Grants.** What the user allowed, each with its scope and date: `2026-10-13: may push branches under agent/, never main.` Ask the user before any outward or destructive action no grant covers.
- **Standing rules** the user gave you.
- **Reports to.** The user, or a lead thread by id.

Edit it by hand in the turn the user rules. The app delivers at most 8 KiB of it and cuts the rest with a notice, so keep it under that.

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
---

<the fact>

Why: <feedback and project only>
How to apply: <feedback and project only>
```

- Write only what changes the actions of a future session. Never a procedural log, never a narration of what you did.
- The repository is the primary store for anything a repository doc should hold. Point at the doc; never copy it here.
- Update the file that covers a fact rather than add a near duplicate. When a fact turns out stale or wrong, fix or delete its file in the turn you find out.
- Name a source for every memory.
- Add, change or remove the memory's line in `MEMORY.md` in the same turn.

### MEMORY.md

The index the app delivers: one line per memory, grouped by type.

```markdown
# Memory

## User
- [Title](memory/<slug>.md): <description>

## Feedback

## Project

## Reference
```

Keep it under 4 KiB: the app delivers at most 4 KiB and cuts the rest with a notice. When it grows near that, merge or delete memories that no longer change a decision rather than shorten every line.
