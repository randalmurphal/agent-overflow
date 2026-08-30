# internal/worktreesetup/

The per-project worktree setup recipe: what it is, what makes it valid, and
how it runs. One project-level config, executed whenever a new worktree is
created for that project.

Storage is `projects.worktree_setup` (a JSON blob, migration v46) and it is
edited in Settings → Projects. It used to be the `worktree_setup` block of
`<config-root>/projects/<slug>/profile.yaml`; that block is now a validation
finding that tells the author to move it here (see
`internal/workflow/profile/AGENTS.md`).

## Boundaries

- No store, no transport, no workflow packages. The config arrives as a
  value; where it was persisted is the caller's problem.
- `Run` blocks. It is called from the app-owned provisioning path before the
  first turn runs in a new worktree, never from the workflow engine
  goroutine.
- A failure is always an error, never a warning, never a partial success.
  The caller decides what that means: workflow provisioning rolls the worktree
  back and parks `needs-human(setup-failed)`, while a chat thread keeps the
  worktree, surfaces the failure, and offers a retry (`app_worktree_setup.go`).
- Observation is optional and additive. `RunObserved` is the whole engine and
  `Run` is it with a no-op observer. There is ONE execution path, so the
  blocking caller cannot drift from the streaming one.

## Files

| File | Responsibility |
|---|---|
| `config.go` | `Config`, `Validate`, `ResolveTimeout`, `DefaultTimeout`. |
| `copy.go` | The copy phase and every path-safety rule it enforces. |
| `run.go` | Env rendering, the run loop, and the per-command execution. |
| `observe.go` | `Observer`, `Step`/`StepKind`, `ResolveSteps` (the step list a config will execute, copy first), and the per-step output writer. |

## Order and bounding

`Run` copies first, then executes commands in authored order, stopping at the
first failure. Every command shares ONE `context.WithTimeout` derived from
`Config.Timeout` (`DefaultTimeout` = 10m when absent). The bound is on the
recipe, not per command, because a recipe that takes longer than its budget is
finished either way. Each command runs in its own process group and a timeout
kills that group (`internal/procutil`), so a step that backgrounded its real
work cannot outlive the bound.

The run loop owns the command's output sink and passes it in as an
`io.Writer`: one bounded tail buffer (16 KiB) whose contents the failure
message quotes, multiplexed with the observer's per-step writer. That
`io.MultiWriter` is the one seam streaming goes through. The failure text is
built from the tail either way, so an observed run and a blocking one produce
byte-identical errors.

`ResolveSteps` is the pure projection callers render a progress list from. It
is what `RunObserved` itself walks, so the list a caller shows before the run
starts is exactly the list the observer reports against. The copy phase is
step 0 and is omitted entirely when the recipe names no globs (a step that
provably does nothing is noise), and indices stay contiguous either way
because steps are addressed by position in the returned slice.

## Copy safety

These are the properties the copy phase exists to hold. Each has a test.

- Globs are project-root-relative. Absolute paths and anything resolving
  outside the root are refused, never clamped.
- `.git` is refused as an authored glob component and skipped when a wildcard
  sweeps it up. A worktree's `.git` file is what makes it a worktree; copying
  the main checkout's over it would break the checkout.
- Symbolic links are refused, at every level, on both sides. A link inside the
  project root can point anywhere on the host, so following one would let a
  recipe copy an arbitrary host file into a worktree by naming a link the
  repository happens to contain. Destinations are additionally written through
  `os.OpenRoot` on both ends.
- A glob that matches nothing (or whose only matches were skipped as unsafe)
  is a hard error. The recipe named a file it expected to exist; a worktree
  silently missing it breaks later, somewhere else.
- Files are written to a unique `.ao-copy-<uuid>` temp name, fsynced, and
  renamed into place. An interrupted copy leaves a temp file, never a
  half-written destination.

## The command environment

Each `run` argv executes with its working directory set to the new worktree and
the app's own environment (PATH and the user's toolchain intact) plus two
variables. They are appended last, so an inherited variable of either name
cannot shadow the real one. `Env` is the sole writer; the reader is the
authored recipe, so this table is the contract.

| Variable | Value |
|---|---|
| `AO_PROJECT_ROOT` | absolute path of the project's main checkout, the tree `copy` globs read from |
| `AO_WORKTREE_PATH` | absolute path of the worktree being set up, also the command's working directory |

They exist because a recipe can name neither checkout on its own: the worktree
path is generated per worktree, and the project root is not the working
directory. Without them the only expressible way to bring `.env` across is a
copy glob, which snapshots the file and then silently diverges from the main
checkout.

A `run` entry is an **argv, not a shell line**. Nothing here is parsed or
expanded. A recipe that wants expansion asks for a shell explicitly:

```json
{"run": [["sh", "-c", "ln -s \"$AO_PROJECT_ROOT/.env\" \"$AO_WORKTREE_PATH/.env\""]]}
```

The Settings editor accepts one command-line string per row and tokenizes it
client-side (`frontend/src/lib/utils/shellArgv.ts`); what is stored and
executed is always the argv.

`AO_PROJECT_ROOT` is deliberately not the same kind of value as the session
contract's `AO_PROJECT` (`internal/aocli/AGENTS.md`), which is a project
**slug**. A setup command runs no CLI and holds no session credential; these
two paths are the whole of its AO_* surface.

## Testing

`t.TempDir` fixtures only; never the user's real config root. Unix shell
recipes (`/bin/sh -c`) are used in the run tests, matching where the backend
actually executes (native macOS/Linux, or the Linux backend under WSL).
