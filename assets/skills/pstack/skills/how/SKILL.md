---
name: how
description: "Use for \"how does X work\", code walkthroughs before changing something, and placement / ownership / layering questions (\"where should this live\", \"which package owns this\", \"is this the right layer\"). Explains subsystem architecture, runtime flow, onboarding mental models. Use why for motivation."
disable-model-invocation: true
---

# How

Explore the codebase to answer "how does X work?" questions. Produce architectural explanations at the level of a senior engineer onboarding onto a subsystem, enough to build a working mental model, not so much that it reads like annotated source code.

Each spawn below names a role line in the pstack settings file (`~/.agents/pstack-models.md`, written by the **setup-pstack** skill) and a default. Use that line's value, or the default if the file or the line is missing, and resolve it as the **setup-pstack** skill's *Reading a role* section says.

## Step 1. Assess Complexity

If the scope is ambiguous, state your interpretation and explore. The user can redirect.

- **Simple** (a single module, a small utility, a narrow question such as "how does function X work"): no explorers. One explainer explores and explains in a single pass. Go to Step 2b.
- **Complex** (a subsystem spanning multiple files or services, a cross-cutting feature, a full architectural overview): spawn parallel explorers first, then hand off to the explainer. Go to Step 2a.

When in doubt, take the simple path.

**Spawning.** A role on your own provider runs as a native subagent (`Agent` in Claude, `spawn_agent` in Codex). A role on the other provider runs as a thread through `thread_spawn` (see **setup-pstack**, *Reading a role*). Keep the prompt and the model. Drop parameters your tool doesn't have. If your provider has no subagent tool in this turn, run each role yourself, one after another.

## Step 2a. Explore (complex questions only)

Decompose the question into 2 to 4 exploration angles, each a distinct slice of the subsystem. Spawn all explorers in a single message:

- `model`: the `how explorer` line, default `inherit-parent`
- read-only: the prompt says it must not edit files

Each explorer gets the prompt in `references/explorer-prompt.md` with its angle filled in. Then go to Step 3.

## Step 2b. Direct Explain (simple questions)

Spawn one subagent that explores and explains in one pass:

- `model`: the `how explainer` line, default `inherit-parent`
- read-only: the prompt says it must not edit files

Build its prompt from `references/explainer-prompt.md` without the explorer-findings section. Go to Step 4.

## Step 3. Synthesize (complex questions only)

Once all explorers have returned, spawn one subagent to synthesize their findings into one explanation:

- `model`: the `how explainer` line, default `inherit-parent`
- read-only: the prompt says it must not edit files

Build its prompt from `references/explainer-prompt.md` with every explorer's findings filled in.

## Step 4. Present

Present the explainer's output to the user. Light edits for clarity or context from the conversation are fine. Do not substantially rewrite it.

## Output Format

The explanation uses the sections defined in `references/explainer-prompt.md`, dropping any that do not apply: Overview, Key Concepts, How It Works, Where Things Live, Gotchas.
