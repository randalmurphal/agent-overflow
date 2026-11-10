# Composer shell commands

A person runs a shell command from a thread and its output goes to the agent
as a user message. Two entry points share one path: a composer draft whose
first character is `!`, and Run on a `sh`, `bash`, `zsh` or `shell` code
block in a chat. Implemented 2026-11-10.

## Decisions

- The app runs the command for every provider. Codex's native
  `thread/shellCommand` never starts a model reply, so it is not used.
- A finished command is always sent. A command the agent should not see
  belongs in a terminal.
- Stop discards the run: nothing is saved and nothing is sent.
- Run on a code block runs the whole block on click. It is offered only on a
  completed block inside a chat surface.
- The runtime mode governs the agent, not the person: a command runs
  unsandboxed with the person's environment in every mode.
- History the Claude TUI wrote with its own `!` mode is not converted.

## Run

`internal/shellrun` owns the run; `internal/app/app_shell_command.go` owns
the bound methods and what a result becomes.

- `RunThreadShellCommand` requires `terminal:operate` and `threads:operate`
  and the same autonomy check as a send. A terminal thread is refused.
- The command runs as `$SHELL -c <command>` (`/bin/sh` when unset) in the
  thread's workspace, with the app's environment minus AppImage launch
  variables and `AO_*`, stdin closed, no PTY, and a fresh shell each time.
- One command runs per thread. A second start returns `shellrun.ErrBusy`.
- The run id is client-minted and is the send id of the resulting message. A
  repeated request neither runs the command twice nor sends twice.
- A command is stopped after one hour and delivered as timed out.
- Output is captured to 8 MiB from the start plus the last 64 KiB, with a
  marker for what was dropped. The process keeps draining past the cap.
- Deleting the thread, removing its worktree and shutdown stop the run and
  join it. A stopped run sends nothing.

## Stream

`shell:command` carries `started`, `output` and `finished` frames, gated on
`terminal:operate`. Output frames are coalesced on a newline, 8 KiB or
100 ms, end on a rune boundary, and carry a per-run sequence from 1.
`GetThreadShellCommands` is the snapshot: the running command and the end of
its output with the sequence it reflects. `GetThreadLiveState` names the
running run id without output, so a client that mounts mid-run reads the
snapshot and a client holding a run the backend no longer has drops it.

A `finished` frame is `sent`, `stopped` or `failed`. `failed` means the
command could not start or its output could not be saved or sent; the error
reaches every client and nothing is left behind.

## Message

The output is stored as a `file` attachment whose row carries
`store.AttachmentShell` (command, exit code, duration, truncation, timeout
and the first lines as a preview). The message is an ordinary send with that
attachment and the text the person typed, `!<command>`, so queueing while a
turn runs, joins, retry, restore to the composer, fork and transfer are the
send path's own.

On the wire the typed line is replaced by the command block
(`turnAttachments.promptText`): each part of the text that is exactly an
attached command's invocation becomes that command's block, and a block
whose invocation is not in the text follows it. A message that is exactly
an invocation is never expanded as a composer command.

| Provider | Block |
|---|---|
| Claude | `<bash-input>cmd</bash-input>` then `<bash-stdout>` and `<bash-stderr>`, as its own `!` mode records. Success puts the output in stdout. Failure puts the output and `Exit code N`, a timeout line or a signal line in stderr. Output is XML-escaped; the command is not. |
| Codex | `<user_shell_command>` with `<command>` and `<result>` holding `Exit code`, `Duration` and `Output`, as its rollout records one. A timeout is exit code 124 plus a line saying so. |

The model reads up to 30,000 bytes of output inline. Longer output is the
first and last 10,000 bytes around a line that gives the omitted size and
the path of the whole file. Terminal escape sequences are removed and a
lone carriage return becomes a line break; the stored file keeps both.

## Screen

- Shell mode is a draft that starts with `!` and holds no command output.
  The `!` is accented, the text is monospace, a hint names the workspace,
  and Send reads Run. Slash completion is off inside a command.
- Only the text is consumed. Attachments and terminal chips stay.
- A running command is a card above the composer with its last six lines,
  the elapsed time and Stop. Nothing is added to the timeline until it
  exits.
- The message's row is a command block: the command, how it ended, the
  duration and the preview. Opening it reads the output in 256 KiB pages
  (`ReadShellAttachmentOutput`, `threads:read`).
- A message taken back to the composer shows the output as a chip and is
  sent again as a message; the command is not run twice.

## Verification

`e2e/tests/composer-shell-command.spec.ts` covers the flow against the real
backend: the wire block, the live card, reload during a run, Stop, a
command queued mid-turn and Run on a code block.
