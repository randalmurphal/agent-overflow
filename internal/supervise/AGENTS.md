# internal/supervise/

The two-process split behind a serve host that can be updated without anyone
at the machine: `agent-overflow supervise` decides which version runs, and the
backend it spawns is its child. This package is the protocol, the durable
state, and the supervisor loop. Operator-facing walkthrough:
[serve-mode.md](../../docs/architecture/serve-mode.md) § The supervisor.

The architecture is t3code's `docs/internals/server-updates.md` translated to
one Go binary: a stable supervisor the service manager owns, immutable staged
versions, a database snapshot taken while nothing holds the file, a trial boot
parked before it can act, and a durable commit or a marked restore.

## Two properties everything else follows from

**A supervisor is optional forever.** `agent-overflow serve` started by hand
must behave exactly as it did before this package existed. The whole mechanism
keys off `AO_SERVICE_CHANNEL`, which only a spawning supervisor sets, and
`OpenChildChannel` answers `(nil, nil)` without it — an absent marker is not an
error and must never become one. Anything that would make a bare `serve` notice
a supervisor it does not have is wrong.

**An invalid state fails CLOSED.** `State.Validate` refuses an unknown schema,
a version that could name something outside `versions/`, a record in a state
nobody defined, a record whose `From` disagrees with `ActiveVersion`. The
supervisor exits non-zero and starts NOTHING rather than guessing, because
every guess it could make is "run a version the operator did not choose".
`SaveState` validates too, so this package cannot be the thing that writes a
file it would later refuse to read. `LoadState` reports absent-and-nil for a
fresh install and an ERROR for a file it cannot read — never "no state", which
would silently adopt this binary over a committed update.

## The selection table is the feature

`State.Select` is the entire semantics, in one function, so no caller can hold
a second opinion:

| state | runs | how |
|---|---|---|
| no record | `ActiveVersion` | ordinarily |
| `pending` A → B | B | as a retryable trial |
| `committed` A → B | B | ordinarily |
| `rolled-back` A → B | A | ordinarily |
| `failed` A → B | A | ordinarily |

`rolled-back` and `failed` differ in what happened, not in what runs:
rolled-back means the trial RAN and never reached prepared, so the snapshot was
restored over its work; failed means the update never got as far as a trial —
a target that would not preflight, a snapshot that could not be taken, a spawn
that failed — so there was nothing of the target's to undo.

**Exactly one update record at a time.** `Begin` COLLAPSES the settled one: the
version it selected becomes the new `ActiveVersion`. That is why a committed
update needs no separate promotion step, and why "previous" always means one
thing.

**A settled outcome is news exactly ONCE.** The record rests in the file until
the next update collapses it, which can be months of nightly restarts, so
`Select` announces `Outcome`/`Reason`/`Target` only while `UpdateRecord.Reported`
is false, and the supervisor sets that flag (`State.MarkReported`) in the
commit write itself (the committing child publishes it from the commit
frame) or, for a rollback, when the backend it activated says hello.
The flag is additive under the SAME schema number: an absent `reported` in a file written before it existed reads as
false, which is the reading that still announces. Bumping the schema for a
boolean whose absence has one meaning would fail every existing install closed
at boot.

**The activate frame names TWO versions.** `Selection.Version` is what runs and
`Selection.Target` is what the settled update was aiming AT. On a rollback they
are different, and only the record knows both — the version answering is not
the version that failed, and a client told only one of them is told the wrong
one.

## Ordering rules that are the mechanism, not tidiness

- **Preflight before anything durable.** `acceptUpdate` stats the target's
  binary and runs `__service-preflight` in its own process BEFORE it writes the
  pending record. A pending record naming a version that cannot run is a
  rollback the operator did not need to pay for.
- **`PreflightBinary` is the ONE preflight.** Exported, because two processes
  ask that question: the supervisor here, and the backend in `internal/app`
  before it stages a downloaded artifact into a version directory (the remote
  update trigger refuses a file that is not an Agent Overflow binary this host
  can run while it is still a temp file). Both go through
  `preflightBinary`, which differs only in the environment it hands the child —
  `nil` inherits this process's, and the supervisor passes `childEnv()` so its
  answer comes from a process started the way the child would be. A second
  implementation of "run it and read its answer" is how one caller accepts what
  the other would refuse, and `CheckPreflight`'s protocol rule is only correct
  if both saw the same answer.
  `TestTheSupervisorAsksThroughTheSameImplementation` pins the two together.
