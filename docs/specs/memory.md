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
   delivery over the size limit, a malformed fact, an expired fact, a
   context fact with no end) appear only in standing agents' deliveries,
   never in a worker's or an ordinary thread's.
6. **No version control.** The scopes are plain folders. Agents edit
   them with their own file tools; nothing commits.
7. **Shared memory is a setting.** Ordinary threads receive project and
   user memory only when the user turns on shared memory, off by
   default. Standing agents always receive it.
8. **Provider memory is the user's.** The app leaves Claude's and
   Codex's own memory as the user configured it, and offers a setting
   per provider to turn it off in the app's sessions.
9. **Changes reach a live thread at its next prompt.** Each prompt
   delivers only what changed since the session last received its
   memory, from that session's own record; nothing when nothing changed.
   No version control and no state shared between sessions.
10. **Warn, never cut.** A delivery over what a provider shows the model
   whole is delivered whole, opening with its size; the agent reads the
   provider's saved file, and the assistant is warned to shorten it.

## Scopes

| Scope | Folder | Holds | Read by | Written by |
|---|---|---|---|---|
| Home | `<configDir>/agents/<agent-id>/memory/` | Facts about the assistant's role and work | That assistant | That assistant |
| Project | `<configDir>/memory/projects/<key>/` | Facts true of the project whoever learned them | Assistants assigned to the project; every thread in it when shared memory is on | The same sessions |
| User | `<configDir>/memory/user/` | How the user decides, across projects. Small | Every assistant; every thread when shared memory is on | The same sessions |

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
`.project-path`, before the session that reads it starts. `.project-path`
holds one absolute path on one line; anything else in it is a
maintenance warning and turns the project's memory off as a clash does.
A scope folder that is a symbolic link or not a folder is a warning on
the thread and is never passed to the provider as writable, and neither
is a project folder that records another project.

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

A context line carries its `expires` or `until`. Descriptions are
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

