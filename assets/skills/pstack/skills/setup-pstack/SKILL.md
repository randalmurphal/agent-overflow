---
name: setup-pstack
description: Configure which models pstack uses per role and at what reasoning budget. Detects the models Agent Overflow offers and writes the settings file every pstack skill reads when it picks a model. Use for /setup-pstack, "configure pstack models", "pstack budget", or changing pstack's model choices.
---

# Setup pstack

Write pstack's model settings, one model per role, to `~/.agents/pstack-models.md`. Claude and Codex threads read the same file, so one choice holds on both providers. Every skill reads it when it picks a model; nothing loads it into context on its own. "The settings file" below means this file.

## Reading a role

A role's value is one of:

- `inherit-parent` or `auto`: your own provider and model. Spawn a native subagent and leave its model unset.
- `other-provider`: the other provider's `default_model` in `thread_options`, at your own effort if that model offers it.
- `<provider> <model> <effort>`: a model as `thread_options` spells it, such as `claude claude-opus-5-5 xhigh` or `codex <model> high`.

A role on your own provider runs as a native subagent (`Agent` in Claude, `spawn_agent` in Codex) with that model and effort where your subagent tool takes them. Where it does not, run the role on your own model and say so. A role on the other provider runs as a thread: `thread_spawn` with its `provider`, `model` and `effort`, the active project, and `wait_seconds` (or `notify: true` when you will not wait). Its answer comes back as the spawn's reply. Keep your runtime mode for both. A thread in the `read-only` runtime mode cannot run a shell command on Claude, so a role told not to edit files is told so in its prompt instead. If a configured model is not in `thread_options`, use the role's default and say so.

## Steps

### 1. Detect available models

Call `thread_options` with `what` set to `models`. It lists each provider's models with the efforts each offers. That is the dependable source. Never write a model it does not list. The aliases `inherit-parent`, `auto` and `other-provider` are always valid.

### 2. Load current state

The default role-to-model mapping is the rule shape shown in step 5 below. If the settings file already exists, read it and treat its `# budget` line and its role values as the current choices. Otherwise start from those defaults. A line whose role is not in step 5, such as `how critics`, is from a retired role. Drop it.

### 3. Budget, map, and confirm

**(a) Ask for a budget.** Prefer your question tool (`AskUserQuestion` in Claude) over free text; without one, ask in chat with numbered options. Offer these four options with these exact labels, and name the current budget when the settings file records one. With no file, say that `large` matches the skill defaults.

- `unlimited — max reasoning`
- `large — xhigh reasoning`
- `medium — high reasoning`
- `small — medium reasoning`

**(b) Apply it.** Build the working table from the skill defaults, and on a re-run keep any role you changed by model, list, or alias. `unlimited`, `large`, `medium`, and `small` set the effort of every `<provider> <model> <effort>` value, panel entries included, to `max`, `xhigh`, `high`, or `medium`, on the ladder `max` > `xhigh` > `high` > `medium` > `low`. If the model does not offer that effort in `thread_options`, use its highest offered effort at or below the target, else mark the role as needing a choice. The aliases do not change: they follow your own effort or the other provider's model at it.

**(c) Show the roles and confirm.** Show every role with its model, marking any model not in `thread_options` as needing a choice. Also list each line step 2 dropped. Ask whether to accept as-is or change specific roles, offering the detected models plus `inherit-parent` (this role runs on the parent chat model) and `other-provider` as the options. Prefer your question tool over free text. For panel roles (arena runners, architect runners, interrogate reviewers) the value is a list, and one subagent runs per entry, alias entries included, so the list length sets the count. `arena cross-judge pool` is also a list, but Arena selects one value from it whose model family differs from the parent's when possible. `swarm workers` is the default model for every worker unless a race or comparison assigns another model per arm.

### 4. Validate

Every model written must be in `thread_options`, with an effort it offers. The aliases always pass. If a chosen model is not available, stop and ask again.

### 5. Write the settings file

Write the settings file with a `# budget` line with the chosen label and its target effort, and one line per role, using the same labels poteto-mode uses. Overwrite the whole file so re-runs stay idempotent. Shape:

```
# pstack model configuration. One line per role. Delete a line to fall back to the skill default.
# Values: inherit-parent (or auto), other-provider, or "<provider> <model> <effort>" as thread_options spells them. Alias entries in a panel list still count toward its fan-out.
# budget: large (xhigh)
feature, refactoring: inherit-parent
bug-fix: inherit-parent
perf-issue: inherit-parent
hillclimb: inherit-parent
judgment and prose: inherit-parent
hardest tasks: inherit-parent
how explorer: inherit-parent
how explainer: inherit-parent
why investigators: inherit-parent
why synthesizer: inherit-parent
reflect tooling: inherit-parent
reflect judgment, divergent, synthesizer: inherit-parent
arena runners: inherit-parent, other-provider
arena cross-judge pool: inherit-parent, other-provider
swarm workers: inherit-parent
architect runners: inherit-parent, other-provider
interrogate reviewers: inherit-parent, other-provider
```

### 6. Confirm

Tell the user which file was written. pstack skills read it when they pick a model, in Claude and Codex threads alike. Re-running this skill updates it.

### 7. Offer a verification skill (optional)

Check whether the project has a way to drive the real app for proof (a `verify-*` skill, or an existing harness). If not, offer once: "want a project-local verification skill, so agents can drive the app the way a user does and prove changes work? I can generate one with /create-verification-skill." On yes, invoke `/create-verification-skill` (resolves wherever pstack is installed: workspace, user, or plugin). On no, move on without pushing.
