// Release upgrade check (docs/architecture/release-upgrade-check.md): the
// candidate binary takes a large database the previous release wrote,
// migrates it through the launcher's real update path, boots on it, and
// serves its sidebar and threads. A release gate and an operator tool,
// run per pull request only for changes that reach the store or the check:
// the corpus is gigabytes and the run takes minutes.
//
// Modes, chosen by environment:
//   AO_RELEASE_UPGRADE_PREVIOUS_BIN=<headless binary of the previous release>
//     The previous release boots in harness mode on a fresh data root and
//     seeds a synthetic corpus (upgrade-corpus-helpers.ts) at
//     AO_RELEASE_UPGRADE_SCALE times the reference shape (1 is a year of
//     real use, about 2 GB), so the database is at that release's schema
//     by construction. Needs a release with the `repeat` and `padBefore`
//     seed fields (v0.0.16 and later).
//   AO_RELEASE_UPGRADE_CORPUS=<data root>
//     An existing data root (an `ao-harness clone` of a real database) is
//     copied in instead. Its schema is whatever it is.
// Either way: a root behind the build's schema must make the gated
// headless boot refuse with migrations-pending; __update-snapshot and
// __update-trial-run must reach `prepared` (on a root already at the
// build's schema too, before the gate answers ready); every old row the
// trial changed must be one a migration exception or a recovery rule of
// its boot explains (upgrade-corpus-fingerprint.ts); the next gated boot
// must be ready; a candidate harness boot on the same root must report the
// build's schema through HarnessStoreStatus, answer the project, thread,
// archived and group reads with the counts the previous release wrote less
// the threads a sweep rule removes, render the sidebar and open the largest
// thread in a real browser without an uncaught error, and finish every
// deferred phase without a failure. compareCensus holds entities exact and
// rows never fewer, in total or per thread, across the migrations (less the
// recovery rules' removals), the boots after the trial (less the sweep
// rules') and the top-up.
// AO_RELEASE_UPGRADE_REPORT names the JSON report (default: the test's
// output dir); AO_RELEASE_UPGRADE_ROOT is where the sandbox is created;
// AO_RELEASE_UPGRADE_KEEP=1 keeps it.
import { cp, mkdir, readFile, rm, stat, writeFile } from 'node:fs/promises';
import * as path from 'node:path';
import { expect, test } from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import {
  BOOT_PROBE_DEADLINE_MS,
  TRIAL_CEILING_MS,
  TRIAL_STALL_WINDOW_MS,
  UPDATE_COMMAND_CEILING_MS,
  UPDATE_COMMAND_STALL_WINDOW_MS,
  type SweepContext,
} from './upgrade-corpus-config.js';
import { archiveCorpus, assertRelocated, restoreCorpus, type CorpusManifest } from './upgrade-corpus-archive.js';
import { corpusPlan, seedCorpus } from './upgrade-corpus-generator.js';
import {
  assertExceptionsCurrent,
  awaitDeferredSettled,
  census,
  compareCensus,
  largestThread,
  openThreadByTitle,
  expectedCatalog,
  readCatalog,
  sweepBaseline,
  sweepGrowth,
  type Census,
  type StoreStatus,
  type SweepBaseline,
  type SweepCount,
} from './upgrade-corpus-helpers.js';
import { buildSchema, fingerprint, migrateAlone, type Fingerprint } from './upgrade-corpus-fingerprint.js';
import {
  assertCadence,
  cliCalls,
  killGroup,
  newUpdateID,
  runUpdateCommand,
  settledBootstrap,
  startHeadless,
  stopHeadless,
  updateResult,
  type Cadence,
  type HeadlessBackend,
} from './update-launcher-helpers.js';
import { buildVersion, harnessTool, prepareRig, timed } from './upgrade-corpus-tools.js';
import { topUp } from './upgrade-corpus-topup.js';

