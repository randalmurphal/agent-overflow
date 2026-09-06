# Commands on connected computers

A conversation stays on its current computer while selected commands run in a
registered project on another one. For moving the whole conversation, use
Move or Copy instead; that transfers provider history and optionally the
workspace through the separate conversation-transfer protocol.

## Setup

Connect both computers in Computers. In Settings, select the originating
computer, open Remote access, and use **Agent access to other computers**.
Choose the destination and enable access. The frontend mints a destination
invitation, enrolls the originating computer, compares both verification
numbers and the destination identity, then confirms and enables commands.
Each computer retains its own device key and rotating credentials. Pairing a
phone to both computers does not silently give either computer the phone's key.

This is an ordinary full device pairing between computers you control. The
agent toggle separately controls whether new peer commands can start. Disabling
it leaves the pairing intact and allows status/cancel for previously accepted
jobs. Revoke the originating device in the destination's Devices list to end
its device access. Confirmation and enrollment retain the usual step-up checks.

Enabling verifies the destination's connection and command permission first.
An unavailable, unconfirmed, incompatible, or view-only pairing cannot become
enabled. Disable works offline. If a reply was lost during pairing, try Enable;
if it fails and the frontend can reach the destination, Connect again repairs
the pairing. An incomplete saved profile keeps that recovery path after closing
settings. Existing computers appear once; the add selector lists new peers.

## Agent use

The source backend checks explicitly enabled peers independently of any open
frontend. When a reachable peer has registered projects, Claude and Codex get
additional guidance at their existing safe session-configuration boundary.
The guidance disappears after no usable enabled peers remain. Discovery is a
periodic hint; every command rechecks the actual authenticated connection.
Workflow phases must additionally declare the `remote-commands` grant in the
frozen workflow definition. Claude TUI receives no extra prompt.

The guidance documents `agent-overflow remote list`, `remote run`, `remote
status`, and `remote cancel`. Run accepts an exact argument vector after `--`;
there is no implicit shell interpolation. Use `remote --help` for current
flags and limits. Destination project IDs and workspace paths come from that
computer. It has its own files, account environment and installed programs.
Commands receive no source AO session credentials.

Run prints its request UUID before sending and returns a durable receipt.
After a lost reply, query that UUID or retry the identical request with `--id`.
Changing the UUID can start another command. The destination persists acceptance
before spawning and refuses a changed request under an existing UUID.

A phone disconnect or source-backend restart does not cancel an accepted job.
Status and cancellation belong to the original source conversation and the
paired source device. The destination bounds active processes, run duration
and retained output; the output is a tail, with truncation reported explicitly.
After a destination restart, unfinished receipts say interrupted and are never
automatically rerun. Buffered output may be lost in a crash; the acceptance
receipt survives. Old output can expire while its receipt remains.

## Maintenance and verification

`internal/remotejobs` owns processes and durable receipts; `attachedbackends`
reuses its existing credential owner and pinned transport. `app_remote_jobs.go`
binds authenticated ownership and workspace resolution. Provider adapters append
the optional instructions without replacing native/custom instructions.

The source-to-destination tests exercise actual pairing, TLS, authorization,
lost-reply retries, opt-out, revocation and discovery with injected job runners.
The browser tests exercise both directions of pairing, identity mismatch and
reloading incomplete pairings. They execute no real provider or GPU workload.
Update admission must count remote jobs, including a completed process whose
result is still awaiting persistence, before treating a destination as idle.
