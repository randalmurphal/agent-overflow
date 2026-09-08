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

- `remote_computers` lists enabled destinations with registered project IDs,
  paths and reachability errors. Discovery does not enable a computer.
- `remote_run` takes a computer ID, destination project/workspace, exact `argv`
  and a caller-chosen UUID `request_id`. There is no implicit shell interpolation.
- `remote_status` reads the receipt and output for that ID.
- `remote_cancel` cancels that job without interrupting the source conversation.

The server advertises tools when paired profiles exist, even if a machine is
currently offline. Destinations still require Agent access opt-in at every start.
Status/cancel remain useful after opt-out. Configuration changes refresh live MCP
discovery; no background network polling or provider restart is needed. A
provider starting during a configuration change refreshes after registration.
Thread toggles last for this app process and survive provider-session restarts,
like the built-in browser toggle; machine opt-ins persist across app restarts.

`remote_run` normally waits up to one second for a result. `wait_seconds` can
request zero (return immediately) through ten seconds on run/status/cancel.
A running receipt is accepted work, not an error or permission to start again.
The separate `timeout_seconds` limits the actual job (one hour by default,
seven days maximum). After a lost reply, query the chosen UUID or retry identical
arguments with that same UUID. A changed UUID can start a second job; a changed
request under the old UUID is refused.

Replies include the destination, receipt state, exit code and a bounded output
tail. `max_output_bytes` defaults to 8192; zero requests metadata only, and up to
131072 retrieves more retained output. `omittedOutputBytes` counts retained
bytes withheld by the reply budget; `truncated` means older output was discarded
at the destination. Increasing the budget cannot recover discarded data. For
large test/build logs, explicitly write a log file on the destination. Each
status call reads the current tail; this is not a cursor-based complete log.

The session-scoped `agent-overflow remote` CLI remains a compatibility entry
point into the same execution/authorization methods. Agents use MCP directly;
they need no repository instructions or knowledge of the CLI.

A phone disconnect or source-backend restart does not cancel an accepted job.
Status and cancellation belong to the original source conversation and the
paired source device. The destination bounds active processes, run duration
and retained output; the output is a tail, with truncation reported explicitly.
After a destination restart, unfinished receipts say interrupted and are never
automatically rerun. Buffered output may be lost in a crash; the acceptance
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
prompt overrides. `threadmcp` owns the shared HTTP guard.

The source-to-destination tests exercise actual pairing, TLS, authorization,
lost-reply retries, opt-out, revocation and discovery with injected job runners.
The browser tests exercise both directions of pairing, identity mismatch and
reloading incomplete pairings. They execute no real provider or GPU workload.
Update admission must count remote jobs, including a completed process whose
result is still awaiting persistence, before treating a destination as idle.