One hook delivers a session's memory: this app's binary
(`memory.HookSubcommand`) run as a command hook with the session's
delivery plan, the folders and projects below, as its argument. Claude
and claude-tui register it in the session's inline `--settings`, Codex
in the thread's `config` with the trust hash of each command
([Codex hooks](../references/codex-wire.md#sessionstart-hooks-verified-01571)),
for two events:

- **SessionStart**, matcher `startup|compact|clear`: the whole memory as
  it is then, for a new thread, after a compaction and after a clear.
  Its output opens with `## Agent Overflow memory`, the time of the
  delivery on the next line, and closes with
  `[End of Agent Overflow memory]`.
- **UserPromptSubmit**, at each prompt the provider takes, a queued or
  steered one included: what changed since the session last received
  its memory, nothing when nothing did. Its output opens with
  `## Changes to Agent Overflow memory` and closes with
  `[End of changes to Agent Overflow memory]`. A section whose text
  changed is shown whole; an index or a project list shows its new or
  changed lines and the files it no longer lists, each named on one line
  and escaped as the lines are; a section that is gone
  says so. With no record of what the session received, the prompt
  delivers the whole memory.

Text inside either that would read as an ending or as a delivery's time
line is written with parentheses in place of its brackets.

A resume runs no SessionStart hook: the resumed history carries what
earlier runs delivered, and its first prompt delivers what changed
since. Each run records what it delivered, one file per provider
session in `<configDir>/memory-records/<thread id>/`, named by a hash of
the session id; the run prints before it writes, so a record it could
not write sends the change again rather than losing it. A thread's
records share no state with another thread's, and no run reads another
session's record. A record, or a temporary file a failed write left,
that nothing wrote for 30 days is deleted at the next write; any other
file in the folder is left alone.

The app deletes a thread's records whenever its model may have lost what
they say it holds and the hook would not deliver it again first, so the
next prompt delivers the whole memory:

- a start or a compaction that ended without the SessionStart output;
- on Codex, a turn that ends with that output still pending: Codex runs a
  compaction's hook only at the next turn, which a stop of the session
  or the app can prevent;
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

When a start or a compaction ends without the SessionStart output, the
provider puts a non-fatal error row on the thread saying
`Agent Overflow memory did not reach this session` and why: the failed
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
pass as writable; the records folder it could not create; a delivery
over the size limit; and a hook it could not build, in which case the
session starts without its memory. Each provider checks once the hook
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
| Thread in a project, shared memory on | no | no | its project | yes | yes | no |
| Assistant's helper, shared memory on | no | no | no | yes | yes | no |
| Any thread, shared memory off | no hook | | | | | |

**Size.** Nothing is cut. Both providers save a hook's output past
10,000 units (Claude counts UTF-16 units, Codex bytes) to a file and
show the model a preview with the file's path: Claude the first 2,000
characters, Codex the start and the end
([Claude](../references/claude-wire.md#sessionstart-hooks-verified-21283),
[Codex](../references/codex-wire.md#sessionstart-hooks-verified-01571)).
A delivery, or a delivery of changes, over 10,000 bytes (`memory.Limit`;
a byte is never fewer than one UTF-16 unit) opens with
`[This memory is N bytes, …]`, N its own size, telling the agent to read
the saved file before it acts, so the preview carries it and the order
above puts the rest of the start to use. An assistant's maintenance
warnings say its memory is over the limit, and the thread gets a warning
row at a start and at a compaction. An `instructions.md` over 64 KiB,
which no delivery carries, is replaced by a line naming its path and
size that tells the agent to read it, and is a maintenance warning.

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

- a memory over the size limit;
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
| Shared memory | off | Ordinary threads receive project and user memory and may write it |
| Claude memory off | off | The app's Claude and claude-tui sessions send `autoMemoryEnabled: false` and, in the same inline `--settings` block's `env`, `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`, which outranks the inherited environment and the user's settings; the user's settings files are never touched |
| Codex memory off | off | The app's Codex threads send `features.memories = false` and `memories.generate_memories = false` in their thread config, and a resumed thread's stored memory mode is set to `disabled` (`thread/memoryMode/set`), so the thread neither uses memories nor is mined for them |

Turning shared memory on while a provider's own memory is on shows a
warning on the setting that names the provider and the setting that
turns it off (`GetNativeMemoryStatus`). Claude's memory is decided as
Claude Code decides it: `CLAUDE_CODE_DISABLE_AUTO_MEMORY` turns it off
when true and on when false, taken from the `env` of
`~/.claude/settings.json`, else of `~/.claude.json`, else the
environment a session gets; otherwise `autoMemoryEnabled` in
`~/.claude/settings.json`, on when absent
([Claude](../references/claude-wire.md#auto-memory-switch),
[Codex](../references/codex-wire.md#memories-config)).
Codex's is on when `[features] memories` is true in
`~/.codex/config.toml`.

Shared memory gives sessions write access outside their workspace, so
it is a host setting (`internal/settings/tier.go`), like the other
grants of that kind. A prompt override's
`{{MEMORY_DIR}}` names Claude's own memory folder whether or not
Claude's memory is off.

A changed setting reaches each thread's next session start. A loaded
Codex thread keeps the memory config it was loaded with; ending its
session applies the change. A Codex thread that ran while Codex memory
was off stays excluded from mining after the setting is turned back on.

## Verification

Unit tests (`internal/memory`, `internal/app/app_memory_test.go`,
`app_standing_agent_home*_test.go`, the provider packages and
`MemorySettings.test.ts`) cover: the project key and its clash;
frontmatter parsing, each malformed case and expiry; index order and
line format; nothing cut past the size limit, the size notice and its
warnings; delivery order per session kind; the changes a prompt
delivers, nothing when nothing changed, and the whole memory without a
record; the records and every path that deletes them; the write rules
naming only writable scopes and left out in read-only mode; the project
list; warnings for
assistants only; shared memory on and off, and a helper's user-only
delivery; the writable folders, refused links and clashes, and the
native-memory settings on Claude, claude-tui and Codex, including
the resumed Codex thread's memory mode; the native-memory status, its
`env` layers and its warning.
The standing-agent e2e specs cover, on both mock providers, the whole
delivery at a start and a compaction, a running session receiving only
another writer's change at its next prompt, a resume's first prompt
delivering what changed between sessions, maintenance warnings, a
delivery over the size limit, and the first prompt after a missed
delivery receiving the whole memory.
