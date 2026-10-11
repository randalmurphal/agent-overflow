# Release upgrade check

`make release-upgrade-check` proves that the candidate build takes a large
database the previous release wrote, migrates it through the launcher's real
update path, boots on it, and serves its sidebar and threads. It is a release
gate and an operator tool; it never runs per pull request, because the corpus
is gigabytes and a run takes minutes.

The check is `e2e/tests/release-upgrade.manual.spec.ts` under the manual
Playwright config, with its helpers and the corpus generator in
`e2e/tests/release-upgrade-helpers.ts`. `scripts/release-upgrade-check.sh`
resolves and downloads the previous release and starts the suite through
`bin/ao-harness-e2e`, so every process runs inside the harness memory
boundary and, on Linux, the isolated network namespace.

## Where the old-schema data comes from

There is no migration manager and no down migration. The previous release's
own binary is the only generator of data at its schema: the check boots that
release's headless Linux binary in harness mode on a fresh data root and
seeds the corpus through its `HarnessSeed`, so the database is at that
release's schema by construction, written by the code that owned it.

The synthetic corpus is deterministic: a function of its scale and nothing
else. Its shape is a measured year of real use (474 threads over 6 projects,
41% Codex, 5 archived, thread groups and pins; items per thread p50 119, p90
1024, p99 26883, max 160710; turns per thread p50 1, p99 179, max 619;
payload bytes per item p50 640, p90 6 KiB, p99 25 KiB, max 1 MiB). Thread
sizes are stratified quantiles of those distributions, so a smoke slice and
a full corpus carry the same shape. Scale 1 is about 2 GB. The generator
needs the `repeat` and `padBefore` seed fields, which every release from
v0.0.16 on has.

What the corpus cannot carry: subagent rows (59% of real items have a
parent, which `HarnessSeed` does not write), workflow runs, forge state,
transfers, attachments and imports. The report names every table the corpus
leaves empty. A real database covers those, locally:

```sh
bin/ao-harness clone --from ~/.config/agent-overflow --data-dir /path/to/clone
make release-upgrade-check RELEASE_UPGRADE_CORPUS=/path/to/clone
```

A clone is scrubbed but still the user's data. It stays on the machine that
made it: never commit it, attach it to a run, or publish it.

## What one run proves

1. The previous release boots on an empty root and seeds the corpus (or the
   corpus root is copied in). A read-only census records the schema version,
   project, thread, archived, pinned, group, turn, item and payload counts,
   payload bytes, every thread's top-level item count, and every table's row
   count.
2. The candidate's gated headless boot, started as the Windows launcher
   starts it (`--refuse-pending-migrations`), answers `/bootstrap.json` with
   the `migrations-pending` refusal naming the database's version, and runs
   no provider probe. When the corpus is already at the build's schema it
   answers ready instead and the migration steps are skipped.
3. `__update-snapshot` reports the refused version; `__update-trial-run`
   reaches `prepared` through the real App trial boot; the next gated boot
   answers ready.
4. A candidate harness boot on the same root reports the build's schema
   through `HarnessStoreStatus`, and `ListProjects`,
   `HarnessListThreadRows`, `ListArchivedThreads` and `ListThreadGroups`
   return the counts the previous release wrote.
5. A real browser page shows every project, shows thread rows, and opens the
   largest thread through the sidebar search until its first timeline row
   paints. The frontend error log stays empty.
6. The deferred migration phases, which start once a client has read the
   catalog, finish without a recorded failure.
7. After the candidate stops, the census is compared with the one before
   the migration: projects, threads, archived, pinned and groups are
   exactly what the previous release wrote; turns, items, payloads and
   payload bytes never shrink, in total or in any thread, and no thread
   loses its rows. A repair migration may add rows (a carrier, a fork's
   own copy of shown history); the report carries every count's delta so
   an addition is visible.
8. `__update-discard` commits the update, as the launcher does.

Every phase's duration lands in the JSON report (`durations`), beside the
versions, the corpus figures, the gate's answer, the reads, the UI timings,
the final deferred status and the empty tables. The report is attached to
the Playwright result; `RELEASE_UPGRADE_REPORT=<file>` writes it elsewhere.

## Running it

```sh
make release-upgrade-check                                   # newest release, scale 1
make release-upgrade-check RELEASE_UPGRADE_SCALE=0.02        # smoke slice, seconds
make release-upgrade-check PREVIOUS_RELEASE=v0.0.16          # a named release
make release-upgrade-check RELEASE_UPGRADE_CORPUS=/clone     # a real data root
```

At scale 1 from v0.0.16 (474 threads, 649k items, a 2.7 GB database) a
run takes under three minutes on a developer laptop: about 140 s to seed,
7 s for the launcher's snapshot, under a second for the trial boot's
migrations from v139 to v141, 1.5 s for the candidate boot, and under a
second until the 130k-row thread paints its first row. The smoke slice
takes about eight seconds.

The previous release's `agent-overflow-headless-linux-amd64` is downloaded
once to `bin/releases/<tag>/` and verified against the release's
`SHASUMS256`, so the check runs on Linux (a developer's WSL or a CI runner).
The migration and store code is platform-neutral; the macOS and Windows
release legs are covered by their own smokes. The sandbox lives under the
system temp dir (`AO_RELEASE_UPGRADE_ROOT` moves it) and needs roughly three
times the corpus size while the launcher's snapshot exists.
`AO_RELEASE_UPGRADE_KEEP=1` keeps it; the Playwright annotation names the
path.

## Reading a failure

- The gate answers 200 on a corpus that should be behind: the candidate's
  chain has no migration past the corpus's version, which for a synthetic
  corpus means the version bump is missing a migration or the previous
  release was not the one intended. The report's `previous.schemaVersion`
  and `candidate.schemaVersion` say which.
- `__update-trial-run` fails: the migration itself failed on this data.
  The command's stderr is in the test output. Fix the migration; never the
  corpus.
- A count differs after the migration: a migration dropped or duplicated
  rows. The census compares per thread, so the failing diff names the
  thread.
- The deferred phase records a failure: `HarnessStoreStatus` reports the
  version, title and first error. The phase skips a failing item and the
  next boot retries it, so this is a defect even when the boot looked fine.
- The timeline never paints or the frontend error log is non-empty: the
  migrated rows render into a client error. The Playwright trace carries
  the page.