- **Snapshot only while nothing holds the database.** Between the stop and the
  trial's start, and nowhere else. The copy is not safe at any other moment.
- **The snapshot is taken where the trial is SPAWNED, not where the update was
  accepted.** `Run`'s trial branch takes it on the first attempt
  (`snapshotForTrial`, `Attempts == 0`), because that is the one place a trial
  can begin. Accepting an update writes the pending record durably and then
  waits out `ResponseGrace` so the asking child can flush its answer, and a
  crash, a shutdown or a kill anywhere in that window used to leave a pending
  record with no snapshot beside it — after which the next boot trialled the
  new version against a database nothing could put back. The general rule:
  when a durable record promises a resource, whatever CONSUMES the record
  creates the resource, or a process that dies between the two leaves a
  promise nobody can keep.
- **A record already mid-trial with no snapshot FAILS CLOSED.** The trial has
  written to the database, so a snapshot taken now would preserve exactly the
  work a rollback exists to undo. The supervisor starts nothing and names
  `agent-overflow service update` as the way to choose a version by hand.
- **Commit durably, THEN tell the child.** The child opens its activation gate
  on the commit frame, so a frame sent before the write would be a backend
  acting unattended on an update no restart would select.
- **Marker before restore, and the manifest before the marker.**
  `RestoreSnapshot` first confirms a readable snapshot manifest is there
  (`SnapshotPresent`), then writes and fsyncs the marker, then removes the
  live triple, then copies back exactly the manifest's set, then removes the
  marker. `ResumeRestore` runs before the state file is even READ, so no
  version can open a half-restored database. The check is FIRST because the
  marker is the one thing in this package nothing else clears: a marker
  fsynced for a snapshot that does not exist fails every subsequent boot at
  `ResumeRestore`, and no state the supervisor can reach removes it. Never
  write a durable "I am part-way through X" marker before confirming X can
  finish.
- **Count the attempt before the trial, durably.** A supervisor killed by the
  very trial it is starting must find the attempt recorded when it comes back,
  or the two of them loop forever. `Run` is the one place `Retry` is called.

## One goroutine

`runChild` is a single `select` over the context, the child's exit, its frames,
and two timers. The linear work between selects — stop, snapshot, restore — runs
on that same goroutine with nothing else scheduled. There is no lock in
`supervisor.go` and there must not need to be: two state transitions cannot
overlap because there is only ever one thing running.

Two consequences that were bugs before they were rules:

- **The child's exit is a FACT, not a message.** Two readers need it (the loop,
  which decides what it means, and `stopChild`, which waits for the stop it
  asked for). It is a closed `exited` channel plus an `exitErr` field, because a
  one-shot value channel gave whichever read second a wait that never ended —
  a crashed trial wedged the supervisor permanently, in exactly the case
  rollback exists for.
- **The message channel closing and the process exiting are the SAME event**,
  arriving on two channels in whichever order the scheduler picks. Never settle
  a trial on the channel close: the exit STATUS is the better description and
  is a moment behind, so that arm is disabled and the exit (or the trial budget)
  supplies the reason. Settling on the first made a crashed trial's durably
  recorded reason a coin flip.

## Spawning

- **The activate frame is written BEFORE `Start`.** The child learns whether it
  is a trial from its first frame, and pre-loading the pipe means it never waits
  on a supervisor that might be busy. Tens of bytes into a buffer measured in
  tens of kilobytes, so the write cannot block.
- **`exec.CommandContext(context.Background(), …)`, deliberately.** The
  supervisor's own context must not be the command's: exec would SIGKILL the
  group the instant it cancelled, and shutdown is precisely when the backend has
  to close provider sessions and flush SQLite. A context is still required,
  because `procutil.ConfigureGroup` installs a `Cancel` and exec refuses one on
  a command built without one.
- **SIGTERM to the PROCESS, group kill only as the fallback.** Signalling the
  group directly would interrupt the very shutdown the stop is waiting for; the
  fallback must be the group, or a provider CLI keeps the database open past the
  snapshot.
- Descriptors 3 and 4, because 0-2 are the child's own stdio — a serve host
  prints its endpoints for a person to read and takes a pairing answer on stdin.

## What crosses the boundary, and what does not

The snapshot covers the SQLite triple and NOTHING else. Attachments, provider
homes, narratives, the tailnet state directory are all outside it. That is one
of the two reasons the child's activation gate exists (`internal/app`'s
`app_activation.go`): a trial that swept retention or refreshed a credential
would have done something no snapshot can undo.

