# Memory

Status: design agreed 2026-10-21 on the `personal-assistant` branch.
Assistants themselves are in [standing agents](standing-agents.md); this
spec owns what the app delivers to a session as memory, where it lives,
and the settings around it. Behavior is the same on Claude, claude-tui
and Codex.

## Goal

Agents keep the facts a future session would act on, and only those,
in plain files the app indexes and delivers. Three scopes hold them: an
assistant's home, a project, and the user. The app builds each scope's
index from the facts themselves at every delivery, so no agent maintains
an index and none can drift. Nothing ships as a skill: the rules an
agent needs to write memory arrive with the memory.

## Decisions

Rulings of 2026-10-21. Each is binding until the user changes it.

1. **No built-in skills.** The app embeds and installs no skills. Users
   install their own. What an agent needs to keep its home and memory is
   in its session block and in the delivery.
2. **One fact per file, typed.** A fact is a Markdown file with
   frontmatter. Its type says what kind of fact it is; a ruling the user
   gave and a recipe for verifying work are types, not files of their
   own. A reversed ruling is the same file edited.
3. **The app builds the index.** No hand-kept `MEMORY.md`. Each scope's
   index is read from its facts' frontmatter at delivery.
4. **Only facts that change a future decision.** The write rules ride
   with every delivery a session may write from. Progress, plans, task
   notes and anything temporary are never memory.
5. **Maintenance is the assistants' job.** Warnings about memory (a
   part over the size limit, a malformed fact, an expired fact, a
   context fact with no end) appear only in standing agents' deliveries,
   never in a worker's or an ordinary thread's.
6. **No version control.** The scopes are plain folders. Agents edit
   them with their own file tools; nothing commits.
7. **Shared memory is a setting.** Ordinary threads receive project and
   user memory only when the user turns on shared memory, off by
   default. Standing agents, and the threads that work for them, always
   receive it.
