---
name: reflect
description: Spawn three parallel review subagents over the active transcript, surface learnings, and route each to a concrete edit on an existing skill. Use when the user says reflect.
disable-model-invocation: true
---

# Reflect

Mine the current conversation for durable learnings, then route them into skill edits.

## When to invoke

Invoke when the user says "reflect" or "/reflect". Skip when the conversation is trivial, off-topic, or already covered by an existing skill the parent followed correctly. One-offs are not learnings.

## Process

### 1. Locate the active transcript

The parent finds its own thread before fanning out. Read only this thread, never another project's threads and never provider session files on disk: those cross workspace boundaries and read private chats from unrelated work.

Call `thread_search` with no query and the active project's `project_id`. Your own thread is the row marked `self`. Write the whole thread to a file with `thread_show` on that id with `all`, `include` set to tool calls, tool outputs and subagent runs, and `to_file` set to a path under the session's temp directory. That file is the transcript the reviewers read. If the export fails, write a tight digest of the session and pass that instead.

### 2. Spawn three reviewers in parallel

One message, three subagent spawns, with the model set as below and your own runtime mode. Reviewers need MCP and shell access for context lookups (tickets, chat threads, observability traces referenced in the transcript). The `read-only` runtime mode blocks the shell on Claude, so never use it for a reviewer.

**Spawning.** A role on your own provider runs as a native subagent (`Agent` in Claude, `spawn_agent` in Codex). A role on the other provider runs as a thread through `thread_spawn` (see **setup-pstack**, *Reading a role*). Keep the prompt and the model. Drop parameters your tool doesn't have. If your provider has no subagent tool in this turn, run each role yourself, one after another.

Each reviewer and the synthesizer name a role line in the pstack settings file (`~/.agents/pstack-models.md`, written by the **setup-pstack** skill) and a default. Use that line's value, or the default if the file or the line is missing, and resolve it as the **setup-pstack** skill's *Reading a role* section says.

| Lens | Role line | Default `model` | Prompt template |
|---|---|---|---|
| Judgment | `reflect judgment, divergent, synthesizer` | `inherit-parent` | `references/judgment-reviewer.md` |
| Tooling | `reflect tooling` | `inherit-parent` | `references/tooling-reviewer.md` |
| Divergent | `reflect judgment, divergent, synthesizer` | `inherit-parent` | `references/divergent-reviewer.md` |

Pass each template verbatim, substituting the transcript path or digest where marked. Reviewers return findings in their final message.

### 3. Synthesize

One subagent spawn, with the model from the `reflect judgment, divergent, synthesizer` line (default `inherit-parent`) and your own runtime mode. The synthesizer's quality check includes spot-verifying citations, which can require shell or MCP access. Use `references/synthesizer.md` verbatim, with each reviewer's full output inlined where marked. The synthesizer returns a structured Accepted / Rejected / Backlog list.

### 4. Structural enforcement check

Sanity-check the synthesizer's Accepted list. For any item that would be enforced more reliably by a lint rule, script, metadata flag, or runtime check, move it from Accepted to Backlog. See the **encode-lessons-in-structure** principle skill.

### 5. Apply

Before applying any Accepted edit, present the synthesizer's full Accepted/Rejected/Backlog output to the user and wait for explicit approval. The user picks which subset to apply and may redirect routings. Skill changes affect every future agent in the org. Do not auto-apply.

Backlog items file to whatever devex / backlog tracker your team uses automatically. Only the Accepted list waits for approval.

For each approved Accepted item, follow the Routing field exactly:

- Trivial existing-skill edit (a one-line bullet, a tightened sentence, a stale fact corrected): parent does directly.
- Substantive existing-skill edit (a new section, a new pattern table, more than ~10 lines): hand to a skill-authoring skill and run its draft / test / iterate loop: Anthropic's `skill-creator` where it is installed. If you have none, follow the Agent Skills format at agentskills.io. "`create-skill`" below means whichever of these you use.
- `tune description: <skill path>` (the skill exists but didn't trigger when it should have): hand to `create-skill` and run its description-optimization loop.
- `new skill via create-skill: <kebab-name>`: hand creation to `create-skill`. Do not invent the shape ad hoc.

If your environment ships a SKILL.md validator, run it on every touched skill before declaring done. Skip this step if it doesn't.

### 6. Summarize for the user

Short list, no preamble:

- Edits applied: `<skill path>`. What changed, one line each.
- New skills created: `<skill path>`. One line each (rare).
- Backlog filed to the devex tracker: `<issue title>` (`<tags>`). One line each.
- Dropped: one line per rejected finding + reason from the synthesizer.
