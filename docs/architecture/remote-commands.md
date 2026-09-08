# Commands on connected computers

A conversation stays on its current computer while selected commands run in a
registered project on another one. For moving the whole conversation, use
Move or Copy instead; that transfers provider history and optionally the
workspace through the separate conversation-transfer protocol.

## Setup

Connect both computers in Settings → Remote access → Connect to a computer. Open
Remote access → Agent access and select the originating computer.
Choose the destination and enable access. The frontend mints a destination
invitation, enrolls the originating computer, compares both verification
numbers and the destination identity, then confirms and enables commands.
Each computer retains its own device key and rotating credentials. Pairing a
phone to both computers does not silently give either computer the phone's key.

This is an ordinary full device pairing between computers you control. The
agent toggle separately controls whether new peer commands can start. Disabling
it leaves the pairing intact and allows status/cancel for previously accepted
jobs. Revoke the originating device in the destination's Paired devices list to end
its device access. Confirmation and enrollment retain the usual step-up checks.

Enabling verifies the destination's connection and command permission first.
An unavailable, unconfirmed, incompatible, or view-only pairing cannot become
enabled. Disable works offline. If a reply was lost during pairing, try Enable;
if it fails and the frontend can reach the destination, Connect again repairs
the pairing. An incomplete saved profile keeps that recovery path after closing
settings. Existing computers appear once; the add selector lists new peers.

## Agent use

Claude and Codex receive the built-in **ao-remote-tools** MCP server, controlled
from the composer's MCP menu. It uses the same provider registration and HTTP
boundary as **ao-browser-tools**. Tool descriptions carry usage; no CLI guidance
is injected into the system/developer prompt. Claude TUI is not supported.
Workflow phases must declare `remote-commands` in their frozen definition.

| Tool | Use |
|---|---|
| `remote_computers` | Enabled computers, execution OS/architecture, Windows host versus WSL, executable paths, registered projects and worktrees. |
| `remote_run` | Exact `argv`, or exact `script` plus explicit `interpreter`; destination project/workspace and a caller-chosen UUID `request_id`. |
| `remote_jobs` | Recover this conversation's job IDs, receipts and completion-queue state (pending first, latest 256 maximum). |
| `remote_status` / `remote_cancel` | Inspect or cancel the original job without interrupting its source conversation. |
| `remote_read_log` / `remote_search_log` | Bounded byte ranges/tails or literal searches of saved output. |
| `remote_fetch_artifact` | Copy one workspace file to a private local path, with size and SHA-256 verification; no inline file bytes in model context. |

Discovery never changes a checkout or enables a computer. Worktree paths are
revalidated at execution. Executable discovery checks PATH without running
binaries; it does not prove their versions or a working GPU. Agents explicitly
run version/GPU probes when relevant. Older peers may omit environment facts.
WSL commands run in Linux even though the host is Windows.

Agents use ordinary Git commands to push/pull changes and create worktrees.
There is no implicit file synchronization, checkout switching, credential
forwarding, or fallback to another machine/directory. Worktrees are useful for
isolated testing but are optional. Commands have the destination account's normal
authority, just like local execution; this is not a sandbox against destructive
commands. Workspace confinement prevents accidental destination selection,
not deliberate escape by arbitrary commands.

Run long-lived commands in the **foreground on the destination**: do not append
`&`, use `nohup`, or otherwise detach from AO's process group. AO backgrounds
the job and manages cancellation. `remote_run` normally waits one second;
`wait_seconds` independently selects zero through ten seconds on run/status/cancel.
The command timeout defaults to one hour, up to seven days. Explicit
`unlimited: true` with no timeout removes that duration limit for training or
services. It does not add reboot survival or automatic restart.

Choose the request UUID before calling. After a lost reply, inspect the same
ID or retry exactly the same arguments with it. Changing the ID can run twice;
changing arguments under an accepted ID is refused. Scripts accept up to 1 MiB,
are written privately with an interpreter-appropriate extension, and are removed
after execution. AO appends the script path to the explicitly supplied interpreter
argv; it never guesses a shell.

The background tray shows jobs and offers Stop plus an on-demand bounded log
view. The source backend tracks outstanding jobs across frontend disconnects
and source restarts. It polls only outstanding jobs, four checks at a time,
with a slower retry after connection errors. An unreachable host is not an
exited process. The agent need not poll merely to discover completion.

A completed job enters the **ordinary message queue**, including normal
interrupt behavior and queued-message presentation. Idle conversations lazily
start a follow-up turn. User drafts remain intact. Source registration precedes
the network mutation; one durable transaction inserts the completion message
and transfers notification responsibility to the queue. Stable send identity
and this handoff prevent repeated observations from injecting duplicates.
`notification: queued` means queue ownership, not proof the agent read it.
After handoff, normal queue crash recovery applies: undelivered text is recovered
into the composer, never independently sent again by the remote-job watcher.
Archived/deleted/moved conversations and finished workflow phases are not
reopened. Transfer waits for outstanding remote work and its notification
handoff, like the existing queued/background-work checks.
Deletion is refused while a remote command is running or its acceptance is
uncertain; stop it and wait for confirmation first. Registration and final
deletion share the thread mutation lock so a concurrent send cannot leave an
orphaned command. Archived threads retain job monitoring but do not start a
completion turn.
Forgetting a computer also waits for outstanding jobs and notification handoff;
reconnect an offline computer and stop its jobs first. Pairing credentials must
remain available while AO still needs to confirm or cancel its work.