8. **One memory per session.** A session that receives Agent Overflow
   memory runs with its provider's own memory off by default, so an
   agent does not keep two memories that can disagree. Every other
   session keeps Claude's and Codex's own memory as the user configured
   it. A setting per provider changes where the app turns it off: in
   every session, or never. An assistant that runs with both shows a
   banner saying so ([standing agents](standing-agents.md#agent-home-contents)).
9. **Changes reach a live thread at its next prompt.** Each prompt
   delivers only what changed since the session last received its
   memory, from that session's own records, one per part; nothing when
   nothing changed. No version control and no state shared between
   sessions.
10. **Never cut.** Each section that can grow is a hook output of its
   own, so each has the whole of what a provider shows the model of one
   output. A section still over that is delivered whole, opening with its
   size, and the assistant's warning names it and how to fix it.

## Scopes

| Scope | Folder | Holds | Read by | Written by |
|---|---|---|---|---|
| Home | `<configDir>/agents/<agent-id>/memory/` | Facts about the assistant's role and work | That assistant | That assistant |
| Project | `<configDir>/memory/projects/<key>/` | Facts true of the project whoever learned them | Assistants assigned to the project; every thread in it that works for an assistant, and every one when shared memory is on | The same sessions |
| User | `<configDir>/memory/user/` | How the user decides, across projects. Small | Every assistant and every thread that works for one; every thread when shared memory is on | The same sessions |

A thread works for an assistant when it is one of the assistant's
helpers or descends on this computer from the assistant's thread or a
helper: a thread the assistant spawned, directly or through its own
workers at any depth, and an agent's ask copy of one
(`store.WorksForAssistant`). Lineage follows the spawn, not the row: a
worker spawned with a sidebar row, or moved to the sidebar later, still
works for the assistant, while
[sub-thread levels](agent-thread-tools.md#sub-thread-depth) keep their
own rule. A worker a paired computer's assistant spawned does not work
for one here: it receives memory only while shared memory is on on its
own computer.

**Project key.** The project's path relative to the user's home
directory with every `/` written as `--`; other characters stay as
they are. A path outside the home directory is written whole, so its
key starts with `--`. `~/repos/agent-overflow` is
`repos--agent-overflow`; `/srv/work/site` is `--srv--work--site`. A
project's path never changes, so the key is stable and a project
removed and added again finds its memory. A key longer than 200 bytes
keeps its first 183 bytes, cut at a character, and ends with `--` and
the first 15 hex digits of the SHA-256 of the path, so the folder name
stays within every file system's limit. The folder records the
project's absolute path in `.project-path`. Two paths can share a key
only when a folder name holds `--` itself. A project whose key folder
records another path gets no project memory: its sessions see a line
saying so in place of its index, and its assistants get a warning
naming the clash. The project the folder records keeps it.

The app creates a scope's folder, and the project folder's
`.project-path`, before the session that reads it starts; the marker
appears with its content and never replaces one another session wrote
first. `.project-path` holds one absolute path on one line; anything
else in it, nothing included, and a
`.project-path` or project folder that cannot be read (not a regular
file, unreadable), is a maintenance warning and turns the project's
memory off as a clash does. A scope folder that is a symbolic link or
not a folder is a warning on the thread and is never passed to the
provider as writable, and neither is a project folder whose memory is
off.

## Facts

A fact is a `.md` file directly in a scope's folder. Hidden files and
subfolders are not facts. The file opens with frontmatter:

```markdown
---
description: Release notes are drafted in the release issue, not in the repo.
type: convention
source: user, 2026-10-21
confirmed: 2026-10-21
---
Optional body: detail an agent reads when it opens the file.
```

| Key | Required | Meaning |
|---|---|---|
| `description` | yes | One line; the index shows it |
| `type` | yes | One of the types below |
| `source` | yes | Who said it or how it was verified |
| `confirmed` | yes | `YYYY-MM-DD`, the day it was last verified true |
| `expires` | no | `YYYY-MM-DD`; after it the index leaves the fact out |
| `until` | no | A plain-language end condition; an agent that sees it met deletes the fact |

Types, in index order:

| Type | Holds |
|---|---|
| `context` | Temporarily true and changes priorities (a release freeze, crunch mode). Must carry `expires` or `until` |
| `ruling` | A decision the user made: what to do, what not to do, what is allowed |
| `verification` | How to prove work is done: the check and how to run it |
| `pitfall` | Something that went wrong and how to avoid it |
| `convention` | How things are done here |
| `preference` | How the user likes things (user scope) |
| `reference` | Where something lives: a doc, a dashboard, a contact |

Other keys are ignored; keys and types are matched without regard to
case, so `Type` and `type` are the same key. Frontmatter that does not open on the first line, does not close
in the first 16 KiB, sets a key twice, lacks a required key, names an
unknown type or a malformed date makes the fact malformed: it is left
out of every index and reported to the scope's assistants. A fact whose
`expires` date is before the backend's local date at delivery is left
out and reported, so an assistant deletes it. A file whose name an
index line cannot show (invalid UTF-8, a control or format
character) is reported instead of indexed.

## Index

A scope's index is one line per fact, grouped by type in the order
above, files in name order within a type:

```text
### Project memory: agent-overflow (<configDir>/memory/projects/repos--agent-overflow/)
Context:
- Release freeze for 2.4; only fixes merge. (release-freeze.md; expires 2026-10-30)
Rulings:
- Never push to main; every change goes through a pull request. (no-direct-push.md)
Verification:
- A sidebar change is proven by its Playwright spec, desktop and compact. (sidebar-proof.md)
```

A context line carries its `expires` or `until`. A fact whose
`confirmed` is more than 90 days before the backend's local date at
delivery carries that date last, as in
`(release-notes.md; unchecked since 2026-06-01)`, which tells any reader,
one that may not write included, to check it before relying on it; the day a fact crosses the mark its line changes, and
the next prompt delivers it as a changed line. Descriptions are
cleaned as attached files are (`agenthome.CleanText`), kept to one line
and cut to 300 characters; an `until` to 200. An empty scope shows
`No facts yet.` under its heading.

Reads are bounded: the first 1000 entries of a folder, and the first
16 KiB of each file to find its frontmatter. A folder past the entry
bound is a maintenance warning. Every file is read as an attached file is
(`agenthome.CleanText`: invalid UTF-8 replaced, line ends made `\n`,
every Unicode control and format character removed except tab and line
break), through an `os.Root` on its folder, so a link out of it is not
followed, and only a regular file is read: the open does not wait
(`O_NONBLOCK` on Unix), so a FIFO cannot hang a delivery.

## Delivery

One hook delivers a session's memory in parts, each a command handler
of its own: this app's binary (`memory.HookSubcommand`) run with the
part and the path of the thread's plan file as its arguments. The app
writes the session's delivery plan, the folders and projects below, to
that file before the session starts, at one path per thread
(`<config>/memory-plans/<thread id>.json`), so a command's length does
not grow with the plan and Codex's trust hash of it does not change
from one session to the next. The file is outside the records folder,
which a rollback deletes under a live session, and goes with the
thread's delete. A run that cannot read it exits 1 with the reason on
stderr, which the provider reports as a part that did not arrive.
Claude and claude-tui register the handlers in the
session's inline `--settings`, Codex in the thread's `config` with the
trust hash of each command
([Codex hooks](../references/codex-wire.md#sessionstart-hooks-verified-01690)),
in the order below, for two events:

- **SessionStart**, matcher `startup|compact|clear`: the whole memory as
  it is then, for a new thread, after a compaction and after a clear.
  Each part's output opens with `## Agent Overflow memory, part i of n`
  (`## Agent Overflow memory` for a delivery of one part), the time of
  the delivery on the next line, and closes with
  `[End of Agent Overflow memory]`.
- **UserPromptSubmit**, at each prompt the provider takes, a queued or
  steered one included: for each part, what changed in it since the
  session last received it, nothing when nothing did. Each output opens
  with `## Changes to Agent Overflow memory` and closes with
  `[End of changes to Agent Overflow memory]`. A section whose text
  changed is shown whole; an index or a project list shows its new or
  changed lines and the files it no longer lists, each named on one line
  and escaped as the lines are; a section that is gone
  says so. The user part also names as gone the sections of a part the
  session received and the plan no longer has, such as an assistant's
  project index once it serves no project, whether or not the user part
  has a record of its own. With no record of what the session received
  of a part, the prompt delivers that part whole.

The parts, in delivery order, follow from the plan alone, so a change in
the folders never adds or removes one:

| Part | Holds | Present |
|---|---|---|
| Instructions | 1 below | assistant |
| Home | 2 | assistant |
| Project | 3 | a project, or an assistant serving one or more |
| User | 4 and 5 | always |
| Warnings | 6, or `[Nothing here needs fixing now.]` | assistant |

The write rules share the user part: they are a few fixed lines, and
read-only mode then changes no part.

Text inside either that would read as an ending or as a delivery's time
line is written with parentheses in place of its brackets.

A resume runs no SessionStart hook: the resumed history carries what
earlier runs delivered, and its first prompt delivers what changed
since. Each run records what it delivered, one file per provider
session and part in `<configDir>/memory-records/<thread id>/`, named by
a hash of the session id followed by the part, so a provider that runs the
parts at once never writes one file twice; the run prints before it
writes, so a record it could not write sends the change again rather
than losing it. A thread's records share no state with another
thread's, and no run reads another session's record. A session's
records live and die together: once no run has written any of them for
30 days, the next write deletes them all, so a long-lived session keeps
the record of a part that has not changed. A temporary file a failed
write left is deleted once it is 30 days old; any other file in the
folder is left alone.

The app deletes a thread's records whenever its model may have lost what
they say it holds and the hook would not deliver it again first, so the
next prompt delivers the whole memory:

- a start or a compaction that ended without every part's SessionStart
  output;
- on Codex, a turn that ends with that output still pending: Codex runs a
  compaction's hook only at the next turn, which a stop of the session
  or the app can prevent, and a session that ends, closed or with its
  app-server exited, with that output still pending;
- on claude-tui, a session closed before a model request showed the
  output;
- a revert that cuts the history in place (Codex's in-place rollback,
  claude-tui's Esc revert), before the cut and again once it lands;
- a session that starts with no memory hook, since a compaction in it
  removes the memory with nothing to record that;
- the thread's delete, an empty draft's included.

A Claude rollback and a fork start another provider session, which has
no record. A session whose delivery did not arrive is told by the
session block to read its files itself.

When a start or a compaction ends without a part's SessionStart output,
the provider puts a non-fatal error row on the thread saying
`Agent Overflow memory did not reach this session`, or
`Part 2 of 5 of Agent Overflow memory did not reach this session` when
other parts arrived, and why: the failed
run's cause where the provider reports one (Claude its exit code and
stderr, Codex its error entries), otherwise the likely causes (a hook
failed, or hooks are off in the provider's own settings), and that the
next prompt delivers it whole. A failed UserPromptSubmit run of the hook
is a non-fatal error row saying the changes may not have reached the
turn; Claude reports it as the hook's when its stderr opens with
`__memory-context:`, Codex by its session-flags source. Records the app
could not delete are a non-fatal error row too. On any thread that
receives memory, the app writes a warning row, naming what the failure
costs, for: a rollback it could not record; a folder or file it could
not read for a reason other than absence, which its sessions also
receive a notice about; a scope folder it could not create or will not
pass as writable; the records folder it could not create; each part
over the size limit, named with its size; and a hook it could not
build, in which case the session starts without its memory. Each provider checks once the hook
has had its chance to run
([Claude](../references/claude-wire.md#sessionstart-hooks-verified-21283),
[Codex](../references/codex-wire.md#what-agent-overflow-does)).

What a session receives, in order, the most important first:

1. The assistant's `instructions.md`, or a notice when it does not
   exist, is empty or cannot be read.
2. The assistant's home index.
3. The project index: the thread's project, or for an assistant
   assigned to one project, that project. An assistant assigned to
   several gets a project list instead: each project's name, path and
   memory folder with its fact count, or why the folder could not be
   read; it reads a project's facts when it needs them.
4. The user index.
5. The write rules, unless the session started in read-only mode,
   which refuses every write. claude-tui leaves approvals to the
   terminal, so its sessions always get them.
6. Maintenance warnings, for an assistant only.

| Session | 1 | 2 | 3 | 4 | 5 | 6 |
|---|---|---|---|---|---|---|
| Assistant | yes | yes | its projects | yes | yes | its home, its projects, user |
| Thread in a project that works for an assistant, or with shared memory on | no | no | its project | yes | yes | no |
| Thread outside every project (an assistant's helper) that works for an assistant, or with shared memory on | no | no | no | yes | yes | no |
| Any other thread, shared memory off | no hook | | | | | |

**Size.** Nothing is cut. Both providers save a hook's output past
10,000 units (Claude counts UTF-16 units, Codex bytes) to a file and
show the model a preview with the file's path: Claude the first 2,000
characters, Codex the start and the end
([Claude](../references/claude-wire.md#sessionstart-hooks-verified-21283),
[Codex](../references/codex-wire.md#sessionstart-hooks-verified-01690)).
Each provider applies the limit to each output on its own, so memory
over 10,000 bytes in total reaches the model whole while each part is
under it. On a Codex that reads `additionalContextLimit` (0.145.0 and
later, from the app-server's version) each handler sets it to 0, which
turns the spill off, so every part reaches the model whole; an older
Codex, or one whose version is unknown, gets the handlers as before. A
part, or a part's delivery of changes, over 10,000 bytes
(`memory.Limit`; a byte is never fewer than one UTF-16 unit) opens with
`[This part is N bytes, …]` (`[This memory is …` for a delivery of one
part), N its own size, telling the agent to read the saved file before
it acts, so the preview carries it. An assistant's maintenance warnings
name each such part and how to fix it, and the thread gets a warning row
naming it and its size at a start and at a compaction. An
`instructions.md` over 64 KiB, which no delivery carries, is replaced by
a line naming its path and size that tells the agent to read it, and is
a maintenance warning.

**Writable folders.** Every scope a session may write is passed as an
additional directory: `permissions.additionalDirectories` in the inline
`--settings` on Claude and claude-tui, a writable root in each Codex
turn's workspace-write sandbox policy, the one sandbox that limits
writes to the workspace. A memory write then gets the treatment of a
workspace write in each runtime mode; read-only refuses it as it
refuses every write. Claude's `--add-dir` is not used for them: it also
loads each directory's `.claude/skills` and plugin settings, and its
`CLAUDE.md` when `CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD` is set,
which would let a session that writes a memory folder add a skill to
every later session that reads it
([Claude](../references/claude-wire.md#additional-directories),
[Codex](../references/codex-wire.md#writable-roots-on-turnstart)).
An assistant's assigned projects' folders are additional directories too
([standing agents](standing-agents.md#standing-agent)).

## Write rules

The rules are a few lines at the top of the delivery, the same for every
session that writes, naming only the scopes that session may write:

- Save a fact only if a future session would act differently knowing
  it, it is confirmed (verified, or the user said it), no file, doc or
  code already holds it, and it outlives the current task. Never save
  progress, plans, task notes or anything temporary.
- One fact per file, with the frontmatter above.
- Update the existing fact instead of adding a near-duplicate. Delete a
  fact the moment it is shown wrong. Bump `confirmed` when you rely on a
  fact and it holds.
- Scope: the home for the assistant's role, the project for the
  project, the user only for how the user decides. A user's correction
  is a project `ruling` or a user `preference`. A check that caught a
  problem, or a miss that got through, becomes a project `verification`.
- When a human contributor would need it too, propose a change to the
  repository's docs instead.

## Maintenance warnings

An assistant's delivery ends with the warnings of the scopes it is
responsible for: its home, each project assigned to it, and the user
scope. A warning names the file and what to do:

- a part over the size limit: `instructions.md`, told to move detail
  into files in the home that it points to; an index, told to merge or
  delete facts, or move detail from their descriptions into their
  bodies; the project list, named alone;
- an `instructions.md` over 64 KiB, named instead of shown;
- a malformed fact, with the reason;
- an expired fact;
- a `context` fact with neither `expires` nor `until`;
- a project key clash.

Past 20 warnings about single files, the list ends with how many more
there are.

A project no assistant is assigned to has no one to warn; its facts are
still indexed for the threads that read them.

## Settings

| Setting | Default | Effect |
|---|---|---|
| Shared memory | off | Ordinary threads receive project and user memory and may write it; the threads that work for an assistant do either way |
| Turn off Claude's own memory (`claudeOwnMemoryOff`) | where Agent Overflow memory is used | In which Claude and claude-tui sessions the app turns Claude's own memory off: those that receive Agent Overflow memory, every one, or never. Off, a session sends `autoMemoryEnabled: false` and, in the same inline `--settings` block's `env`, `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`, which outranks the inherited environment and the user's settings; the user's settings files are never touched |
| Turn off Codex's own memory (`codexOwnMemoryOff`) | where Agent Overflow memory is used | The same choice for Codex threads. Off, a thread sends `features.memories = false` and `memories.generate_memories = false` in its thread config, and a resumed thread's stored memory mode is set to `disabled` (`thread/memoryMode/set`), so the thread neither uses memories nor is mined for them |

A session receives Agent Overflow memory when it is an assistant's, works
for one, or runs while shared memory is on, read-only or not (the
"Delivery" table). The app decides each session's provider memory from
that same fact (`receivesMemory`): a start and a resume when the
session is read, a Codex fork from the thread it works for (a local
spawn's caller or ask's asker, whose link is written after the fork),
and the throwaway resume of a rollback from the thread itself. Each
choice has one line under it on the settings page.

A computer whose settings answer lacks these keys runs a release before
them: its page says `<computer> runs an older version: it has no memory
settings.`, and nothing is read from or written there.

Shared memory beside a provider's own memory, which only "never"
allows, shows a warning on the setting that names the provider and the
choice that turns it off (`GetNativeMemoryStatus`). Claude's memory is decided as
Claude Code decides it: `CLAUDE_CODE_DISABLE_AUTO_MEMORY` turns it off
when true and on when false, taken from the `env` of
`~/.claude/settings.json`, else of `~/.claude.json`, else the
environment a session gets; otherwise `autoMemoryEnabled` in
`~/.claude/settings.json`, on when absent
([Claude](../references/claude-wire.md#auto-memory-switch),
[Codex](../references/codex-wire.md#memories-config)).
Codex's is on when `[features] memories` is true in
`~/.codex/config.toml`. Each provider is read on its own: one whose
file cannot be read or parsed says so on the page, naming the file, and
the other's answer still warns.

Shared memory gives sessions write access outside their workspace, so
it is a host setting (`internal/settings/tier.go`), like the other
grants of that kind. A prompt override's
`{{MEMORY_DIR}}` names Claude's own memory folder whether or not
Claude's memory is off.

A changed setting reaches each thread's next session start. A loaded
Codex thread keeps the memory config it was loaded with; ending its
session applies the change. A Codex thread that ran with Codex's own
memory off stays excluded from mining after the setting turns it back
on.

## Verification

Unit tests (`internal/memory`, `internal/app/app_memory_test.go`,
`app_standing_agent_home*_test.go`, the provider packages and
`MemorySettings.test.ts`) cover: the project key and its clash;
frontmatter parsing, each malformed case and expiry; index order and
line format, an old `confirmed` shown and a fresh one not; nothing cut
past the size limit, only a part itself over it carrying the size
notice, and the warning naming it with its fix; delivery order and the
parts per session kind; the changes a prompt delivers, nothing when
nothing changed, a part the plan no longer has named as gone once, and
a part without a record delivered whole; the records per part and every
path that deletes them; a start or compaction that misses one part, on
Claude, claude-tui and Codex; each part's handler registered on Claude,
claude-tui and Codex, and the Codex trust hash with and without
`additionalContextLimit` by version; the write rules
naming only writable scopes and left out in read-only mode; the project
list; warnings for
assistants only; shared memory on and off, a helper's user-only
delivery, and with shared memory off the delivery of an assistant's
workers at depth one and two, one spawned with a sidebar row and one
moved there, and none for a sidebar thread no assistant spawned; the writable folders, refused links and clashes; the
provider memory choices, each of the three deciding the provider's own
memory for an assistant, a sub-thread, a read-only one and an ordinary
thread with shared memory off and on, on Claude, claude-tui and Codex,
the other provider's choice playing no part, each value reaching every
real launch path, a Codex fork made by the person, by a thread tool
for an assistant and for an ordinary thread, and a rollback's
throwaway resume deciding as a start of that thread does, and the
resumed Codex thread's memory mode; the plan file in place of the plan
on the command line, its commands the same for one project and a large
catalog, a missing or unreadable plan failing the run, and its delete
with the thread; the provider memory status, its `env` layers, the
three choices on the settings page and its warning only where a choice
is never.
The standing-agent e2e specs cover, on both mock providers, the banner
on an assistant whose provider choice is never while its own memory is
on, shown and cleared on the assistant, never on its worker or an
ordinary thread, in both layouts; the whole
delivery at a start and a compaction, a running session receiving only
another writer's change at its next prompt, a resume's first prompt
delivering what changed between sessions, maintenance warnings,
sections over the size limit together reaching the model whole, a part
over it whole on Codex and previewed on Claude, and the first prompt
after a missed delivery receiving the whole memory.
