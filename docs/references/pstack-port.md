# pstack port

`assets/skills/pstack/` is pstack, poteto's skill library, vendored at a
pinned upstream commit and ported to the thread tools so Claude and Codex
threads run the same skill text. The port table and the product rules
live in the [standing agents spec](../specs/standing-agents.md#skills-library);
[`UPSTREAM.md`](../../assets/skills/pstack/UPSTREAM.md) records the pin,
the license, what was not vendored and every file the port changed.

## What the port maps

The Agent Overflow section of
[`poteto-mode/SKILL.md`](../../assets/skills/pstack/skills/poteto-mode/SKILL.md)
is the map every other skill reads through. Choices a resync must keep:

- Model roles live in `~/.agents/pstack-models.md`, one file both
  providers read, written by `setup-pstack`. A role value is
  `inherit-parent`, `other-provider`, or `<provider> <model> <effort>` as
  `thread_options` lists it. A role on the thread's own provider runs as
  a native subagent; one on the other provider runs as a `thread_spawn`
  thread, because a native subagent cannot change provider. Defaults are
  `inherit-parent`, plus an `other-provider` seat on the review panels.
- Transcripts are threads: `thread_search`, `thread_show` and
  `thread_item`, never provider session files.
- Waking is `thread_loop` for any thread and `thread_watch` for a
  standing agent; nothing sleeps in a shell loop.
- Agents run `forge which` once to learn the forge, then use `gh` or
  `glab` directly. The forge script is the adapter pstack's scripts read
  the forge through, and `forge stack` reads a stack.
- Who merges is governed by the agent's instructions and the
  repository's guides; when the operator merges, the merge step ends at
  merge-ready with the PR and head SHA reported, and auto-merge is never
  armed.
- Spawned briefs start with `/poteto-mode`, the user-message entry that
  loads the skill on both providers; Claude's Skill tool refuses a
  model's own invocation of a `disable-model-invocation` skill. A worker
  with no slash entry reads `SKILL.md` by path.
- Benny's skills read reports through a source adapter the agent
  configures; a standing agent runs them from a `thread_watch`.

## Forge script

`skills/poteto-mode/scripts/forge` is the adapter `watch-pr`, `orch` and
`worktree-audit.sh` read the forge through, and the way to read a stack.
It is one bash script with the same JSON output on both forges: `which`,
`pr view|merge-state|list|create|comment|comments|checks|commits|threads|reply|merge|edit`,
`ci status|log` and `stack`. `forge --help` lists the arguments. It needs
bash 3.2 or later (stock macOS), git, jq and `gh` or `glab`. It sets
`inherit_errexit` where bash has it, but its checks do not depend on it.

It decides the forge from the checkout's remote (`FORGE_REMOTE`, default
`origin`):

1. The remote URL's host is taken as written. An SSH host alias is
   resolved with `ssh -G`, as the app does, so `git@work:group/repo` maps
   to the host the alias names.
2. `github.com` or a host containing `github` is GitHub; `gitlab.com` or
   a host containing `gitlab` is GitLab.
3. Any other host needs `FORGE=github` or `FORGE=gitlab`; the script
   refuses to guess.

GitHub calls go through `gh` with `-R host/owner/repo` and `gh api
--hostname`; GitLab calls go through `glab api --hostname` with the
URL-encoded project path. `pr create` names the remote's repository on
both CLIs (`-R`), so it opens in that repository whichever remote the
CLI would pick. A paginated GitLab list is slurped into one array, since
`glab --paginate` prints each page's array after the last, and a GitLab
write sends `Content-Type: application/json`. Bodies reach the CLI on
stdin or as one argument, never through a shell. A `pr` read that
defaults to the current branch fails, printing nothing, on a branch with
no open pull request; it, `ci status` and `stack` fail the same way on a
detached HEAD. `watch-pr`, `orch` and `worktree-audit.sh` read through
the script, so they work on both forges.

`forge stack` replaces Graphite. It walks base links from a branch (the
current one by default) down to the repository's default branch and
prints those pull requests bottom first. A head with an open request
uses it, else its newest merged one, else its newest closed one, so a
merged base the child was not retargeted from still shows. A branch on
the chain with no pull request exits 3, and a cycle of bases fails, so
`orch frontier set` never records a guessed stack.

## Tests

`make pstack-check` runs:

- `TestForgeScript` (`cmd/ao-mockforge/forge_script_test.go`) runs every
  subcommand on both forges against `forgefake` through the real
  `ao-mockforge` binary, including an SSH alias, writes read back
  through the app's `ReadPR`, and a refusal for an unknown host.
  `TestForgeScriptStack` covers `forge stack` on both forges: a stack of
  three, merged and closed bases, the default branch, a cycle, and a
  branch with no pull request (exit 3). `-v` prints the run log.
- The tripwires in `internal/threadtools/skills_tree_test.go`.
  `TestSkillsNameOnlyToolsThatExist` fails when a skill names a
  `thread_*` or `browser_*` tool the app does not serve.
  `TestSkillsCarryNoReplacedPrimitive` fails when a replaced primitive
  (`/loop`, `subagent_type`, provider home paths, `control-ui`, Graphite
  `gt` and the rest of the list) comes back, or when a script other than
  the forge script runs `gh` or `glab`; `keptVerbatim` lists
  the upstream lines kept on purpose. `TestSkillsLinkOnlyFilesThatExist`
  fails when a relative Markdown link names a file the tree does not
  carry.

- `make pstack-scripts`: the watch-pr and orch tests and the watch-pr
  type check under bun, pinned in `ci/Dockerfile`. orch's bootstrap
  installs their dependencies from `bun.lock` first, which needs the
  network; the tests run in the isolated namespace. CI runs it in the
  Go tests (rest) job.

## Resync

1. Clone upstream and check out the new commit.
2. Diff it against the pinned commit, not against this tree:
   `git diff <pinned>..<new> -- skills automations agents README.md LICENSE`.
3. Apply the upstream changes here, keeping the port's edits. For a file
   `UPSTREAM.md` lists, reapply its change by hand. Leave the paths
   `UPSTREAM.md` lists as not vendored out.
4. Run `make pstack-check`. Every hit names a file and line that brought
   back a replaced primitive or a tool name the app does not serve; port
   it to the mapping above.
5. Update the pin and the changed-file table in `UPSTREAM.md`.
