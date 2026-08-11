# internal/gitroot/

Resolves a filesystem path to the **main** git repository root it belongs
to, and enumerates a repository's registered worktrees, by reading git's
own on-disk layout. Stdlib only. **It never spawns git.**

```go
root, ok := gitroot.MainRoot("/home/u/.config/agent-overflow/worktrees/repo/BLITZ-188")
paths, err := gitroot.RegisteredWorktrees("/home/u/repos/repo")
```

## Why it is not `internal/git`

`internal/git` is the subprocess layer: every answer there costs a `git`
invocation. This question is asked once per session cwd during a session-import
scan — ~120 distinct cwds across ~1600 rows in a listing the user expects to be
instant — and it is answerable from three tiny files. Keeping it in its own
stdlib-only package is what makes "no subprocess" structural rather than a
comment: nothing here *can* shell out, and a caller acquires no path to `gh` /
`glab` by importing it.

## `MainRoot` is `--git-common-dir` semantics, not `--show-toplevel`

That distinction is the whole reason the package exists.
`git rev-parse --show-toplevel` run inside a LINKED WORKTREE answers with the
**worktree's** root, so resolving a project that way gives every worktree a
project of its own — named after a branch. A project is the repository; a
workspace is where the provider operates (root `AGENTS.md` core principle 7).

The walk, from the cwd upward, at each ancestor:

| `.git` entry | Answer |
|---|---|
| directory | This ancestor. It is a primary checkout, so `.git` IS the common dir. |
| file with `gitdir: <path>`, **confirmed** (below), and `<gitdir>/commondir` exists | The commondir's parent — but only when the common dir is named `.git`. |
| file with `gitdir: <path>`, **confirmed**, no `commondir` (git < 2.13) | The segment before `/.git/worktrees/` in the gitdir path. |
| file that exists but cannot be interpreted | This ancestor. **The walk stops** — see below. |
| no `.git` entry | Keep walking up. |
| anything else | This ancestor. |

## The repository has to confirm the worktree

Git maintains the worktree link in BOTH directions: the worktree's `.git` file
names its private gitdir, and the registration carries a `gitdir` file naming
that worktree's `.git` file back. Both pointer resolutions above run on that
back-pointer first (`registrationNamesWorkTree`).

Without that check, resolving is spoofable and the answer is not cosmetic: a
resolved main root becomes an **auto-created project row at that path**. The
commondir path needs two files anybody can write (a `.git` pointer plus a
`commondir` naming the target); the pre-2.13 fallback needs ONE, naming a
gitdir that need not even exist, as long as its path contains
`/.git/worktrees/`.

The back-pointer alone is not enough on the commondir path, because an author
who controls the registration directory controls both files in it — a complete
fake registration with a correct back-pointer can still point `commondir` at
anyone. So the commondir must also be **the directory that physically holds
the gitdir** (`commonDirHoldsGitDir`: every layout git writes keeps a linked
worktree's private gitdir at `<commonDir>/worktrees/<name>`, and the commondir
file is literally `../..`). Bound this way, a resolved main root can only ever
be a path that contains the registration — and whoever can write there owns
that path already. The marker fallback gets the same property for free: it
derives the root from where the gitdir physically sits.

A missing or mismatched back-pointer, or a redirecting commondir, is not an
error — it degrades to "this checkout is its own root", the same answer a bare
or submodule layout gets, and the honest answer for a worktree someone moved
by hand (the state `git worktree repair` exists to fix). Both sides of every
comparison are symlink-resolved, because macOS `/tmp` is `/private/tmp` and a
lexical compare would refuse every real worktree under one.

## An uninterpretable `.git` entry STOPS the walk

"No `.git` entry" and "a `.git` entry that cannot be interpreted" are different
answers, and only the first may continue upward. A corrupt pointer, an empty
file, a socket, or an entry that cannot be read is where git itself stops and
errors — walking past it hands the checkout to whatever repository happens to
contain it, which is the same false attribution the missing-path refusal below
exists to prevent.

The `.git`-named check on the common dir is load-bearing. Two layouts reach it
and neither has a main working tree to name: a **bare** repository
(`repo.git`), and a worktree of a **submodule** (`<super>/.git/modules/<name>`).
Their parent directories (`/path`, `<super>/.git/modules`) are not checkouts, so
the answer falls back to the checkout in hand — which is also what
`--show-toplevel` answers there. A plain **submodule** lands on the same
fallback from the other direction: its gitdir is a repository of its own and
carries no `commondir`.

## `MainRoot` refuses a path that does not exist

Deliberately, and it is not a degrade. A deleted worktree cannot be walked, and
walking up from it *lexically* attributes it to whatever repository happens to
contain its parent — a dotfiles repo in `$HOME` is enough to make that wrong.
Dead paths are answered from the repository side instead, by
`RegisteredWorktrees`.

## `RegisteredWorktrees` is the dead-path half

`<commonDir>/worktrees/*/gitdir` holds the path of each registered worktree's
own `.git` file; its parent is the worktree. The registration outlives the
directory until someone prunes it, which makes it the only thing that can still
place a session whose worktree the user deleted.

The path may be **relative to the registration directory** — git ≥2.48 writes
it that way under `worktree add --relative-paths` /
`worktree.useRelativePaths`, the same rule the `.git` pointer and `commondir`
files follow. It is resolved before the `.git`-name check; left relative it
could never match an absolute session cwd, so every deleted worktree of such a
repository would silently lose its grouping.

Two rules callers depend on:

- Returned paths are **cleaned but not symlink-resolved.** They are usually
  gone, so there is nothing to resolve, and the lexical form is what both git
  and the provider that ran there recorded.
- "No registry" is **not an error.** A repository with no worktrees, a root
  that is not a checkout, and a path that does not exist all return no paths
  and no error. Only a registry directory that exists and cannot be read is an
  error — the session-import scan calls this once per project row, and a
  project on a stale network path must not fail a listing.

Everything else is symlink-resolved (`canonical`), because AO's project rows
are stamped with canonical roots and macOS `/tmp` is `/private/tmp`.

## Anti-patterns

- Do NOT add a `git` subprocess here. If an answer genuinely needs one it
  belongs in `internal/git`.
- Do NOT make `MainRoot` fall back to a lexical walk for a missing path. See
  above — the false positives are silent and wrong.
- Do NOT read a pointer file unbounded. `pointerFileLimit` exists because
  these names are read once per session cwd and hold one short path.
- Do NOT open a pointer path without the regular-file screen. A FIFO named
  `commondir` or `gitdir` blocks `os.Open` until someone opens the other end,
  and this resolver runs inside a scan with no cancel path — one wedges the
  listing and every thread creation behind it.
- Do NOT resolve a pointer-kind `.git` without the back-pointer check, and do
  NOT turn a failed check into an error. Both properties are load-bearing; see
  above.

## Testing

Layouts are hand-written into `t.TempDir()` — the file contents git writes
are exactly what the resolver reads, and writing them directly pins those
contents while spawning nothing. That includes the layouts git would never
write: a spoofed pointer, a mismatched back-pointer, a corrupt `.git` file.

`gitroot_fifo_test.go` is `!windows` (no `mkfifo` there) and wraps each call in
a watchdog, because the defect it guards is a hang — a failed guard must be
reported, not left to time out the whole package run.

## Consumers

- `internal/project` (`EnsureForWorkspace`) — which project a workspace's
  thread lands in.
- `internal/sessionimport` (`projectIndex`) — which project a scanned
  session's cwd is grouped under, including via `RegisteredWorktrees` for
  deleted worktrees.
