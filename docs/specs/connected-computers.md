# Connected computers

Approved product direction, 2026-09-05. This document supersedes conflicting
home-only settings, fixed thread ownership, phone tailnet-only access, and
machine-label decisions in `remote-access.md`. Implementation status is tracked
below; a requirement is not evidence that it has shipped.

## User contract

One frontend can work with several computers. Each frontend owns its appearance,
interaction preferences, connection catalog, and last execution target per
repository. Computers own provider accounts, execution, checkouts, and hosting
configuration. A computer's settings can be selected and edited remotely. A
settings edit captures its destination before starting asynchronous work.

The first paired host has no special product authority. Offline hosts stay
remembered; other hosts remain usable. A phone closing or losing its network does
not stop work. Access, execution ownership, and route selection are independent.
Discovery suggests a computer; pairing grants access; the receiving computer
checks authorization on every operation. Knowing another host's name is not an
authority to execute there.

## Minimal UI

- Computers is the connection destination. Add by invitation/QR or desktop SSH
  setup. Each named computer exposes status, configuration, access, and updates.
  Ports, certificate files, and troubleshooting belong in expanded details.
- Settings distinguish **This device** from **Computer: <name>**. Frontend
  preferences keep working offline and remain identical when switching hosts.
- The composer remembers the last chosen computer per frontend and repository.
  An unavailable choice stays selected; no operation silently moves to another
  machine. Existing-thread selection offers Move or Copy.
- Add project selects a computer and browses that computer's existing folders;
  registering an already-cloned repository never requires local filesystem
  access or a shell command. Capture the host across browse and registration.
- Sidebar machine names share the existing worktree metadata line. Remote
  desktop threads identify their host even if the repository exists only there.
  A phone with several hosts identifies all of them. No new attention feed.
- Dev-server and generated HTML links open a preview on the owning host with
  its related assets. No separate artifact dashboard or model-written work log.
- Agent remote tools are optional. Advertise them only when enabled and usable
  peers exist; changes in availability reconcile with supported provider tool
  refresh boundaries. Existing jobs stay inspectable during a disconnection.

## Connections and hosting

A computer has stable identity and one or more verified routes. LAN and tailnet
are routes to that identity, never duplicate computers. Route changes preserve
pairing, thread state, and queued request identity. Plain LAN access on Android
must work without weakening all WebView origins or bypassing transport auth.
Private certificates need explicit trust bound to the paired host.

An occasional GPU host can start with one command or a saved desktop SSH
connection. Persistent hosting is offered during setup; disconnecting a setup
client must not stop a separately installed service. Existing SSH host-key checks
and credential agents remain authoritative. Credentials are never placed in
logs, generated shell arguments, or provider prompts. The UI describes whether
hosting survives logout and reboot on each platform.

## Conversation portability and remote execution

The operation contract and native validation evidence are detailed in
[conversation-transfer.md](conversation-transfer.md).

**Move** preserves the AO conversation identity and changes its sole execution
owner. **Copy** creates a new conversation with provenance. **Run elsewhere**
creates a task on another computer whose result belongs to the requesting thread.
UI and agent tools call the same application operations through transport.

Transfer provider-native resumable history, relevant AO metadata and attachments,
and explicitly selected workspace changes. SQLite display history alone is not
the resume format. Provider adapters remain provider-specific. Validate target
provider compatibility, account availability, repository identity, and workspace
state before activation. Never rewrite arbitrary historical prose as paths.
Running processes and pending approvals are not portable transcript content.

Moves occur at a quiescent boundary. Durable transfer IDs make retry/status
queries idempotent. Prepare the destination, durably retire the source's right to
execute, then activate the destination. A lost reply leaves a recoverable pending
transfer, never two writers. Retain source recovery data until confirmation.
Stale clients must learn the new owner rather than dispatching to a fallback.
Copies have distinct identities and usage provenance prevents historical usage
from being counted twice.

Remote commands use existing process/provider execution mechanisms and bounded
output, not a second agent orchestrator. The initiating model chooses the task;
AO routes and tracks it. Peer grants, destination workspace, and stable request ID
are checked by the destination. Jobs survive loss of the requesting frontend.
Moving execution does not change a cloud provider into a locally hosted model.

## Compatibility and recovery

Additive contracts and per-host feature advertisements are the default. Support
at least 90 days of stable releases once a compatibility baseline is released.
Test current clients against the supported oldest server and current servers
against released clients, as well as mixed hosts in one frontend. A breaking
protocol change needs an explicit floor and an update/recovery path independent
of the incompatible feature. Old hosts may lack a feature without losing chat.

