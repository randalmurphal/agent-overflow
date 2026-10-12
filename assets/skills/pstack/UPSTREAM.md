# Upstream

This tree is pstack, vendored from
[backnotprop/pstack](https://github.com/backnotprop/pstack) at commit
`3a604672c46cd8187d2b19980eae0a34f9f91138`. That repository mirrors
`cursor/plugins/pstack` at `ccb5507` (version 0.15.15).

License: MIT, Copyright (c) 2026 Lauren Tan. The license text is
[`LICENSE`](./LICENSE), carried unchanged.

Nothing pulls from upstream. To resync, follow
[pstack port](../../../docs/references/pstack-port.md#resync).

## Not vendored

| Upstream path | Why |
|---|---|
| `.git`, `.gitignore`, `MIRROR.md`, `.cursor-plugin/` | Mirror and plugin packaging for other hosts |
| `assets/` | The upstream logo |
| `docs/` | The upstream guide and its screenshots, which teach the upstream host's tools |
| `skills/make-bot-ui/` | Not ported: it drives a webhook bot outside this app |
| `automations/benny/skills/setup-benny/`, `automations/benny/FOR_AGENTS.md` | Benny's setup for the upstream host's automations; the ported [Benny README](./automations/benny/README.md) replaces it |
| `automations/benny/templates/` | The automation prompts and the configuration template; the configuration is ported to [`automations/benny/configuration.example.yaml`](./automations/benny/configuration.example.yaml) |

## Changed for the port

Every edit replaces a host primitive with the app's tools, per the port
table in the [standing agents spec](../../../docs/specs/standing-agents.md#skills-library).
The model defaults move from upstream's model slugs to `inherit-parent`
and `other-provider`, read through the `setup-pstack` skill.

| File | Change |
|---|---|
| `README.md` | Replaced the mirror preface with a port preface; dropped install, plugin, Custom Mode, `/loop`, the upstream guide link and the `make-bot-ui` row |
| `automations/benny/README.md` | Rewritten: a standing agent runs Benny from a `thread_watch` on a configured report source |
| `automations/benny/configuration.example.yaml` | Added: the upstream template with `source` in place of `slack` and role values for models |
| `automations/benny/skills/triage-issue-reports/SKILL.md` | Slack replaced by a source adapter contract; coordinates come from the wake line; the follow-up window runs on `thread_loop` |
| `automations/benny/skills/triage-issue-reports/references/routing.example.md` | Copies live in the agent home; `source_channel` |
| `automations/benny/skills/reproduce-and-fix-issues/SKILL.md` | Same source adapter; waiting runs on `thread_loop`; the draft PR goes through the forge script |
| `automations/benny/skills/reproduce-and-fix-issues/references/control-adapter.md` | Copies live in the agent home; `ao-browser-tools` named for web and Electron apps |
| `automations/benny/skills/reproduce-and-fix-issues/references/feature-map.example.md` | Copies live in the agent home |
| `automations/benny/skills/reproduce-and-fix-issues/references/verify-existing-fix.md` | PR links come from the forge |
| `skills/architect/SKILL.md` | Settings file path; panel defaults |
| `skills/arena/SKILL.md` | Settings file path; panel defaults; candidates as subagents or threads by provider |
| `skills/automate-me/SKILL.md` | History from `thread_search` and `thread_show`; provider skill folders; question tool |
| `skills/create-verification-skill/SKILL.md` | Provider skill folders; `ao-browser-tools` as the web driver |
| `skills/how/SKILL.md` | Settings file path; spawning by provider; defaults |
| `skills/interrogate/SKILL.md` | Settings file path; spawning by provider; reviewer defaults |
| `skills/maintain-verification-skill/SKILL.md` | Provider skill folders |
| `skills/no-comments/SKILL.md` | Comment Sicko as a native subagent with its prompt |
| `skills/poteto-help/SKILL.md` | Install, settings, keeping the mode on, and tools not shipped, for this app; links to vendored files instead of the guide |
| `skills/poteto-help/references/prompting.md`, `skills/poteto-help/references/recipes.md` | `/loop` wording |
| `skills/poteto-mode/SKILL.md` | The Harness section becomes the Agent Overflow section (the port's map); Subagents section by provider; slop pass instead of `deslop` |
| `skills/poteto-mode/playbooks/authoring-a-skill.md` | Skill-authoring tool |
| `skills/poteto-mode/playbooks/autonomous-run.md` | `thread_watch` and `thread_loop` wakes |
| `skills/poteto-mode/playbooks/autopilot-full.md`, `autopilot-stack.md` | Owners are `thread_spawn` threads; the tick is a `thread_loop`; liveness through `thread_status` and `thread_ask`; forge script; Graphite mentions dropped |
| `skills/poteto-mode/playbooks/babysit.md` | watch-pr on both forges; the watch is `thread_watch` or `thread_loop`; replies through the forge script; Origin dropped; Graphite mentions dropped |
| `skills/poteto-mode/playbooks/bug-fix.md`, `feature.md`, `hillclimb.md`, `perf-issue.md`, `refactoring.md` | `thread_loop`; model defaults |
| `skills/poteto-mode/playbooks/eval.md`, `session-pickup.md` | Transcripts through `thread_show` and `thread_search` |
| `skills/poteto-mode/playbooks/multi-phase-plan.md` | Drivers, `thread_loop` tick, forge script, plans read from the installed library; Graphite mentions dropped |
| `skills/poteto-mode/playbooks/opening-a-pr.md`, `shipping.md` | Forge script; verifiers drive `ao-browser-tools` or the shell; Graphite mentions dropped |
| `skills/poteto-mode/playbooks/orchestrate.md` | Workers and sub-coordinators are threads; the store is the agent home; liveness through `thread_status` and `thread_ask`; the frontier from `forge stack` and retargets through the forge script instead of Graphite |
| `skills/poteto-mode/playbooks/visual-parity.md` | `browser_screenshot`; loop wording |
| `skills/poteto-mode/playbooks/worktree-cleanup.md` | Threads per worktree from `thread_search` instead of a transcript scan |
| `skills/poteto-mode/scripts/forge` | Added: the one entry to GitHub and GitLab |
| `skills/poteto-mode/scripts/orch/store.ts`, `orch.ts`, `orch.test.ts` | `frontier set` reads stack order, states and head SHAs from `forge stack` instead of Graphite `gt` |
| `skills/poteto-mode/scripts/check-plan.mjs` | Program markers for the ported plan template |
| `skills/poteto-mode/scripts/worktree-audit.sh` | PR state through the forge script; the transcript scan and its column removed |
| `skills/poteto-mode/scripts/watch-pr/github.ts` | `ForgeCliReader` reads through the forge script instead of `gh` |
| `skills/poteto-mode/scripts/watch-pr/types.ts`, `types.compile.ts`, `render.ts`, `cli.ts` | Forge-neutral PR links and labels |
| `skills/poteto-mode/scripts/watch-pr/github.test.ts`, `cli.test.ts` | Tests for the forge reader |
| `skills/recall/SKILL.md` | History from `thread_search` and `thread_show` |
| `skills/reflect/SKILL.md` | The transcript is the thread, exported with `thread_show`; spawning by provider |
| `skills/reflect/references/divergent-reviewer.md`, `judgment-reviewer.md`, `tooling-reviewer.md` | Skill folders and spawn tools to scan for |
| `skills/setup-pstack/SKILL.md` | Models from `thread_options`; one settings file for both providers; the *Reading a role* section |
| `skills/show-me-your-work/SKILL.md` | The audit reads the thread with `thread_show` |
| `skills/swarm/SKILL.md` | Workers are native subagents with their own worktrees, or threads on the other provider |
| `skills/why/SKILL.md`, `skills/why/references/source-playbook.md`, `skills/why/references/sources/code-archaeology.md` | PR context through the forge script; MCP discovery; spawning by provider |