const previousBinary = process.env.AO_RELEASE_UPGRADE_PREVIOUS_BIN;
const corpusRoot = process.env.AO_RELEASE_UPGRADE_CORPUS;
const archivePath = process.env.AO_RELEASE_UPGRADE_ARCHIVE;
const archiveOut = process.env.AO_RELEASE_UPGRADE_ARCHIVE_OUT;
const scale = Number(process.env.AO_RELEASE_UPGRADE_SCALE ?? '1');
const MINUTE = 60_000;

interface PendingRefusal {
  reason: string;
  database: number;
  build: number;
  pending: number;
}

interface Report {
  mode: 'synthetic' | 'corpus' | 'archive';
  previous: { binary?: string; archive?: string; version?: string; schemaVersion: number };
  candidate: { version: string; schemaVersion: number };
  corpus: { scale?: number; threads: number; archived: number; groups: number; pinned: number; items: number; payloadBytes: number; dbBytes: number };
  gate: { status: number; pending?: PendingRefusal };
  durations: Record<string, number | number[]>;
  reads: { projects: number; threads: number; archived: number; groups: number; pinned: number };
  ui: {
    visibleProjects: number;
    visibleThreadRows: number;
    largestThread: { title: string; items: number; firstRowMs: number };
    consoleErrors: string[];
  };
  /** The store status right after the candidate boot, before any client read. */
  deferredAtBoot: StoreStatus;
  /** The store status once the deferred phases settled. */
  deferred: StoreStatus;
  /** Report cadence of the launcher's commands against their stall rules. */
  cadence: { snapshot: Cadence; trial?: Cadence };
  /** The trial's effect on the snapshot's rows: by table, by cause, and by recovery rule. */
  fingerprint: Fingerprint;
  /** Every census count after the migrations and the trial minus before them, tables included. */
  migrationDeltas: Record<string, number>;
  /** What each sweep rule let the boots after the trial remove. */
  sweeps: SweepCount[];
  /** Every census count after the candidate's boots minus after the trial, tables included. */
  censusDeltas: Record<string, number>;
  emptyTables: string[];
  /** The top-up's step timings and what it added. */
  topUp: { steps: Record<string, number>; threads: number; projectId: string };
  /** Every census count after the top-up minus before it. */
  topUpDeltas: Record<string, number>;
  /** The archive written for the next release, when one was asked for. */
  archive?: CorpusManifest & { path: string; bytes: number };
  cliCalls: string[];
}

