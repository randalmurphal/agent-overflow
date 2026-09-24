# In-app updates: trial and rollback

Status: the Windows launcher and WSL payload are implemented, including
the no-live-migration gate (rule 7). The macOS and Linux desktop helper, the
gate on those platforms and serve's trial budget are not. Decisions are
listed at the end.

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
| A database with pending migrations reached without an update trial: the app, binary or launcher replaced by hand, an update started by a version that predates this flow, another WSL distribution after an update | Snapshot and trial before the boot that would migrate (rule 7) |
| Update aimed at a version that predates this flow | Existing Wails swap, no trial |

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
7. A database is never migrated live without a snapshot, however the binary
   got there. Before a boot that would migrate, the snapshot and trial
   sequence runs first, and the live boot finds the database already
   migrated ([No live migration](#no-live-migration)).

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
backend never reads the record: the launcher passes it what the record
settled (`ReconcileDecision.BackendArgs`).

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
(`CheckLiveBeforeAttempt`). The snapshot is
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
  (`startupprogress.Progress`). Its `updatedAt` and `aliveAt` mean what they
  mean on `/bootstrap.json`
  ([startup readiness](../architecture/transport.md#startup-readiness)):
  `updatedAt` advances only on observed progress, `aliveAt` on every
  heartbeat. The child's startup reporter sends every report it publishes,
  heartbeats included.
- `hello` gains a `progress` capability flag. It is additive, so
  `ProtocolVersion` stays 1 and existing supervisors keep accepting new
  children.
- The parent judges the trial by the rule the launcher's probe applies
  (`startupprogress.StallWatch`, `TrialStallWindow`). After 30 s without an
  `updatedAt` change the reason is "the new version did not finish
  starting: no progress for 30s in phase <phase> (<status>)"; after 30 s
  without either field changing it is "... backend stopped responding for
  30s in phase <phase> (<status>)". The trial also fails after 30 minutes
  (`TrialCeiling`) whatever it reports, and when the child exits before
  `prepared`.
- A child without the capability keeps the 120 s ceiling
  (`DefaultTrialBudget`), because there is no signal to judge a stall by. An
  older target chosen in the version picker is such a child.
- The SQLite driver exposes no per-statement progress. A long migration
  statement counts as progress while the heartbeat's work sampler sees the
  process computing or moving data, or the database files changing size; a
  statement blocked without working for 30 s fails the trial and rolls back
  (Decision 4).
- A process running the trial reports in its own clock. Its steps (snapshot
  bytes, trial, restore bytes, each announced wait) are progress. A relayed
  trial report is progress where the trial's `updatedAt` changed and a sign
  of life where only its `aliveAt` did. A once-a-second heartbeat advances
  `aliveAt`, and `updatedAt` when the shared work sampler
  (`startupprogress.Sampler`) sees the command working. The launcher judges
  each WSL command by the same rule and messages with a 45 s window (the
  trial's 30 s plus 15 s, so the command judges its trial and reports before
  the launcher stops it) and a 60 minute ceiling.

Serve uses the same timer: its supervisor gains the stall rule and nothing
else changes in its cycle.

### No live migration

A boot that would migrate an existing database runs it through a snapshot and
a trial first (rule 7). The store owns the check: an open with
`store.Options.RefusePendingMigrations` fails with
`store.MigrationsPendingError` (the database's version, the build's and the
count between) before anything writes the file. A database without an
applied migration is new and is created as usual. The trial and the update
commands open without the option, so the trial migrates and the next boot
finds nothing pending. This covers every way a new version reaches a
database outside an update: a replacement by hand, an update started by a
version that predates this flow, and another WSL distribution's data root
after an update. On Windows the launcher runs the gate
([No live migration on Windows](#no-live-migration-on-windows)). On macOS and
Linux the helper mode runs it (not implemented). A supervised serve host
keeps its rule: its child only changes through `agent-overflow service
update`, which runs the trial.

### Failure memory

A migration or update trial that settles rolled back or failed is remembered
(`supervise.FailedTrial`) by the build it ran and the database's migration
version when it began, with its reason and its phase: the last progress the
snapshot or trial command reported before the failure, not its restore
(`supervise.RecoveryPhase`). The snapshot command reads the version under
the lock before it copies (`store.ReadSchemaVersion`, the version
`MigrationsPendingError` reports as `Database`) and reports it; the record
keeps it (`UpdateRecord.FromSchema`) for later attempts. The memory lives
beside the durable record, one per record, and a trial that commits removes
it.

The migration gate reads it before it opens a migration. A memory of the
same build over the same schema version stops the gate before the snapshot:
the failure page shows the stored reason and phase with a Retry that runs
the migration again. A different build or a changed schema version removes
the memory, and the migration runs. A memory that cannot be read is logged
and stops nothing; the outcome replaces it. An in-app update is a person's
request and always runs; its failure replaces the memory and its commit
removes it. Not remembered: a failure whose schema version is unknown
(logged), an update interrupted before its first trial, and an update rolled
back because its new launcher is missing. An update whose trial was
interrupted at every attempt is remembered by the recovery that rolls it
back.

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
  `CancelRestartToUpdate` ends the wait until the handoff begins. From the
  handoff until the process is replaced, `CheckForUpdate` reports
  `restartingTo`, read from the update's durable record (on WSL the
  selfupdate marker), so a reloaded page shows the restart underway and
  offers nothing. A failed
  handoff, or a WSL handoff the launcher fails, refuses or goes silent on,
  reopens admission. Shutdown ends and joins a waiting restart (Decision 7).
- **One update at a time.** In process, `busy` and `updateInstalling` as now.
  Across processes, a pending record refuses `Begin`. On macOS and Linux the
  helper applying the update holds the single-instance identity for its whole
  duration. On Windows the record names the launcher applying it, a launch
  while that launcher runs joins the update instead of acting on the record,
  and `UpdateSequence.Apply` refuses an update another running launcher
  applies.
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

The launcher's record is `runtime\app-update-<mode>.<distro>.json` under
`%APPDATA%\agent-overflow` (`supervise.LauncherRecordPath`): one per launcher
build (dev, production or an isolated profile) and data root (the
distribution), so no launcher reads another's record. Every step below names
the record of the distribution whose backend is being updated.

The commands are `__update-space`, `__update-snapshot`, `__update-trial-run`,
`__update-restore` and `__update-discard`, each taking the update id. The trial command restores
the snapshot itself on failure, before it releases the lock. Recovery commands
run through the stable payload, which is the version that will run on the
result and is known to start.

### Sequence

| # | Who | Step |
|---|---|---|
| 1 | B_old | `RestartToUpdate`: interlocks, the wait for running work, selfupdate marker, `updater:install` directive (as now). |
| 2 | L_old | Directive checks and `proceeding` acknowledgement (as now). `StageCopy` copies L_new to `runtime\agent-overflow-update-<id>.exe` and verifies its digest. L_old checks that it can write beside S, then runs `L_new --update-preflight <answer> --update-id <id> --distro <d> --update-stable <path>` (3 min), which installs B_new beside the stable payload, runs its `__service-preflight` through `wsl.exe`, asks it with `__update-space` whether the snapshot its plan will copy fits (`supervise.PlanSnapshot`: the database files plus a margin, less a leftover snapshot the plan reclaims, against the data disk and the host drive), and writes the answer file. A launcher that predates this flow rejects the unknown flag and writes no answer; that target, and the running version again, take the existing swap. A failed or refused answer reports `failed` with its reason; nothing has changed. |
| 3 | L_old | Writes the record `pending` (From L_old, To L_new) durably, starts `L_new --update-apply <id> --distro <d> --wait-pid <pid> --wait-start <start>` detached, names it in the record as the update's applier by process id and creation time (`wsllauncher.StartApplier`; a launcher it cannot name is killed and the update settles `failed`), and quits through its ordinary shutdown, which stops B_old. |
| 4 | L_new | Waits for L_old to exit (30 s, as the Wails helper does; otherwise settles `failed` and exits). It waits on a handle to the process with L_old's id whose creation time is `<start>` (`supervise.ProcessRef`), so a reused process id is never waited on. L_new holds no single-instance identity, so a launch of S from L_old's exit to the end of the update claims it, finds L_new named and running in the record, and joins the update (`UpdateSequence.Join`): it names itself in `app-update-<mode>.<distro>.joiner.json`, shows the progress L_new publishes in `app-update-<mode>.<distro>.progress.json` (at most every 500 ms, written to a temporary file and renamed over it; a rename Windows refuses while the joiner reads is written again unless a newer report replaced it), and reconciles once L_new exits. A later launch focuses the joined one. L_new shows the loading page with `updatingTo`; closing the window hides it and the update continues. It hides the window while a joined launch runs, and exits without starting S once the update ends with one running, including while it shows an error page. L_new runs on its own WebView2 profile (`<profile>-update`) without the DevTools port or notifications, which the joined launch owns. It opens at the saved window placement and does not save its own; `window.json` belongs to the ordinary launcher. L_new and every launcher that starts S again pass the distribution with `wsllauncher.DistroArgs`, so a transient `--distro` stays transient and a picked or saved one stays saved. |
| 5 | L_new, B_new | On the first attempt, `__update-snapshot`, given the free space of the host drive that holds the distribution (`--host-free`): waits up to 30 s for B_old to release `backend.lock` (otherwise the update settles `failed` and S starts), finishes a marked restore, reads the database's migration version for the [failure memory](#failure-memory), checks free space again, snapshots with progress. |
| 6 | L_new | `Retry`, durably. |
| 7 | L_new, B_new | `__update-trial-run`: takes the lock, requires the snapshot, and before every attempt compares the live database files' sizes and modification times with what the update last left: the snapshot before the first attempt, and after that what the previous attempt recorded when it ended. A mismatch reports `changed`. Runs the trial child with the stall rule and relays its progress. On `prepared` it stops the trial and exits 0. On failure it restores the snapshot marker-first and exits with the reason. If its stdout closes, it stops the trial and exits without restoring, leaving that to recovery. `changed`, or a refusal on the first attempt, settles `failed` without a restore, which would discard another backend's writes; any other end that is not `prepared` or `rolled-back` takes step 8b with a restore. |
| 8a | L_new | Commit: `Settle(committed)` durably, then `__update-discard`. Invalidate `wsl.json`, rename the staged B_new over the stable path, and record its fingerprint; the trial was a successful boot of those bytes. Copy L_new to `S.new` beside S and replace S with it (`MoveFileEx`, `REPLACE_EXISTING` and `WRITE_THROUGH`). Start S, unless a launch joined the update, and exit. |
| 8b | L_new | Rollback: `__update-restore` through the stable payload unless the trial command restored, `Settle(rolled-back, reason)` durably, `__update-discard`, delete the staged payload, start S (still L_old) unless a launch joined the update, and exit. A restore that fails leaves the record `pending` and shows an error page; the next launch retries it. |
| 9 | Launcher at S | Reconciles the chosen distribution's record after it claims the single-instance identity and before that distribution's backend starts, then launches. Marks a settled record reported and removes staging residue. The first launch of a committed target passes `--updating-to <To>` to its backend, whose startup report names the update it finishes; the record is the only source of that version. After a rollback or failure, the launcher passes `--update-failed-to <To> --update-failed-reason <reason>` to a backend of the update's starting version (`ReconcileDecision.BackendArgs`). B_old's boot quotes the reason when its selfupdate marker expected that version, "Update to X didn't apply: <reason>. Still running Y.", through `ApplyFailure` and `NotifyPendingUpdateApplyFailure`. The backend never reads the record. A launch that joined the update and is not the target of a committed one started before the commit replaced S: it starts S and exits instead (`ReconcileRelaunch`), so its older payload never runs. |

Handing back to the previous launcher is step 8b: S still holds L_old because
nothing is published before commit. After commit S holds L_new, and the
previous launcher and payload are gone (rule 4).

Between steps 5 and 7 no process holds `backend.lock`. The record's named
applier keeps other launchers out: a launch joins instead of starting a
backend. A backend started by hand inside WSL in
that interval blocks step 7 until it exits, and step 7 then reports `changed`,
so a rollback never loses its writes. Each attempt records the files it left
when it ends, prepared, rolled back or interrupted, so a resumed attempt makes
the same check. An attempt whose command died before recording its end leaves
nothing to compare (`UnendedAttemptError`): the next attempt restores the
snapshot first, and every restore records the files it restored as what that
attempt left, so the check runs against them.

B_old's install deadline after the acknowledgement is 5 minutes: the staged
copy, the 3 minute preflight or the 2 minute direct swap, and the 25 s exit
watchdog.

### No live migration on Windows

The launcher starts its ordinary backend with `--refuse-pending-migrations`
(`wsllauncher.RefusePendingMigrationsArgs`, accepted only with
`--print-url-fd` and without `--soak`). A backend whose store refuses serves
`/bootstrap.json` as 409 with `{"reason":"migrations-pending","database",
"build","pending"}` until it is stopped (`startupprogress.WriteMigrationsPending`).
`ProbeBootstrap` returns it as `MigrationsPendingError`, and the launcher
stops the backend. A refusal whose backend could not be stopped shows the
startup failure page and starts nothing, because that backend may still hold
the database.

`UpdateSequence.Migrate` then writes a migration record
(`State.BeginMigration`: `From` and `To` are the installed version, and the
staged fields are empty), refused while an update is pending, and runs steps
5 to 8 through the stable payload. Commit only discards the snapshot;
rollback is step 8b without a staged payload. The launcher removes the
record once it settles, so a launcher that predates migrations never reads
one, and the backend is never told about one. On commit the launcher starts
the backend again, which finds nothing pending. Otherwise it shows why on a
failure page and starts nothing: the backup was restored, or the migration
did not run, with the recorded reason. A restore that fails leaves the
record pending for the next launch.

The gate passes the refusal's `Database` version to `Migrate`
(`wsllauncher.MigrationRequest`), which applies the
[failure memory](#failure-memory) first. The memory is
`app-update-<mode>.<distro>.failed-trial.json` beside the record
(`wsllauncher.FailedTrialPath`). The page's Retry calls the launcher's bound
`RetryMigration`, which runs the same launch again with
`MigrationRequest.Retry`.

Isolated profiles (harness, soak, perf) are not gated: their `--soak`
backend resolves its own data root, which the update commands do not
address.

### Recovery on the next launch

A launcher at S reads the record before it starts WSL:

| Record | Action |
|---|---|
| any, with its named applier running | join (step 4), then this table once the applier exits; after a commit, a launcher that is not the target starts S instead |
| none, or settled and reported | ordinary launch |
| settled, not reported | ordinary launch; the backend shows the outcome |
| pending, 0 attempts | settle `failed` ("interrupted before it started"), `__update-discard` for a partial snapshot; ordinary launch |
| pending, attempts below `TrialAttemptLimit` | hand off to the staged `L_new --update-apply <id>`, which resumes at step 5 |
| pending, at the limit or staged launcher missing | `__update-restore` through the stable payload, settle `rolled-back`, `__update-discard`, ordinary launch; if the restore fails, an error page and nothing starts |
| committed, S is not the target | hand off to the staged L_new, which repeats step 8a; if it is missing, show an error page naming the version to reinstall and start nothing |
| committed, S is the target | `__update-discard` while the record is unreported; ordinary launch |
| migration, settled | `__update-discard`, remove the record, ordinary launch |
| migration, pending, 0 attempts | settle `failed`, `__update-discard`, remove the record, ordinary launch; the gate starts a new migration if the backend refuses |
| migration, pending, with attempts | resume it in this launcher (`ReconcileResume`): step 7 below the limit, the restore at it; then as `Migrate` ends |

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

## Release notes

The first release with this flow says:

- In-app updates back up the database, start the new version in a trial, and
  return to the previous version with its data if the trial fails. An update
  needs free disk space about the size of the database.
- This release is installed the previous way. On Windows it backs up the
  database and upgrades it in a trial when it first starts; trial and
  rollback of the update itself apply from the next update.
- On Windows, a release installed by hand also backs up the database before
  it upgrades it. On macOS and Linux it does not yet: copy
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
- L_new without the single-instance identity beside a joined launch: the
  separate WebView2 profile, hiding for the joined launch, exiting for it
  after the update, and the joined launch starting S after a commit.
- The no-live-migration gate in the launcher: `--refuse-pending-migrations`
  on the ordinary backend's argv through `wsl.exe`, stopping the refusing
  backend through its shutdown call, the progress page during `Migrate`,
  the failure page after one that did not commit, and `ReconcileResume` on
  the next launch.
- The remembered failure's page in WebView2: its Retry button reaching the
  bound `RetryMigration` through `/wails/runtime`, the loading page during
  the retried migration, and the window after it.

Linux: `FICLONE` on btrfs and XFS needs root for a loop mount here.

## Decisions

Accepted: 2, 3, 4 (with a progress-only stall rule), 5, 8 and 9. Decisions
1, 6 and 7 are the user's; the implementation follows each recommendation in
one place: 1 in the helper's window, 6 in `supervise.PlanSnapshot` and
`TakeSnapshot`, 7 in `App.RestartToUpdate`.

1. **Progress window during an update on macOS and Linux.** Recommended: the
   helper shows the boot progress page, claims the single-instance identity so
   a launch during the update focuses it, and closing it does not interrupt
   the update. The one-shot trial moves migration time out of the visible
   boot, so without a window the user sees nothing for that time. L_new on
   Windows has the same close behavior; a launch during its update joins it
   and shows its progress (Windows step 4).
2. **One-shot trial.** Recommended: the trial stops after `prepared` and the
   published version boots again, costing one boot with no pending
   migrations. The alternative keeps the trial as the live backend, which
   needs a commit channel into WSL that the launcher does not have, and a
   desktop boot that opens its window only after commit.
3. **Rollback ends at commit.** Recommended: no confirmation after the
   relaunch, and the snapshot is deleted at commit. A failure after
   activation (provider resume, workflows) is not rolled back, as for serve.
4. **Absolute trial ceiling.** Accepted: a 30 minute ceiling beside the 30 s
   stall rule, which counts observed progress (`updatedAt`) and never the
   bare heartbeat (`aliveAt`). Observed progress includes the process's own
   CPU and storage work, so one long migration statement keeps the trial
   alive while it works and fails it only when blocked for 30 s.
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
