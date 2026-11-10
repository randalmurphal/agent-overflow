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
  completed block inside a chat surface, to a session granted both scopes
  the run needs (`data-shell-runnable` on the surface).
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
  repeated request is answered from the running command, the message it
  became, or how the run settled (the service remembers its last 256), so
  it neither runs the command twice nor sends twice. That makes the method
  an entry of the transport's `RETRY_ON_TRANSIENT_CLOSE`, and a client
  whose start reply was lost asks again with the same run id.
- A command is stopped after one hour and delivered as timed out.
- Output is captured to 8 MiB from the start plus the last 64 KiB, with a
  marker for what was dropped. The process keeps draining past the cap.
- When the command's shell exits, processes it left in its group are
  stopped and the output ends with a line saying so. A command the kill
  cannot end (it runs as another user) fails with a message that it is
  still running.
- Deleting the thread, removing a worktree (every command running in or
  below it) and shutdown stop the run and join it. A stopped run sends
  nothing.
- A running command holds back an app update, and keeps an empty draft
  thread from being deleted.

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
reaches every client. The output file is kept only when the send recorded
a message that owns it.

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
whose invocation is not in the text follows it. Which parts those are is
one rule (`usermessage.ShellInvocationRuns`) with a frontend copy that
hides the same parts behind the row's command block; both run one set of
shared vectors. A command that holds the join rule is a run of several
parts. A message that is exactly an invocation is never expanded as a
composer command, and never names a temporary worktree branch.

| Provider | Block |
|---|---|
| Claude | `<bash-input>cmd</bash-input>` then `<bash-stdout>` and `<bash-stderr>`, as its own `!` mode records. Success puts the output in stdout. Failure puts the output and `Exit code N`, a timeout line or a signal line in stderr. Output is XML-escaped; the command is not. |
| Codex | `<user_shell_command>` with `<command>` and `<result>` holding `Exit code`, `Duration` and `Output`, as its rollout records one. A timeout is exit code 124 plus a line saying so. |

The model reads up to 30,000 bytes of output inline. Longer output is the
first and last 10,000 bytes around a line that gives the omitted size and
the path of the whole file. Terminal escape sequences are removed and a
lone carriage return becomes a line break; the stored file keeps both.

Claude's fork-point walk skips the CLI's own `!` records, which are not
prompts. The block this app sends joins command and output in one record
(`sessionfork.HostShellRecordJoint`), and the walk counts that record as
the prompt it is.

## Screen

- Shell mode is a draft that starts with `!` and holds no command output.
  The `!` is accented, the text is monospace, a hint names the workspace
  (the new worktree when one is chosen and not yet made), and Send is the
  `run` action. Slash and mention completion are off inside a command.
- Only the text is consumed. Attachments and terminal chips stay. A
  refused command's text goes back ahead of anything typed since.
- Enter runs the command that was in the box when it was pressed. Text
  that changed while the workspace was prepared is neither run nor sent.
- A running command is a card above the composer with its last six lines,
  the elapsed time and Stop. Nothing is added to the timeline until it
  exits. Stop and Dismiss hand focus to the composer.
- The message's row is a command block: the command, how it ended, the
  duration and the preview. Opening it reads the output in 256 KiB pages
  (`ReadShellAttachmentOutput`, `threads:read`). Whether it is open is the
  pane's row state for that message and attachment, so it survives the
  row leaving the virtual window.
- The card and the block show output as a terminal leaves it
  (`shellDisplayText`): cursor control is dropped, a redrawn line is its
  latest frame, and color is kept.
- A message taken back to the composer shows the output as a chip and is
  sent again as a message; the command is not run twice.

## Verification

`e2e/tests/composer-shell-command.spec.ts` covers the flow against the real
backend: the wire block, the live card, reload during a run, Stop, a
command queued mid-turn, a command as the first message of a new thread
and Run on a code block.