Configuration changes refresh live MCP discovery without restarting provider
turns or polling peers for discovery. Thread toggles survive provider-session
restarts within the app process; machine opt-ins persist across app restarts.
Disabling destination agent access blocks new work while preserving owned job
status, cancellation, log reads and artifact retrieval. Revoking the pairing
ends all peer access. A disabled conversation MCP server blocks its tools.

## Logs and artifacts

Output always goes to disk on the destination. Limits are 4 GiB per job and
20 GiB retained logs, with a 256 MiB free-space floor. The disk log keeps the
newest bytes after reaching its cap. Disk failures/gaps are explicit, and the
process output continues draining so a full disk cannot wedge a training job.
Completed logs expire oldest first when reserving space for new jobs; receipt
identity survives log expiry. Memory remains a bounded 128 KiB inline tail per
active job. SQLite keeps 128 recent inline tails for old-client compatibility.

Replies default to 8 KiB of output; `max_output_bytes: 0` requests metadata,
and the maximum is 128 KiB. `omittedOutputBytes` counts retained bytes omitted
from that reply; `truncated` identifies discarded destination output. New peers
also return `log` metadata with absolute `startOffset`, `totalBytes`, and
`retainedBytes`. Log readers use absolute byte offsets and `nextOffset`;
`offset: -1` reads the tail. Literal search scans bounded pages and returns
bounded match contexts. Follow `nextOffset` until `done`; it preserves overlap
for matches spanning pages. Expired logs report `expired`, not empty success.

Artifact retrieval accepts one regular file up to 1 GiB relative to the original
job workspace. Internal symlinks work; paths/symlinks escaping that workspace,
FIFOs/devices and directories are refused. Transfers use 256 KiB chunks, reject
changing files, verify a whole-file SHA-256, and remove incomplete local copies.
Completed copies live under the source data directory's `remote-artifacts/`;
the returned path can be inspected with the agent's ordinary local tools.
Copies are retained until explicitly removed. HTML assets are separate files;
use an explicit archive command when a complete directory is needed.

The session-scoped `agent-overflow remote` CLI remains a compatibility entry
point into the same execution/authorization methods. Agents use MCP directly;
they need no repository instructions or knowledge of the CLI.

A phone disconnect or source-backend restart does not cancel an accepted job.
Status and cancellation belong to the original source conversation and the
paired source device. The destination bounds active processes, run duration
and retained disk output, with truncation reported explicitly.
After a destination restart, unfinished receipts say interrupted and are never
automatically rerun. A process crash during a log overwrite reports the incomplete log explicitly; the acceptance
receipt survives. Old output can expire while its receipt remains.

## Errors and recovery

Expected refusals carry stable codes and actionable prose across the paired
connection: invalid requests, access/ownership refusals, unavailable projects
or workspaces, missing receipts, occupied command slots, and conflicting IDs.
MCP replies include the operation and valid computer/request IDs. Argument
errors identify the field without echoing its value. Raw internal errors stay
in host logs behind a reference; they are never marked as public failures.

A failed network reply does not establish whether a command ran. Inspect its
original receipt, then retry identical arguments with the same ID if needed.
A cancellation reply lost in transit likewise requires status verification.
An older destination may still redact its errors; the source explains that
limitation and preserves the destination's reference instead of guessing why
it refused. Detailed destination errors require updating that destination.

## Maintenance and verification

`internal/remotejobs` owns processes and durable receipts; `attachedbackends`
reuses its existing credential owner and pinned transport. `app_remote_jobs.go`
binds authenticated ownership and workspace resolution. `app_remote_mcp.go`
adapts the same methods to MCP without provider-specific command behavior or
prompt overrides. `threadmcp` owns the shared HTTP guard. `app_remote_watch.go`
coordinates fixed job completion with the existing queue; it owns no provider
state or alternative message placement. `remote_watches` survives history restore
alongside acceptance receipts. The tray projects these records without inventing
provider tool-completion rows in the transcript.

The source-to-destination tests exercise actual pairing, TLS, authorization,
lost-reply retries, opt-out, revocation and discovery with injected job runners.
The browser tests exercise both directions of pairing, identity mismatch and
reloading incomplete pairings. They execute no real provider or GPU workload.
Update admission must count remote jobs, including a completed process whose
result is still awaiting persistence, before treating a destination as idle.

The extended MCP test crosses real HTTP MCP and paired TLS with an injected
runner: scripts/unlimited lifetime, retry identity, full logs, search, artifacts,
opt-out and conversation ownership. Watch integration tests use real mocked
Claude/Codex subprocess adapters for queued dispatch, provider echoes and idle
startup. Store tests cover atomic handoff/rollback and history restore. Browser
tests exercise narrow/wide log views; tray lifecycle tests cover lost events,
reconnection, stale replies and listener disposal. These do not prove a particular
GPU driver, real provider account, or physical Windows network is working.
