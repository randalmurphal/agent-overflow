# internal/shellenv/

Startup-time probe that asks the user's login shell what `PATH` looks
like, then merges any new entries into the running process's
`os.Environ()`.

## Why it exists

When the binary is launched outside an interactive terminal — the
WSL-side backend spawned by `wsl.exe -d <distro> -- <bin>`, a macOS
`.app` double-clicked from Finder, a Linux desktop entry — the
inherited `$PATH` is the OS's minimal default, not the `$PATH` the
user's shell rc files build. As a result `exec.LookPath("claude")`
misses anything installed via:

- `nvm` (`~/.nvm/versions/node/v*/bin`)
- `asdf` (`~/.asdf/shims`)
- `volta` (`~/.volta/bin`)
- npm with custom prefix (`~/.npm-global/bin`)
- user-local installs (`~/.local/bin` — the `pip --user` and the
  Anthropic Claude Code installer location on Linux)

A startup probe that runs `<user-shell> -ilc 'printenv PATH'` and
merges the result fixes this everywhere at once. No hardcoded paths.

## Public API

```go
shellenv.Sync(ctx)
```

That's it. The function:

1. Picks shell candidates: `$SHELL`, then platform-default
   (`/bin/zsh` on darwin, `/bin/bash` everywhere else).
2. Runs the first candidate as `<shell> -ilc 'echo …; printenv PATH;
   echo …'` with sentinel markers bracketing the value.
3. Extracts the bracketed PATH from stdout (banners / MOTDs / etc.
   before/after are ignored).
4. Merges the captured PATH with the inherited PATH — login-shell
   ordering wins, duplicates are dropped, empty entries are skipped.
5. Calls `os.Setenv("PATH", merged)` if anything changed.

Errors are best-effort. Any failure (no shell, shell exited non-zero,
sentinel markers missing, etc.) returns an error but leaves PATH
untouched. Callers log the error and proceed — provider detection
will surface a "binary not found" status banner if PATH genuinely
lacks the binary.

## Layout

- `shellenv.go` — public `Sync` entry, no build tag (so callers
  always link). Delegates to platform-specific `doSync`.
- `shellenv_unix.go` (`!windows`) — implementation: shell candidate
  selection, the `-ilc` probe, sentinel parsing, PATH merging.
- `shellenv_windows.go` (`windows`) — stub that returns nil. The
  Windows `.exe` is the launcher in `cmd/agent-overflow-windows`; it
  never spawns provider children, so there's nothing to probe.
- `shellenv_unix_test.go` (`!windows`) — pure-helper tests
  (`mergePath`, `extractPath`, `candidateShells`) plus a fake-shell
  fixture that exercises the full `probe` / `Sync` round-trip without
  depending on bash being installed or having nvm configured.

## Why `-ilc` (not `-lc` and not `-c`)

- `-l` (login) sources `/etc/profile`, `~/.bash_profile`, `~/.profile`.
  Catches `~/.local/bin` additions and similar profile-only edits.
- `-i` (interactive) sources rc files: `~/.bashrc`, `~/.zshrc`. Nvm in
  particular installs into `~/.bashrc` and only adds its bin dir to
  PATH when `nvm.sh` runs from there. Without `-i`, nvm-managed PATH
  is invisible.
- `-c <script>` is how we deliver the probe.

`bash`, `zsh`, `dash`, `ksh` all accept `-ilc`. `fish` does not — its
short flags differ. A user with `SHELL=fish` will see the primary
shell fail and the candidate loop fall through to `/bin/bash` (which
in their setup probably doesn't have nvm, but at least it doesn't
break). The Settings → Provider Binaries override is the explicit
escape hatch when an unusual shell setup defeats the probe.

## Why sentinels (and not just `printenv PATH`)

Login + interactive shells write banners, MOTDs, `~/.bashrc` debug
echoes, deprecation warnings, and so on. The captured stdout will
have all of that mixed with the value we want.

The probe brackets the value with `__AO_SHELLENV_PATH_START__` /
`__AO_SHELLENV_PATH_END__` sentinels and `extractPath` slices between
them. Anything before / after / completely-absent sentinels yields a
clear error.

T3-code uses the same pattern (see
`/Users/randy/repos/t3-code/packages/shared/src/shell.ts`); the
namespacing convention there is `__T3CODE_*`, ours is `__AO_*`.

## Anti-patterns

- Do NOT add fallback paths like `~/.nvm/versions/node/*/bin` here.
  That's the hardcoding the user explicitly called out as wrong; the
  whole point of probing the shell is to avoid it.
- Do NOT make `Sync` block startup on shell failure. The 5 s timeout
  is a hard cap; everything past that returns an error and continues.
- Do NOT extend the captured env beyond `PATH` without a concrete
  case. T3-code captures `SSH_AUTH_SOCK`, `HOMEBREW_*`, `XDG_*`; if
  any of those are needed here, add them with a justification rather
  than upfront.
- Do NOT call `Sync` from anywhere except `main()` once. Repeated
  calls would re-merge an already-merged PATH (idempotent, but
  wasteful) and fork a shell on every call.

## References

- `/Users/randy/repos/t3-code/apps/desktop/src/syncShellEnvironment.ts`
  — reference implementation we're aligning with.
- `internal/provider/detect.go` — the consumer most affected by this:
  `DetectProvider` calls `exec.LookPath` against a settings-supplied
  binary name.