test('the candidate migrates a large previous-release database and boots on it', async ({ page }) => {
  test.skip(!previousBinary && !corpusRoot && !archivePath,
    'set AO_RELEASE_UPGRADE_ARCHIVE, AO_RELEASE_UPGRADE_PREVIOUS_BIN or AO_RELEASE_UPGRADE_CORPUS');
  if (corpusRoot && archiveOut) {
    throw new Error('a real data root is never archived; AO_RELEASE_UPGRADE_ARCHIVE_OUT is for archive and synthetic runs');
  }
  test.setTimeout(4 * 60 * MINUTE);
  const rig = await prepareRig(process.env.AO_RELEASE_UPGRADE_ROOT);
  const durations: Record<string, number | number[]> = {};
  const headless: HeadlessBackend[] = [];
  let candidate: HarnessApp | undefined;
  let previous: HarnessApp | undefined;
  const report: Partial<Report> = { durations };
  try {
    // 1. The database the previous release wrote.
    let before: Census;
    if (archivePath) {
      // The corpus the previous release published: its own migrated,
      // topped-up root, restored at the root it was made at.
      report.mode = 'archive';
      const restored = await timed(() => restoreCorpus(rig, archivePath));
      durations.restoreMs = restored.ms;
      await assertRelocated(rig);
      before = await census(rig.dbPath);
      expect(before.schemaVersion).toBe(restored.value.schemaVersion);
      report.previous = { archive: archivePath, version: restored.value.release, schemaVersion: before.schemaVersion };
      report.corpus = { ...corpusCounts(before), dbBytes: (await stat(rig.dbPath)).size };
    } else if (corpusRoot) {
      report.mode = 'corpus';
      const source = path.join(corpusRoot, 'agent-overflow');
      await stat(path.join(source, 'agent-overflow.db'));
      const copy = await timed(() => cp(source, path.join(rig.dataRoot, 'agent-overflow'), { recursive: true, force: true }));
      durations.copyCorpusMs = copy.ms;
      before = await census(rig.dbPath);
      report.previous = { schemaVersion: before.schemaVersion };
      report.corpus = { ...corpusCounts(before), dbBytes: (await stat(rig.dbPath)).size };
    } else {
      report.mode = 'synthetic';
      const plan = corpusPlan(scale);
      const boot = await timed(() => launchHarness({
        binary: previousBinary,
        savedRelease: true,
        mockProvider: harnessTool('ao-mockprovider'),
        mockForge: '',
        dataDir: rig.dataRoot,
        timeoutMs: 2 * MINUTE,
        // The previous release refuses a timing name it does not know.
        env: { AO_HARNESS_TIMING: '' },
      }));
      previous = boot.value;
      durations.previousBootMs = boot.ms;
      const seeded = await timed(() => seedCorpus(previous!, plan, 60 * MINUTE));
      durations.seedMs = seeded.ms;
      durations.seedProjectMs = seeded.value.projectSeedMs;
      const stop = await timed(() => previous!.stop());
      expect(stop.value, 'the previous release did not stop').toBe(true);
      durations.previousStopMs = stop.ms;
      before = await census(rig.dbPath);
      report.previous = { binary: previousBinary, version: previous.bootstrap.version, schemaVersion: before.schemaVersion };
      previous = undefined;
      report.corpus = { scale, ...corpusCounts(before), dbBytes: (await stat(rig.dbPath)).size };
      // What the previous release wrote is what the plan asked for.
      expect(before.threads + before.archived).toBe(plan.threads);
      expect(before.archived).toBe(plan.archived);
      expect(before.groups).toBe(plan.groups);
      expect(before.pinned).toBe(plan.pinned);
      expect(before.items).toBe(plan.items);
      expect(before.childItems).toBe(0);
    }
    expect(before.threads, 'the corpus has no thread to open').toBeGreaterThan(0);
    const largest = await largestThread(rig.dbPath);
    const candidateVersion = await buildVersion();

    // The stale-entry rule is the lineage's: a real data root can sit at any
    // version, ahead of the newest release included, and says nothing about
    // which entries the next release's corpus has crossed.
    if (!corpusRoot) assertExceptionsCurrent(before.schemaVersion);

    // 2. Whether the candidate's gated boot would migrate: a root already
    // at the build's schema boots straight through, settling what its
    // last process left live, so its trial runs first (step 3) to read that
    // boot against a snapshot; a root behind it is refused, as the launcher
    // sees it, and then migrated.
    const schemaDir = path.join(rig.root, 'build-schema');
    const buildSchemaVersion = await buildSchema(schemaDir).finally(() => rm(schemaDir, { recursive: true, force: true }));
    const current = before.schemaVersion === buildSchemaVersion;
    const updateID = newUpdateID();
    // The census after the trial, and what the later boots' sweeps select,
    // which the census and the reads allow.
    let postTrial = before;
    let swept: SweepBaseline = { threads: {}, rows: [] };
    const gateBoot = async (name: 'gatedBoot' | 'readyBoot') => {
      const gated = await startHeadless(rig, 10 * MINUTE);
      headless.push(gated);
      const gate = await timed(() => settledBootstrap(gated, 30 * MINUTE, BOOT_PROBE_DEADLINE_MS));
      durations[`${name}Ms`] = gate.ms;
      durations[`${name}MaxProgressGapMs`] = gate.value.maxProgressGapMs;
      await stopHeadless(gated);
      return gate.value;
    };

    // 3. The launcher's update: snapshot, then the real App trial boot,
    // each held to the stall rule its launcher judge applies. Then the
    // trial's effect on the rows it was handed: every row of the launcher's
    // snapshot must still be in the store, byte for byte on the columns
    // both schemas share, unless a migration exception or a recovery rule
    // of the trial's boot explains the change.
    const trialAndCompare = async (to: number) => {
      const snapshotStarted = Date.now();
      const snapshot = await timed(() => runUpdateCommand(rig, ['__update-snapshot', '--id', updateID], UPDATE_COMMAND_CEILING_MS));
      durations.snapshotMs = snapshot.ms;
      expect(updateResult(snapshot.value.events), snapshot.value.stderr)
        .toMatchObject({ type: 'result', outcome: 'ok', schema: before.schemaVersion });
      report.cadence = {
        snapshot: assertCadence('__update-snapshot', snapshot.value.events, snapshotStarted, Date.now(),
          UPDATE_COMMAND_STALL_WINDOW_MS, UPDATE_COMMAND_CEILING_MS),
      };
      const trialStarted = Date.now();
      const trial = await timed(() => runUpdateCommand(rig,
        ['__update-trial-run', '--id', updateID, '--to', candidateVersion, '--attempt', '1'], UPDATE_COMMAND_CEILING_MS));
      durations.trialMs = trial.ms;
      expect(updateResult(trial.value.events), trial.value.stderr).toMatchObject({ type: 'result', outcome: 'prepared' });
      expect(trial.value.events.some((event) => event.progress?.phase === 'store.migrate'), trial.value.stderr).toBe(to > before.schemaVersion);
      report.cadence.trial = assertCadence('__update-trial-run', trial.value.events, trialStarted, Date.now(),
        TRIAL_STALL_WINDOW_MS, TRIAL_CEILING_MS);
      expect(await cliCalls(rig)).toEqual([]);

      const snapshotDB = path.join(rig.dataRoot, 'agent-overflow', 'runtime', 'app-update', 'snapshot', 'agent-overflow.db');
      await stat(snapshotDB);
      const alone = path.join(rig.root, 'migrated-alone');
      const compared = await timed(() => fingerprint(rig.dbPath, snapshotDB, before.schemaVersion, to,
        () => migrateAlone(snapshotDB, alone)).finally(() => rm(alone, { recursive: true, force: true })));
      durations.fingerprintMs = compared.ms;
      report.fingerprint = compared.value;
      if (compared.value.failures.length > 0) throw new Error(compared.value.failures.join('\n'));
      // The migrations and the trial lost nothing beyond what the
      // recovery rules removed; what the next ordinary boots sweep is read
      // before any boot does.
      postTrial = await census(rig.dbPath);
      report.migrationDeltas = compareCensus(before, postTrial, before.schemaVersion, postTrial.schemaVersion, 'migration',
        { removed: compared.value.recoveryRemoved });
      swept = await sweepBaseline(rig.dbPath);
    };

    if (current) {
      await trialAndCompare(before.schemaVersion);
      // 4. The gate answers ready.
      const gate = await gateBoot('gatedBoot');
      report.gate = { status: gate.status };
      expect(gate.status, gate.body).toBe(200);
    } else {
      // 4. The launcher's gate refuses, the update runs, and the next gated
      // boot finds nothing pending.
      const gate = await gateBoot('gatedBoot');
      report.gate = { status: gate.status };
      expect(gate.status, gate.body).toBe(409);
      const pending = JSON.parse(gate.body) as PendingRefusal;
      report.gate.pending = pending;
      expect(pending).toMatchObject({ reason: 'migrations-pending', database: before.schemaVersion, build: buildSchemaVersion });
      expect(pending.pending).toBeGreaterThan(0);
      // Provider probes are unattended work: none ran before the refusal.
      expect(await cliCalls(rig)).toEqual([]);
      await trialAndCompare(pending.build);
      const readiness = await gateBoot('readyBoot');
      expect(readiness.status, readiness.body).toBe(200);
    }
    const migrated = await census(rig.dbPath);
    expect(migrated.schemaVersion).toBe(buildSchemaVersion);

    // 5. The candidate boots on the migrated root and answers the reads.
    const boot = await timed(() => launchHarness({ dataDir: rig.dataRoot, mockProvider: harnessTool('ao-mockprovider'), mockForge: harnessTool('ao-mockforge'), timeoutMs: 30 * MINUTE }));
    candidate = boot.value;
    durations.candidateBootMs = boot.ms;
    const atBoot = await candidate.rpc<StoreStatus>('HarnessStoreStatus');
    expect(atBoot.schemaVersion).toBe(atBoot.buildSchemaVersion);
    expect(atBoot.schemaVersion).toBe(migrated.schemaVersion);
    expect(atBoot.deferredFailure).toBeNull();
    report.candidate = { version: candidate.bootstrap.version, schemaVersion: atBoot.schemaVersion };
    report.deferredAtBoot = atBoot;
    // What the rowSweepRules conditions read: the projects' forge origins as the candidate resolves them.
    const live: SweepContext = { forgeProjects: (await candidate.rpc<SweepContext['forgeProjects'] | null>('ListForgeProjects')) ?? [] };
    const reads = await timed(() => readCatalog(candidate!, swept));
    durations.readsMs = reads.ms;
    report.reads = reads.value;
    expect(reads.value).toEqual(expectedCatalog(postTrial, swept));

    // 6. The page: every project in the sidebar, rows, the largest thread.
    // Uncaught page errors are caught at the page as well as from the
    // backend's error log, which the page reports in batches a short run
    // may not flush. console.error lines are reported, not asserted: a
    // corpus whose project paths do not exist on this machine logs its
    // git status refusals there.
    const pageErrors: string[] = [];
    const consoleErrors: string[] = [];
    page.on('pageerror', (error) => pageErrors.push(error.message));
    page.on('console', (message) => {
      if (message.type() === 'error') consoleErrors.push(message.text());
    });
    const open = await timed(() => candidate!.open(page));
    durations.pageOpenMs = open.ms;
    await expect(page.getByTestId('project-item')).toHaveCount(before.projects, { timeout: 5 * MINUTE });
    await expect(page.getByTestId('thread-row').first()).toBeVisible({ timeout: 5 * MINUTE });
    const visibleThreadRows = await page.getByTestId('thread-row').count();
    const token = /#\d+ of \d+/.exec(largest.title)?.[0] ?? largest.title;
    const firstRowMs = await openThreadByTitle(page, largest.title, token, 5 * MINUTE);
    report.ui = {
      visibleProjects: before.projects,
      visibleThreadRows,
      // A real database's titles are the user's; the report carries only
      // the corpus's own.
      largestThread: { title: corpusRoot ? '(corpus title withheld)' : largest.title, items: largest.items, firstRowMs },
      consoleErrors,
    };

    // 7. The deferred phases run once a client has read the catalog.
    const deferred = await timed(() => awaitDeferredSettled(candidate!, 2 * 60 * MINUTE));
    durations.deferredMs = deferred.ms;
    report.deferred = deferred.value;
    const info = await candidate.rpc<{ frontendErrorsPath: string }>('HarnessInfo');
    const frontendErrors = (await readFile(info.frontendErrorsPath, 'utf8').catch(() => '')).split('\n').filter(Boolean);
    expect(frontendErrors, 'the page recorded frontend errors').toEqual([]);
    expect(pageErrors, 'the page threw an uncaught error').toEqual([]);
    await page.close();
    const stop = await timed(() => candidate!.stop());
    expect(stop.value, 'the candidate did not stop').toBe(true);
    durations.candidateStopMs = stop.ms;
    candidate = undefined;

    // 8. Nothing the previous release wrote is gone: the boots after the
    // trial lost only what a sweep rule lets them.
    const after = await census(rig.dbPath);
    expect(after.schemaVersion).toBe(atBoot.schemaVersion);
    const sweeps = await sweepGrowth(swept, rig.dbPath, live);
    report.sweeps = sweeps.report;
    report.censusDeltas = compareCensus(postTrial, after, postTrial.schemaVersion, after.schemaVersion, 'boot', sweeps.growth);
    report.emptyTables = Object.entries(after.tables).filter(([, n]) => n === 0).map(([name]) => name);

    // 9. The launcher commits by discarding the snapshot.
    const discard = await runUpdateCommand(rig, ['__update-discard', '--id', updateID], 10 * MINUTE);
    expect(updateResult(discard.events), discard.stderr).toMatchObject({ type: 'result', outcome: 'ok' });

    // 10. The top-up: the candidate writes its own slice of use into the
    // migrated store, natively at its schema, so the corpus the next
    // release inherits carries rows of everything this release writes.
    const reboot = await timed(() => launchHarness({ dataDir: rig.dataRoot, mockProvider: harnessTool('ao-mockprovider'), mockForge: harnessTool('ao-mockforge'), timeoutMs: 30 * MINUTE }));
    candidate = reboot.value;
    durations.topUpBootMs = reboot.ms;
    const topped = await timed(() => topUp(candidate!, candidateVersion));
    durations.topUpMs = topped.ms;
    report.topUp = { steps: topped.value.ms, threads: topped.value.threadIds.length, projectId: topped.value.projectId };
    const stopTopped = await timed(() => candidate!.stop());
    expect(stopTopped.value, 'the candidate did not stop after the top-up').toBe(true);
    durations.topUpStopMs = stopTopped.ms;
    candidate = undefined;
    const final = await census(rig.dbPath);
    report.topUpDeltas = compareCensus(after, final, after.schemaVersion, final.schemaVersion, 'top-up', topped.value.growth);
    report.cliCalls = await cliCalls(rig);

    // 11. The corpus the next release inherits: this root, as the candidate
    // left it, at the candidate's schema.
    if (archiveOut) {
      const dbBytes = (await stat(rig.dbPath)).size;
      const packed = await timed(() => archiveCorpus(rig, archiveOut, {
        release: candidateVersion,
        schemaVersion: final.schemaVersion,
        threads: final.threads + final.archived,
        items: final.items,
        dbBytes,
      }));
      durations.archiveMs = packed.ms;
      report.archive = { ...packed.value, path: archiveOut, bytes: (await stat(archiveOut)).size };
    }
  } finally {
    for (const b of headless) {
      await stopHeadless(b).catch(() => killGroup(b.child));
    }
    await previous?.stop().catch(() => undefined);
    await candidate?.stop().catch(() => undefined);
    const reportPath = process.env.AO_RELEASE_UPGRADE_REPORT ?? test.info().outputPath('release-upgrade-report.json');
    await mkdir(path.dirname(reportPath), { recursive: true });
    await writeFile(reportPath, JSON.stringify(report, null, 2) + '\n');
    await test.info().attach('release-upgrade-report', { path: reportPath, contentType: 'application/json' });
    if (process.env.AO_RELEASE_UPGRADE_KEEP === '1') {
      test.info().annotations.push({ type: 'sandbox', description: rig.root });
    } else {
      await rm(rig.root, { recursive: true, force: true });
    }
  }
});

function corpusCounts(c: Census): Omit<Report['corpus'], 'scale' | 'dbBytes'> {
  return {
    threads: c.threads + c.archived,
    archived: c.archived,
    groups: c.groups,
    pinned: c.pinned,
    items: c.items,
    payloadBytes: c.payloadBytes,
  };
}

