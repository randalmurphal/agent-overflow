# In-app updates: trial and rollback

Status: the Windows launcher and WSL payload are implemented. The macOS and
Linux desktop helper and serve's trial budget are not. Decisions are listed at
the end.

An in-app update on macOS, the Linux desktop or Windows (the launcher and its
WSL payload) must leave the previous version and its database in place when
the new version cannot open or migrate the database. This spec applies the
serve supervisor's update cycle
([Serve mode](../architecture/serve-mode.md#the-update-cycle)) to those
platforms and changes the serve trial budget. Remote triggering and its trust
model stay in
[remote access §7](remote-access.md#headless-serve-mode-and-remote-update).

## Scope

| Path | Behavior |
|---|---|
| Desktop app (macOS, Linux) that owns its backend | Trial and rollback (this spec) |
| Windows launcher and WSL payload | Trial and rollback (this spec) |
| Supervised serve host | Existing cycle; the trial budget changes |
| Desktop window attached to a running local service | Existing Wails swap; the service owns the database |
| Replacing the app, binary or launcher by hand | Unchanged: an ordinary boot, no snapshot |
| Update started by, or aimed at, a version that predates this flow | Existing Wails swap, no trial |

## Rules

1. The rollback boundary is the SQLite triple, as for serve
   ([`snapshot.go`](../../internal/supervise/snapshot.go)). It is copied while
   no process holds it and put back if the new version fails before commit.
2. The new version runs its own trial. The old version quiesces, verifies and
   stages the artifact, records the update durably, hands off and exits. It
   never has to speak the new version's trial protocol.
3. Nothing is published before commit. The install path keeps the old version
   until the commit is durable, so an interrupted update leaves a runnable old
   version where the user launches it, and a target that cannot start never
   displaces it.
4. After commit only the new version is kept. The previous version cannot run
   safely on the migrated database: there are no down migrations, and the
   store refuses a schema newer than it knows.
5. Every step is recoverable on the next launch from durable state, by
   whichever version the install path holds.
6. Layout, snapshot, restore, state, the child channel and the stall rule come
   from `internal/supervise`. The platforms add sequencing and publishing.

## Shared mechanism

### Durable state

The desktop uses a second supervise layout rooted at
`<dataDir>/runtime/app-update/`, with serve's file names: the state file,
`snapshot/` and `restore-marker.json`. It is separate from serve's `runtime/`
files because serve's `ActiveVersion` selects a binary under `versions/`,
while here the install path is the selection.

On Windows the WSL side uses only that layout's `snapshot/` and
`restore-marker.json`. The record lives on the Windows side, under `runtime\`
in the launcher's config directory (`wsldistro.WSLConfigDir`), one file per
runtime profile (`supervise.LauncherRecord`). The launcher chooses an
executable before WSL starts, and fsync and rename are native there. The WSL
backend reads the record through `/mnt/c` only for the reason it shows after
a rollback; it reads the file dev and production launchers share, because
isolated profiles run the harness backend, which has no updater.

The record is `supervise.State` with one `UpdateRecord`. `Begin` refuses a
second update while one is pending, `Retry` counts a trial attempt durably
before the trial starts, `Settle` ends the update and `MarkReported` records
that the outcome reached the user. `ActiveVersion` is the version at the
install path. A binary reads every older schema and writes a record in the
schema it was created with, so the previous version can read what the new one
settled.

Every process that takes `backend.lock` (`bootTransport` and `supervise`)
checks both layouts before anything opens the store. It finishes a marked
restore (`ResumeRestore`), and it refuses to open the store while a pending
record it does not own exists, naming the app that resolves it. This also
closes the same gap for serve: a `serve` started by hand after its supervisor
stopped mid-restore or mid-trial opens that database today.

### Snapshot

`supervise.TakeSnapshot` and `RestoreSnapshot` change in three ways:

- **Free space first.** The data directory's filesystem must have room for the
  database, WAL and shared-memory files plus a margin of 10% of their size,
  at least 512 MiB. The old version checks before it waits for running work
  and again before it hands off, so the user sees the failure in the running
  app, and the snapshot step checks again. Reinstalling the running version
  takes no snapshot and is not checked.
  The check applies to clones too, because the trial's writes unshare cloned
  blocks. The margin is not a guarantee: a migration that rewrites a large
  table can need more. A trial that runs out of space fails and rolls back,
  and the restore removes the live files before copying back, so it needs no
  space beyond what they held. On Windows, statfs inside WSL reports the
  virtual disk, not the host drive (for example `/` reporting 471 GB available
  on a 1 TB virtual disk while `C:` has 309 GB), so the launcher also checks
  the host drive that holds the distribution's disk.
- **Clone when the filesystem can.** `clonefile(2)` on APFS; the `FICLONE`
  ioctl on btrfs, XFS with reflink and bcachefs. An unsupported result
  (`ENOTSUP`, `EOPNOTSUPP`, `EXDEV`, `EINVAL`, `ENOTTY`) falls back to a copy.
  The restore makes the same choice.
- **Progress for a copy.** Copy in chunks (`copy_file_range` on Linux) and
  report bytes after each chunk, so the stall rule sees progress. The fsync
  before the manifest stays.

The copy still runs only while no process holds the database. Every backend
takes `backend.lock` before it opens SQLite, and the step that snapshots, runs
the trial or restores holds that lock for its whole duration. The manifest
records the size and modification time of each file of the triple, so a
later step can prove the live database is still the one the snapshot copied
(`VerifyLiveUnchanged`). The snapshot is
deleted only after a durable commit, or after a durable rollback settlement.

Measured on WSL2 ext4 with a warm page cache:
`FICLONE` returns `EOPNOTSUPP` in microseconds, and a 2 GiB copy including
fsync ran at 337 to 424 MiB/s. A 5.9 GB database costs about 15 to 20 s to
snapshot and the same to restore. An APFS clone should cost near zero; it is
not measured here.

### Trial child

The target runs `agent-overflow __update-trial` as a child of the process that
holds the lock:

- It boots as the platform's real boot does, including its credential backend
  (not serve's `UseFileKeychain`), with the lock held by the parent
  (`BackendLockHeldBySupervisor`) and unattended work parked
  (`ParkUnattendedWork`), as a serve trial does. The parent passes its lock
  descriptor to the trial (fd 5, named by `AO_BACKEND_LOCK_FD`), so the data
  root stays locked until the trial exits even when the parent dies first.
  The trial makes it close-on-exec, so nothing the trial starts keeps it.
- It has no window, binds an ephemeral loopback port, does not write the port
  pin, and skips the boot-time update reconciliation and notices, which belong
  to the published version's boot.
- It speaks the supervise channel on fds 3 and 4. It is always a Unix child;
  on Windows it runs inside WSL. It sends `hello`, then `progress` frames, then
  `prepared`.
- After `prepared` the parent stops it with the ordinary shutdown
  (`DefaultStopTimeout`, then kill). The trial never activates; the published
  version then boots once more with no pending migrations (Decision 2).

### Progress and the stall rule

- A `progress` frame carries the boot progress report
  (`startupprogress.Progress`: phase, detail, step, steps, `updatedAt`) and a
  `liveness` flag. The child's startup reporter sends every report it
  publishes. Its once-a-second heartbeat while a phase is open is liveness;
  a phase or step it enters is progress.
- `hello` gains a `progress` capability flag. It is additive, so
  `ProtocolVersion` stays 1 and existing supervisors keep accepting new
  children.
- The parent's trial timer resets only on progress, never on liveness. The
  trial fails after 30 s without progress (`TrialStallWindow`, the limit the
  launcher's loading page uses) with the reason "the new version stopped
  making progress for 30s (last step: <step>)", and after 30 minutes
  (`TrialCeiling`) whatever it reports. It also fails when the child exits
  before `prepared`.
- A child without the capability keeps the 120 s ceiling
  (`DefaultTrialBudget`), because there is no signal to judge a stall by. An
  older target chosen in the version picker is such a child.
- The SQLite driver exposes no per-statement progress, so a migration step
  is one report. A single migration statement that runs longer than 30 s
  fails the trial and rolls back (Decision 4).
- A process running the trial reports its own progress (snapshot bytes, trial,
  restore bytes) the same way, relays the trial's reports, and announces each
  wait as a step. The launcher judges each WSL command with a 45 s window
  (the trial's 30 s plus 15 s for the two announced waits: the previous
  backend's lock and the trial's stop) and a 60 minute ceiling.

Serve uses the same timer: its supervisor gains the stall rule and nothing
else changes in its cycle.

### Interlocks

Existing checks:

| Where | Check |
|---|---|
| `App.RestartToUpdate` | updater present (`ErrUpdatesUnsupported`) |
| `appupdate.Service.RestartToUpdate`, desktop | downloaded update present (`ErrUpdateNotReady`); 25 s force-exit watchdog |
| `restartToUpdateWSL` | `busy` (`ErrUpdateBusy`); staged release present (`ErrUpdateNotReady`); marker saved before the directive |
| `App.DownloadUpdate` | not shutting down; `busy` |
| Launcher `handleUpdateInstall` | `updateInstalling` compare-and-swap drops a second directive; `InstallDirective.Validate`; staged file is regular; `ClassifyInstallAck` refusal stops; digest re-verified by `CheckAndInstall` (2 min); 25 s exit watchdog |
| Serve `RequestServiceUpdate` | step-up; not shutting down; not a parked trial; valid tag; not the running version; supervisor and release source present; one flow at a time; `waitForUpdateIdle` |

Desktop and WSL have no work check today, so a restart to update stops running
turns, transfers, terminals and workflows. Added:

- **No update over running work.** Desktop and WSL `RestartToUpdate` run the
  serve quiescence before the handoff. `waitForUpdateIdle` closes work
  admission through `workAdmission.quiesce(updateWorkReason)`: unfinished
  triage work, queued dispatch, remote jobs, terminals, running workflows,
  active provider turns, running background work, and every lease holder,
  transfers included. An idle host hands off within the call. A busy one
  returns at once and waits: `updater:restart` names what it waits for,
  `CheckForUpdate` reports it as `restartWaitingFor` to a reloaded page, and
  `CancelRestartToUpdate` ends the wait until the handoff begins. A failed
  handoff, or a WSL handoff the launcher fails, refuses or goes silent on,
  reopens admission. Shutdown ends and joins a waiting restart (Decision 7).
- **One update at a time.** In process, `busy` and `updateInstalling` as now.
  Across processes, a pending record refuses `Begin`, and the process applying
  the update holds the single-instance identity for its whole duration.
- **Recoverable at every step.** See the recovery table for each platform.

## Windows: launcher and WSL payload

S is the launcher path the user starts. L_old is the running launcher and
B_old its payload at the stable WSL path (`appidentity.WSLBinaryDir`). L_new is
the verified launcher staged in `%APPDATA%\agent-overflow\update`, and B_new
is its embedded payload.

The launcher is the update's supervisor. Each WSL step is a command of a
backend binary started through `wsl.exe --exec`. It reports on stdout with a
sentinel prefix and ends with its exit code, as the bootstrap handshake does.
There is no supervise parent inside WSL, for these reasons:

- The launcher already owns the backend's lifetime (Job Object) and the
  window, and the launcher executable is a generation only the Windows side
  can select. A WSL parent would be a second supervisor for one backend, which
  is why `supervise` refuses to run on Windows.
- The record must be readable before WSL starts.
- A permanent parent changes the process tree and shutdown path of every
  launch for a need that exists only during an update.
- Commands need only stdout and an exit code across `wsl.exe`. The launcher
  does not wire stdin, and inherited descriptors do not cross `wsl.exe`
  reliably.
- Inside the trial command the trial is an ordinary Linux child, so the fd 3
  and 4 channel works unchanged.

The commands are `__update-snapshot`, `__update-trial-run`, `__update-restore`
and `__update-discard`, each taking the update id. The trial command restores
the snapshot itself on failure, before it releases the lock. Recovery commands
run through the stable payload, which is the version that will run on the
result and is known to start.

### Sequence

| # | Who | Step |
|---|---|---|
| 1 | B_old | `RestartToUpdate`: interlocks, the wait for running work, free-space check, selfupdate marker, `updater:install` directive (as now). |
| 2 | L_old | Directive checks and `proceeding` acknowledgement (as now). `StageCopy` copies L_new to `runtime\agent-overflow-update-<id>.exe` and verifies its digest. L_old checks that it can write beside S, then runs `L_new --update-preflight <answer> --update-id <id> --distro <d> --update-stable <path>` (3 min), which installs B_new beside the stable payload, runs its `__service-preflight` through `wsl.exe` and writes the answer file. A launcher that predates this flow rejects the unknown flag and writes no answer; that target, and the running version again, take the existing swap. A failed answer reports `failed`; nothing has changed. |
| 3 | L_old | Writes the record `pending` (From L_old, To L_new) durably, starts `L_new --update-apply <id> --wait-pid <pid>` detached, and quits through its ordinary shutdown, which stops B_old. |
| 4 | L_new | Waits for L_old to exit before it claims the single-instance identity (30 s, as the Wails helper does; otherwise settles `failed` and exits). A launch of S during the update then focuses this window. Shows the loading page with `updatingTo`; closing the window hides it and the update continues. |
| 5 | L_new, B_new | On the first attempt, `__update-snapshot`, given the free space of the host drive that holds the distribution (`--host-free`): waits up to 30 s for B_old to release `backend.lock` (otherwise the update settles `failed` and S starts), finishes a marked restore, checks free space, snapshots with progress. |
| 6 | L_new | `Retry`, durably. |
| 7 | L_new, B_new | `__update-trial-run`: takes the lock, requires the snapshot, and on the first attempt refuses when the live triple no longer matches the sizes and modification times the snapshot recorded. Runs the trial child with the stall rule and relays its progress. On `prepared` it stops the trial and exits 0. On failure it restores the snapshot marker-first and exits with the reason. If its stdout closes, it stops the trial and exits without restoring, leaving that to recovery. A refusal on the first attempt settles `failed`; any other end that is not `prepared` or `rolled-back` takes step 8b with a restore. |
| 8a | L_new | Commit: `Settle(committed)` durably, then `__update-discard`. Invalidate `wsl.json`, rename the staged B_new over the stable path, and record its fingerprint; the trial was a successful boot of those bytes. Copy L_new to `S.new` beside S and replace S with it (`MoveFileEx`, `REPLACE_EXISTING` and `WRITE_THROUGH`). Start S and exit. |
| 8b | L_new | Rollback: `__update-restore` through the stable payload unless the trial command restored, `Settle(rolled-back, reason)` durably, `__update-discard`, delete the staged payload, start S (still L_old) and exit. A restore that fails leaves the record `pending` and shows an error page; the next launch retries it. |
| 9 | Launcher at S | Reconciles the record after it claims the single-instance identity and before WSL starts, then launches. Marks a settled record reported and removes staging residue. After a rollback or failure, B_old's boot reads the reason from the record beside the selfupdate marker and shows "Update to X didn't apply: <reason>. Still running Y." through `ApplyFailure` and `NotifyPendingUpdateApplyFailure`. |

Handing back to the previous launcher is step 8b: S still holds L_old because
nothing is published before commit. After commit S holds L_new, and the
previous launcher and payload are gone (rule 4).

Between steps 5 and 7 no process holds `backend.lock`. The single-instance
identity keeps other launchers out. A backend started by hand inside WSL in
that interval blocks step 7 until it exits, and step 7 then refuses because
the database changed, so a rollback never loses its writes. A resumed attempt
cannot make that check, because the earlier trial changed the database.

B_old's install deadline after the acknowledgement is 5 minutes: the staged
copy, the 3 minute preflight or the 2 minute direct swap, and the 25 s exit
watchdog.

### Recovery on the next launch

A launcher at S reads the record before it starts WSL:

| Record | Action |
|---|---|
| none, or settled and reported | ordinary launch |
| settled, not reported | ordinary launch; the backend shows the outcome |
| pending, 0 attempts | settle `failed` ("interrupted before it started"), `__update-discard` for a partial snapshot; ordinary launch |
| pending, attempts below `TrialAttemptLimit` | hand off to the staged `L_new --update-apply <id>`, which resumes at step 5 |
| pending, at the limit or staged launcher missing | `__update-restore` through the stable payload, settle `rolled-back`, `__update-discard`, ordinary launch; if the restore fails, an error page and nothing starts |
| committed, S is not the target | hand off to the staged L_new, which repeats step 8a; if it is missing, show an error page naming the version to reinstall and start nothing |
| committed, S is the target | `__update-discard` while the record is unreported; ordinary launch |

Steps 8a and 8b are idempotent. A pending record with attempts and no snapshot
fails closed, as serve's `snapshotForTrial` does. A command that stalls is
stopped from the Linux side (`wsl.exe --exec kill`, SIGTERM then SIGKILL,
with the pid from its first report), because killing `wsl.exe` may not end
the Linux process; `wsl.exe` is killed last. A result the command reports
while it stops still decides the trial.

## macOS and Linux desktop

### Choice

**(a) Supervisor parent.** The bundle's main executable is a supervisor that
spawns the app as its child.

- LaunchServices starts the parent, which is not the process that shows
  windows. Reopen events, URL opens, Dock clicks and the single-instance
  handoff reach the parent and must be forwarded. The child's Dock icon and
  notification identity must resolve to the bundle while it runs from a
  generation path, which means a second bundle with the same identifier
  registered with LaunchServices.
- The parent itself needs updating. That takes (b)'s swap anyway, or it
  becomes serve's limit that only `service update` replaces the supervisor.
- Every launch pays for a second process to serve a need that exists only
  during an update.
- In its favor, `supervise.Supervisor.Run` applies almost unchanged, and the
  trial can become the live app without a second boot.

**(b) Helper mode of the new binary.** Recommended. It adds no process to
ordinary launches, updates itself with every release, and keeps LaunchServices
seeing one bundle at the install path.

### Sequence (b)

| # | Who | Step |
|---|---|---|
| 1 | Old app | `RestartToUpdate`: interlocks and free-space check; the updater downloads and verifies the release (as now). |
| 2 | Old app | A target older than the first release with this flow takes the existing swap. Otherwise the old app unpacks the verified artifact beside the install path, which also proves the directory is writable (`supervise.PrepareArtifact`; a macOS bundle keeps its resources and framework links). Runs the staged binary's `__service-preflight`. Writes the record `pending` durably. Starts `<staged> __update-apply <id> --wait-pid <pid>` detached in a new session, and quits through the ordinary shutdown with the existing 25 s watchdog. |
| 3 | Helper | Waits for the old app to exit (30 s; otherwise settles `failed` and exits). Takes `backend.lock` and holds it to the end. Claims the single-instance identity and shows a progress window (Decision 1). |
| 4 | Helper | Snapshot on the first attempt, `Retry` durably, trial child with the stall rule. |
| 5a | Helper | On `prepared`: stop the trial, `Settle(committed)`, discard the snapshot, publish, release the lock, start the install path (`open` on macOS, a detached exec on Linux), exit. |
| 5b | Helper | On failure: restore marker-first, `Settle(rolled-back, reason)`, discard, delete the staged artifact, release the lock, start the install path (still the old app), exit. |
| 6 | App at the install path | Boot reconciliation after `backend.lock` and before the store opens. A rolled-back or failed record is shown once through `ApplyFailure` and `NotifyPendingUpdateApplyFailure`, which the desktop boot does not call today; every settled record is then marked reported. |

**Publish.** On macOS, `renamex_np` with `RENAME_SWAP` exchanges the staged and
installed bundles in one step. `rename(2)` cannot replace a non-empty
directory, and two renames would leave no app at the install path if
interrupted. The old bundle, now at the staged path, is deleted once `lsof`
shows no process using it, as `scripts/macos-bundle.sh` does. `RENAME_SWAP`
needs APFS; elsewhere the helper falls back to two renames, and an
interruption between them leaves the install path empty. On Linux the install
path is a file, so a rename replaces it atomically. The updater stays blocked
where `internal/selfupdate/linuxgate.go` blocks it (AppImage, an unwritable
directory).

**Signing.** An ad-hoc signature covers the bundle's contents, not its path,
so the swap does not invalidate it, and nothing is re-signed: the published
bundle is the bytes the trial ran. The updater's HTTP download sets no
quarantine attribute, so Gatekeeper should not assess or translocate it. A
stapled notarization ticket, if releases are notarized later, lives inside the
bundle and survives the swap. The rule that a running bundle is never
rewritten, only published at a distinct path, holds.

**What the user sees.** The window closes. With Decision 1 as recommended, the
helper's window shows backup and trial progress, then closes as the app opens.
Without it nothing is visible in between; with a clone snapshot that interval
is the trial's boot, including every migration. On macOS the helper's window
gives it a Dock icon while it runs; the trial child has none.

**Helper crash.** The next launch of the install path runs the recovery table
below with local steps. Before publish that is the old app, which can restore;
after publish it is the new app. A hung helper keeps the lock and the
single-instance identity until the user ends it; the trial's stall rule does
not cover the helper itself.

| Record | Action |
|---|---|
| settled, not reported | report once; ordinary boot |
| pending, 0 attempts | settle `failed`, discard a partial snapshot; ordinary boot |
| pending, below the limit | hand off to the staged binary's `__update-apply <id>` and exit |
| pending, at the limit or staged artifact missing | restore, settle `rolled-back`, discard; ordinary boot |
| committed, install path is not the target | hand off to the staged binary to publish; if it is missing, refuse to start and name the version to reinstall |
| committed, install path is the target | discard a remaining snapshot; ordinary boot |

## Manual replacement and release notes

Replacing the app, binary or launcher by hand stays an ordinary boot:
migrations run under the boot progress page with no snapshot. On Windows the
new launcher installs its payload because its fingerprint differs from
`wsl.json`, as now. A supervised serve host keeps its rule: the replaced file
supervises and `agent-overflow service update` selects it.

The first release with this flow says:

- In-app updates back up the database, start the new version in a trial, and
  return to the previous version with its data if the trial fails. An update
  needs free disk space about the size of the database.
- This release is installed the previous way. Trial and rollback apply from
  the next update.
- Installing a release by hand does not back up the database. Copy
  `agent-overflow.db` from the data folder first, or use the in-app updater.

## Needs a Mac or Windows host

macOS:

- `RENAME_SWAP` on `/Applications` and `~/Applications`, and its failure on
  HFS+.
- `clonefile` of a large database in `~/Library/Application Support`: time and
  space.
- Dock icon, activation and single-instance handoff for a helper started by
  the app rather than LaunchServices; whether LaunchServices registers the
  staged bundle.
- `open` after the swap starting the new bundle.
- No quarantine attribute on the unpacked bundle, and `codesign --verify`
  passing on it after the swap.
- A keychain prompt from the windowless trial.
- `lsof`-based deletion of the swapped-out bundle.

Windows:

- Replacing S with `MoveFileEx` while a second launcher briefly runs from S,
  and antivirus holding the new file.
- Taskbar grouping and AppUserModelID for L_new running from the staging
  directory.
- Progress line latency through `wsl.exe` stdout.
- Whether a Linux process survives when the Job Object kills its `wsl.exe`.
- Resolving the host drive of the distribution's virtual disk.
- Copy throughput with a cold page cache.
- L_new waiting on L_old's process handle, hiding its window on close while
  the update runs, and starting S detached after it.

Linux: `FICLONE` on btrfs and XFS needs root for a loop mount here.

## Decisions

Accepted: 2, 3, 4 (with a progress-only stall rule), 5, 8 and 9. Decisions
1, 6 and 7 are the user's; the implementation follows each recommendation in
one place: 1 in the helper's window, 6 in `CheckDatabaseSnapshotSpace` and
`TakeSnapshot`, 7 in `App.RestartToUpdate`.

1. **Progress window during an update on macOS and Linux.** Recommended: the
   helper shows the boot progress page, claims the single-instance identity so
   a launch during the update focuses it, and closing it does not interrupt
   the update. The one-shot trial moves migration time out of the visible
   boot, so without a window the user sees nothing for that time. The same
   close behavior applies to L_new on Windows.
2. **One-shot trial.** Recommended: the trial stops after `prepared` and the
   published version boots again, costing one boot with no pending
   migrations. The alternative keeps the trial as the live backend, which
   needs a commit channel into WSL that the launcher does not have, and a
   desktop boot that opens its window only after commit.
3. **Rollback ends at commit.** Recommended: no confirmation after the
   relaunch, and the snapshot is deleted at commit. A failure after
   activation (provider resume, workflows) is not rolled back, as for serve.
4. **Absolute trial ceiling.** Accepted: a 30 minute ceiling beside the 30 s
   stall rule, which counts progress and never liveness. The driver exposes
   no per-statement progress, so one migration statement longer than 30 s
   fails the trial. Open: accept that, count liveness during migrations
   within the ceiling, or report progress per migration batch from the store.
5. **Deferred migration phases.** Recommended: outside the rollback boundary.
   They are idempotent and retried on each open by design, may still run
   inside the trial, and continue after commit. The alternative runs them to
   completion inside the trial, which makes the trial as long as that work.
6. **Always snapshot.** Recommended: yes, even for a target with no pending
   migrations, because the trial's boot writes too. The cost is the free
   space and about 15 to 20 s each way for a 5.9 GB database on WSL ext4. A
   user without the space cannot update in-app until they free it.
7. **Waiting for work on desktop and WSL.** Recommended: serve parity, where
   `RestartToUpdate` waits with a "waiting for X" status and Cancel. The
   alternative refuses at once and names the running work. Either changes
   visible behavior.
8. **Retry after an interrupted update.** Recommended: serve's
   `TrialAttemptLimit` of 2 and its selection table, so the next launch
   resumes an interrupted update once before rolling back. The alternative
   rolls back at the first interruption.
9. **Opening a newer schema.** Accepted: the store refuses a database whose
   migration version is newer than the build knows, so a version-picker
   downgrade fails its trial and rolls back with that error as the reason.
   The store owns the refusal.