Phone bundle selection must consider connected-host compatibility as well as
native shell requirements. Preserve a working bundle after download or startup
failure. Server updates stage and validate before replacing a running version,
wait for active work by default, and report the committed version or rollback
after reconnect. macOS bundles and Windows/WSL are production targets too.

Requests with side effects carry stable IDs wherever automatic retries are
allowed. An absent acknowledgement is not proof of failure. Reconcile acceptance
before retrying; never duplicate a turn, transfer, or remote job. Drafts survive
app termination. Offline work remains visibly unsent and retains its target.

## Acceptance matrix

Exercise desktop, compact browser, and Android with two isolated real backends
and mocked providers. No automated test uses live provider credentials.

| Area | Required evidence |
|---|---|
| Preferences | Separate clients differ; switching/removing/offlining the first host preserves preferences; reload restores them. |
| Host configuration | Two hosts with different settings/accounts; edits and event echoes stay on the captured host; denied/offline hosts cannot redirect a write. |
| Selection | Per-project remembered targets survive restart; remote-only projects identify host; offline targets do not fail over. |
| LAN/tailnet | Signed APK on private LAN and trusted HTTPS; route changes, address changes, expired sessions, revocation, and blocked routes. |
| Startup | Fresh headless setup, existing service adoption, SSH disconnect/reconnect, platform service behavior. |
| Transfer | Claude/Codex move and copy; divergent checkout, attachments, provider mismatch; failures before/after ownership commit and repeated requests. |
| Tools | Disabled/single-host sessions omit tools; peer arrival/removal; authorization and cancellation; bounded long output. |
| Updates | Old/new client-host combinations; interrupted download; trial failure and DB restore; real production artifact restart. |
| Preview | Dev server HTTP/HTTPS, WebSockets/HMR, assets/redirects, generated HTML assets, permission denial, server restart, revocation. |
| Weak network | Lost send acknowledgement, repeated disconnects, completion while offline, app termination, re-pair, and host restart. |

## Delivery status

Core workflows are implemented and have local acceptance coverage with isolated
computers and mocked providers. Platform release acceptance remains separate.

- [x] Frontend preference persistence and per-host settings/accounts/usage.
- [x] Computers UI, host labels, and remembered project targets.
- [x] LAN trust/routes and simple headless/SSH onboarding.
- [x] Provider-native conversation move/copy and optional peer tools.
- [ ] Mixed-version gates, remote update coverage, previews, and failure testing.

Only check a row after implementation and relevant validation. Record remaining
physical-device or external-service limitations explicitly at handoff.

The remaining update/release row includes released old/new client fixtures once
the first connected-computers baseline ships, signed APK acceptance on physical
LAN/tailnet devices, actual platform service installation, and platform release
signing/update handoff. Current production-build trial/restart coverage is
recorded below; it does not substitute for those platform checks.

Peer-command operation and failure semantics are documented in
[remote-commands.md](../architecture/remote-commands.md). Remote service-update
preparation and waiting are cancelable before supervisor handoff. The staged
update now waits on existing work owners and fences admission atomically;
focused tests cover queue/echo handoffs, workflow resume, cancellation and
supervisor reply loss. The complete mixed-platform update path remains part of
the unchecked update acceptance row above.

Restart admission also covers sign-in session lifetimes and credential-changing
account operations, including background refresh, plus worktree setup and
session-import jobs. The same leases overlap thread/worktree creation and async
setup kickoff. Isolated tests cover restart refusal, cancellation, failed starts
and lease release after cleanup/publication. Notification failures retain an
explicit retry action; reconnection alone never changes the visible conversation.

Supervisor trial admission now holds all client routes except read-only health
until commit, including ticket minting and credential rotation. Transient HTTP
failures preserve Go-client pairings. Both boundaries have focused regression
coverage. Mixed-platform and end-to-end update validation remain open above.

Session renewal now saves a successor before sending and recovers a committed
operation after a lost response or restart. Its separate endpoint prevents an
older host from silently consuming an unsupported recovery request. Go HTTP
and real browser tests (desktop and compact) cover response loss and client
restart; profile/lease tests cover stale replies, independent processes,
unknown fields and revocation. See
[session-renewal.md](../architecture/session-renewal.md).

Go carriers and supported Android shells now learn bounded HTTPS alternatives
from authenticated manifests, verify identity before switching, and preserve
pairing across listener changes. Go integration tests cover real TLS/WS proxy
switches and lost renewal replies; the Android smoke removes its original
connection, renews over the advertised LAN route, and uploads an attachment.
See [computer-routes.md](../architecture/computer-routes.md). Signed release
APK and real-device route acceptance remain part of the unchecked matrix above.

