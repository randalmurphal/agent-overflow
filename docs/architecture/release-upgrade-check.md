# Release upgrade check

`make release-upgrade-check` proves that the candidate build takes a large
database the previous release wrote, migrates it through the launcher's real
update path, boots on it, serves its sidebar and threads, and writes its own
slice of use into it. It is a release gate and an operator tool; it never
runs per pull request, because the corpus is gigabytes and a run takes
minutes. What runs per pull request is the coverage gate below, which keeps
the corpus representative of the schema.

The check is `e2e/tests/upgrade-corpus.manual.spec.ts` under the manual
Playwright config, with its helpers and the corpus generator in
`e2e/tests/upgrade-corpus-helpers.ts`. `scripts/release-upgrade-check.sh`
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

What the seed cannot write, the top-up does. After the migration the
candidate boots on the migrated root and runs `topUp`
(`e2e/tests/upgrade-corpus-topup.ts`): one live turn per mock scenario in
`topUpScenarios` (text, thinking, tool calls, allowed and denied approvals,
file edits with persisted diffs, Claude and Codex subagents, usage limits, a
session death, a Codex revert), then forks, a side chat, a draft with image
and file attachments, pins, a group, an archive, a rename, a favorite,
threads at every chat-bar setting, a worktree, project settings and an
archived project, and workflow runs (finished, gated, questioned, cancelled,
fired by an automation). Those rows are written by the candidate's own code
paths at its own schema, which is what a corpus handed to the next release
needs: rows of every feature that existed when they were written.

The coverage gate, `e2e/tests/upgrade-corpus-coverage.spec.ts` in `make e2e`,
holds the seed and the top-up to the schema on every pull request: it
seeds a smoke-scale corpus, runs the top-up on the current build, then
reads `sqlite_master` and fails on any table with no rows, any column no row
sets to a non-default value, and any `CHECK (column IN (...))` value no row
carries, unless `coverageAllowlist` in `upgrade-corpus-config.ts` declares
it with the reason it cannot be reached (a paired device, a second backend,
a provider home to import, a scenario the library lacks). An entry that has
become covered, or that names something the schema no longer has, fails the
gate as stale, so the list cannot rot. The gate also fails on a mock
scenario that is neither a top-up turn nor excluded with a reason. A
schema or scenario change therefore lands with the top-up step or the
config entry that keeps the corpus representative; the failure names which.

Forge state, transfers, imports and paired devices stay allowlisted; a real
database covers those, locally:

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
   answers ready. Each command's progress reports are held to the rule the
   Windows launcher applies to them: the gap between reports stays under
   the stall window and the whole command under its ceiling
   (`UPDATE_COMMAND_STALL_WINDOW_MS`, `TRIAL_STALL_WINDOW_MS` and their
   ceilings in `upgrade-corpus-config.ts`, pinned to the Go constants by
   `internal/wsllauncher/release_upgrade_mirror_test.go`). The gated boots
   are held to the launcher's probe deadline the same way.
4. The migrated store is compared with the launcher's snapshot, table by
   table, on the columns both schemas share: every pre-migration row must
   still be present unchanged (`EXCEPT` over the snapshot attached
   read-only), every table must still exist, and no table may shrink. A
   migration that intentionally rewrites, moves or drops old rows declares
   it in `migrationExceptions` with the migration version and the reason;
   the entry applies only while its version lies in the span a run
   migrates, and an entry at or below the corpus's version fails the run
   as stale, so the list empties itself as releases pass. Progress tables
   that delete their rows as a boot finishes their work are listed in
   `countIgnoredTables`.
5. A candidate harness boot on the same root reports the build's schema
   through `HarnessStoreStatus`, and `ListProjects`,
   `HarnessListThreadRows`, `ListArchivedThreads` and `ListThreadGroups`
   return the counts the previous release wrote.
6. A real browser page shows every project, shows thread rows, and opens the
   largest thread through the sidebar search until its first timeline row
   paints. No uncaught page error, and the frontend error log stays empty;
   `console.error` lines are reported, not asserted, because a real
   database's project paths do not exist on the checking machine.
7. The deferred migration phases, which start once a client has read the
   catalog, finish without a recorded failure.
8. After the candidate stops, the census is compared with the one before
   the migration: projects, threads, archived, pinned and groups are
   exactly what the previous release wrote; turns, items, payloads and
   payload bytes never shrink, in total or in any thread; every table
   still exists and none shrank, with the same exceptions as step 4. A
   repair may add rows (a carrier, a fork's own copy of shown history);
   the report carries every count's delta, tables included, so an
   addition is visible.
9. `__update-discard` commits the update, as the launcher does.
10. The candidate boots again and runs the top-up. A third census must show
    exactly the projects, archived project, pins and group the top-up adds,
    at least its threads, and no table shrunk; the report carries every
    delta (`topUpDeltas`) and the top-up's step timings.

Every phase's duration lands in the JSON report (`durations`), beside the
versions, the corpus figures, the gate's answer, the command cadences, the
fingerprint, the reads, the UI timings, the final deferred status, the
census deltas and the empty tables. The report is attached to the
Playwright result; `RELEASE_UPGRADE_REPORT=<file>` writes it elsewhere.

`e2e/tests/upgrade-corpus-config.ts` is the one file a migration author
touches: the mirrored launcher limits, `migrationExceptions`,
`fingerprintIgnoredTables` and `countIgnoredTables`. Every failure message
names the entry to add.

## Running it

```sh
make release-upgrade-check                                   # newest release, scale 1
make release-upgrade-check RELEASE_UPGRADE_SCALE=0.02        # smoke slice, seconds
make release-upgrade-check PREVIOUS_RELEASE=v0.0.16          # a named release
make release-upgrade-check RELEASE_UPGRADE_CORPUS=/clone     # a real data root
```

At scale 1 from v0.0.16 (474 threads, 649k items, a 2.7 GB database) a
run takes about two minutes on a developer laptop: about 105 s to seed,
4 s for the launcher's snapshot, under a second for the trial boot's
migrations from v139 to v141, 11 s for the fingerprint, under a second for
the candidate boot, and under a second until the 130k-row thread paints
its first row. The smoke slice takes about ten seconds.

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
- A command's reports stall or the command outruns its ceiling: the
  launcher would have killed this migration on a user's machine. Report
  progress more often or make the migration cheaper; the limit is the
  product's, not the check's.
- The fingerprint names a table with missing or changed pre-migration
  rows: a migration in the span altered old data. If that is the
  migration's intent, declare it in `migrationExceptions`; otherwise the
  migration lost or altered user data.
- A count differs after the migration: a migration dropped or duplicated
  rows. The census compares per thread, so the failing diff names the
  thread; a shrunken table names the `countIgnoredTables` or
  `rowsMayShrink` entry that would cover it.
- The config names a stale exception: its migration is already in the
  corpus, so remove the entry.
- The deferred phase records a failure: `HarnessStoreStatus` reports the
  version, title and first error. The phase skips a failing item and the
  next boot retries it, so this is a defect even when the boot looked fine.
- The timeline never paints or the frontend error log is non-empty: the
  migrated rows render into a client error. The Playwright trace carries
  the page.
- A top-up step fails on the migrated root: the failure names the step
  (a scenario turn that never rested, a workflow run and the state it is
  in). The same step passes on a fresh store in the coverage gate, so the
  difference is the migrated data.
- The coverage gate fails: the message lists what the corpus never writes
  and the allowlist entries that no longer hold, each with the instruction
  it needs.