`DatabaseFiles()` restates the names `internal/app` opens rather than importing
them — this package must stay clear of the App graph — and
`TestSuperviseSnapshotsTheDatabaseFilesThisPackageOpens` in `internal/app` pins
the two together.

## Testing

The supervisor is a process that runs processes, so testing it any other way
tests something else. `supervisor_unix_test.go` drives **scripted fake
children**: shell scripts staged exactly where a real staged version would be,
speaking the real protocol on the real inherited descriptors, answering the real
preflight. The environment they get is `PATH` plus a `HOME` inside
`t.TempDir()`, so a child cannot see the developer's real home even by accident.

Nothing here may run a real `systemctl` or `launchctl`, reach external services,
or boot a second real backend against anyone's provider homes.

`artifact_smoke_unix_test.go` is an explicit manual exception to scripted
children: `make service-artifact-smoke` accepts two supplied production binaries
or macOS ZIPs, stages them, boots a baseline, commits a trial, and cold-restarts
the target while retaining backend identity and a SQLite row. It skips without
both artifact paths. The fixture uses `kerneltest` isolation, explicit mocks
for boot-time Claude discovery and Codex catalog probes, poisoned PATH fallbacks, and loopback
HTTP only. `--data-dir` names the config root; settings, SQLite and the supervisor
layout belong under its `agent-overflow/` child. The fixture never installs a
service or replaces the supplied artifacts. This supplements the deterministic
failure tests; it does not verify release signatures or the download source.

Every sequence is covered and each one earns its keep: the full commit cycle,
a trial that crashes, a trial that never prepares, a supervisor killed mid-trial
(three supervisors over one install, ending at the attempt limit), a restore
interrupted mid-copy, an invalid state file, an unstaged target, a target
speaking a newer protocol, and an update whose snapshot cannot be taken. The
marked-restore test asserts the trial READ the restored bytes, which is the only
way to prove the resume ran before the spawn.

`supervisor_recovery_unix_test.go` holds the sequences that begin from a state
file rather than from a running backend, over the same rig: a pending update
with no snapshot and no attempt (the boot snapshots, then trials, then rolls
back cleanly), the same with an attempt already spent (the boot starts nothing
and names the remedy), and a settled outcome carried to exactly one backend
across two boots.

## Data-root ownership

The executable supervisor holds `backend.lock` throughout its lifetime, including
the interval with no child during snapshot/rollback. Config.OwnsDataRoot may be
set only after that lock is held; the additive activate-frame field tells a new
child to reuse this ownership. A legacy supervisor omits it, so a new child
acquires its own lock. Ordinary desktop/serve/harness boots also take that lock.
The root shell owns the OS primitive; the supervisor package does not infer
ownership from a PID, an environment variable, or merely having a parent.

## macOS release bundles

`PrepareArtifact` only receives downloads already verified by `ReleaseSource`.
It bounds ZIP entry count and expanded bytes, rejects path traversal and special
files, creates links after regular files, and resolves the complete link graph
inside the canonical bundle root. Canonicalize both sides of containment checks:
macOS temporary directories can have `/var` and `/private/var` spellings.

Preflight inspects the executable inside the expanded bundle. `Artifact.Stage`
fsyncs the tree and publishes the complete version directory without replacing
an existing version. Its flat `agent-overflow` entry point execs the real bundle
executable, preserving compatibility with the installed supervisor protocol.
An identical staged archive or a legacy flat executable with identical bytes is
reused; a different build under the same version is refused. Closing an artifact
removes only its temporary extraction. Resources and framework links are retained.

macOS release ZIP writers use `zip -qry`: `-y` preserves framework symlinks.
Following links while packaging can invalidate the signed bundle even when the
extractor preserves every archived byte. Keep both CI and local packaging aligned.

In process tests, observe the supervisor's durable state before stopping it.
A child logging `hello` proves only that it wrote a frame; cancelling then can
beat the supervisor reading it or saving the reported outcome. Use the rig's
condition wait for assertions about a durable acknowledgement.

A backend that loses the update reply drains and exits with
`RestartForUpdateExitCode` (75). The supervisor chooses from its durable state
again, even without a service manager. An accepted update continues if its child
exits before response grace expires. Older supervisors propagate this nonzero
exit to launchd/systemd, whose on-failure restart loads the same journal. A trial
exiting with this code still fails its trial; it cannot bypass rollback.