Paired desktop `--connect` now boots an independent local frontend controller,
with its own persistent identity, origin and presentation files. It does not
start an execution backend or probe a host to open. Real desktop and compact
browser tests cover restarting it with its launch computer offline, removing
that pairing, retaining frontend theme preferences and opening the other
computer's conversation, for both shared and separate repositories. Local
highlighting and wire signatures have Go coverage. Legacy token attachment
remains a single-upstream relay.

Provider-native transfer browser tests now cover Claude and Codex copies and
moves, including attachments and untracked workspace files, while the requesting
frontend disconnects. The Codex test supplies an isolated native-path probe
because the mock provider has no durable session index; it does not execute a
real model. Reverse-ID timeline pages exposed a history-export ordering bug;
metadata-only transforms now cannot change item identity or ordering.

The desktop SSH dialog has also passed against an owned real loopback OpenSSH
server with temporary keys, strict host-key checks and an executable path with
spaces. Pairing completed and the remote project's sidebar entry loaded; closing
setup left the execution host available. This validates SSH transport and the
pairing console, not installation into a remote machine's service manager.

The real browser flow now enables agent access in both directions between two
independent harness hosts, including enrollment from an attached computer back
to HOME and its step-up checks. Disabling one direction leaves the other enabled.
No provider tokens or GPU commands are used by this gate.

Standalone frontends now share the desktop updater and reopen as `--frontend`,
without replaying a pairing invitation or naming a removed computer. The shared
Wails fix preserves launch arguments through update and rollback and clears
helper metadata on both paths. Startup/helper tests, updater race tests and
local-controller wire checks pass; actual signed-artifact replacement remains
in the update acceptance row above.

The general computer choice is now persisted per frontend in addition to the
per-repository target. Review companions retire captured state when a conversation
changes owner or checkout while keeping its ID. Native browser hydration refuses
remote and ambiguous owners and excludes a frontend-only controller. Targeted
store/component tests cover these ownership edges.

Phone and browser appearance files now live in their own bounded library, with
one-time legacy migration and explicit copy-from-computer controls. Cold reloads
preserve selections; copying files never adopts another frontend's preferences.
The paired desktop/compact browser flows exercise real PNG decoding, import,
host file changes, and reload. Unit coverage includes storage quota/timeout,
cross-tab invalidation, removed hosts, and decoded-image memory limits. The cold
load caught and fixed an existing selection-validator initialization-order bug.

Preview browser grants now survive the app connection disappearing. The full
desktop/compact gateway flows pass, including disconnection, revocation and
stopping the share. Generated HTML previews now run at a separate origin with
relative assets and the same authorization; desktop and compact browser tests
exercise real page scripts, CSS, reload, service-worker refusal and revocation.
See [file-previews.md](../architecture/file-previews.md) for lifetime and browser
certificate limits. Sharing-policy changes also retire existing network preview
listeners; rebind alone previously left those independent listeners open.

Lost-send-reply browser tests now cover one and two consecutive disconnects on
desktop and compact layouts. Retries retain one send identity and create one
persisted user message and provider turn. Duplicate detection now uses sparse
indexes over retained history (v89), replacing the 64-message cutoff that could
lose an accepted send during a longer outage. Workflow resources also distinguish
unknown ownership from HOME, so standalone frontends can resolve a sole computer
or display an ambiguity error instead of waiting for a nonexistent connection.

The production-artifact gate now passes from schema v88 to v89 using two actual
macOS builds, both as executables and as ad-hoc signed application ZIPs. Trial
commit and a subsequent cold restart retain backend identity, SQLite data and
database integrity. It exposed an inherited instance-lock descriptor that could
block restart while a helper survived; Unix locks now use atomic close-on-exec,
with a real-child regression test. Async final path enrichment also no longer
recreates a thread's live caches after cleanup.

The same artifact gate also passes against the final `make build` macOS bundle,
including production build tags, bundled resources and ad-hoc signing, packaged
from a separate checkout without replacing the running desktop host.

The signed, non-debuggable Android shell build 3 has passed a fresh Android 36
Pixel 9 emulator check: paste an invitation, compare and confirm the pairing
number, unlock with the platform PIN, open the remote project/thread, then
force-stop and cold-start the APK. Pairing persists and changes made on the host
while the app is closed appear after unlocking. This uses the real release
network policy and the app's ordinary private-LAN certificate path, with an
isolated host and mocked providers. The first-run screen now offers **Use a
link** alongside scanning. Physical Pixel and real tailnet acceptance remain
separate from this emulator result.
