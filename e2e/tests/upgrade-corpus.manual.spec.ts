// Release upgrade check (docs/architecture/release-upgrade-check.md): the
// candidate binary takes a large database the previous release wrote,
// migrates it through the launcher's real update path, boots on it, and
// serves its sidebar and threads. Operator-run, never a per-PR gate: the
// corpus is gigabytes and the run takes minutes to hours.
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
//     copied in instead. Its schema is whatever it is: when it already
//     matches the candidate the gate answers ready and the check covers
//     the boot, the reads and the deferred phases.
// Either way: the candidate's gated headless boot must refuse with
// migrations-pending (or report ready when nothing is pending);
// __update-snapshot and __update-trial-run must reach `prepared`; the next
// gated boot must be ready; a candidate harness boot on the same root must
// report the build's schema through HarnessStoreStatus, answer the
// project, thread, archived and group reads with the counts the previous
// release wrote, render the sidebar and open the largest thread in a real
// browser without an uncaught error, and finish every deferred phase
// without a failure. The census before and after is compared by
// compareCensus: entities exact, rows never fewer, in total or per thread.
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
} from './upgrade-corpus-config.js';
import { archiveCorpus, assertRelocated, restoreCorpus, type CorpusManifest } from './upgrade-corpus-archive.js';
import { corpusPlan, seedCorpus } from './upgrade-corpus-generator.js';
import {
  assertExceptionsCurrent,
  awaitDeferredSettled,
  census,
  compareCensus,
  fingerprint,
  largestThread,
  openThreadByTitle,
  type Census,
  type Fingerprint,
  type StoreStatus,
} from './upgrade-corpus-helpers.js';
import {
  assertCadence,
  cliCalls,
  killGroup,
  newUpdateID,
  prepareRig,
  runUpdateCommand,
  settledBootstrap,
  startHeadless,
  stopHeadless,
  updateResult,
  type Cadence,
  type HeadlessBackend,
} from './upgrade-corpus-launcher.js';
import { buildVersion, harnessTool, timed } from './upgrade-corpus-tools.js';
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
  /** The migration's effect on the snapshot's rows, table by table. */
  fingerprint: Fingerprint;
  /** Every census count after the run minus before it, tables included. */
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

    assertExceptionsCurrent(before.schemaVersion);

    // 2. The launcher's gate: a headless boot with --refuse-pending-migrations.
    const gated = await startHeadless(rig, 10 * MINUTE);
    headless.push(gated);
    const gate = await timed(() => settledBootstrap(gated, 30 * MINUTE, BOOT_PROBE_DEADLINE_MS));
    durations.gatedBootMs = gate.ms;
    durations.gatedBootMaxProgressGapMs = gate.value.maxProgressGapMs;
    await stopHeadless(gated);
    report.gate = { status: gate.value.status };
    const updateID = newUpdateID();
    if (gate.value.status === 409) {
      const pending = JSON.parse(gate.value.body) as PendingRefusal;
      report.gate.pending = pending;
      expect(pending).toMatchObject({ reason: 'migrations-pending', database: before.schemaVersion });
      expect(pending.pending).toBeGreaterThan(0);
      // Provider probes are unattended work: none ran before the refusal.
      expect(await cliCalls(rig)).toEqual([]);

      // 3. The launcher's migration: snapshot, then the real App trial boot,
      // each held to the stall rule its launcher judge applies.
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
      expect(trial.value.events.some((event) => event.progress?.phase === 'store.migrate'), trial.value.stderr).toBe(true);
      report.cadence.trial = assertCadence('__update-trial-run', trial.value.events, trialStarted, Date.now(),
        TRIAL_STALL_WINDOW_MS, TRIAL_CEILING_MS);
      expect(await cliCalls(rig)).toEqual([]);

      // The migration's own effect on the rows it was handed: every row of
      // the launcher's snapshot must still be in the migrated store, byte
      // for byte on the columns both schemas share, before any boot
      // serves it.
      const snapshotDB = path.join(rig.dataRoot, 'agent-overflow', 'runtime', 'app-update', 'snapshot', 'agent-overflow.db');
      await stat(snapshotDB);
      const compared = await timed(() => fingerprint(rig.dbPath, snapshotDB, before.schemaVersion, pending.build));
      durations.fingerprintMs = compared.ms;
      report.fingerprint = compared.value;

      // 4. The next gated boot finds nothing pending.
      const ready = await startHeadless(rig, 10 * MINUTE);
      headless.push(ready);
      const readiness = await timed(() => settledBootstrap(ready, 30 * MINUTE, BOOT_PROBE_DEADLINE_MS));
      durations.readyBootMs = readiness.ms;
      durations.readyBootMaxProgressGapMs = readiness.value.maxProgressGapMs;
      expect(readiness.value.status, readiness.value.body).toBe(200);
      await stopHeadless(ready);
    } else {
      expect(gate.value.status, gate.value.body).toBe(200);
    }
    const migrated = await census(rig.dbPath);
    expect(migrated.schemaVersion).toBe(report.gate.pending?.build ?? before.schemaVersion);

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
    // An empty list crosses the wire as null (a Go nil slice).
    const list = async <T,>(method: string): Promise<T[]> => (await candidate!.rpc<T[] | null>(method)) ?? [];
    const reads = await timed(async () => {
      const projects = await list<unknown>('ListProjects');
      const threads = await list<{ id: string; pinnedAt?: number }>('HarnessListThreadRows');
      const archived = await list<unknown>('ListArchivedThreads');
      const groups = await list<unknown>('ListThreadGroups');
      return {
        projects: projects.length,
        threads: threads.length,
        archived: archived.length,
        groups: groups.length,
        pinned: threads.filter((row) => row.pinnedAt != null).length,
      };
    });
    durations.readsMs = reads.ms;
    report.reads = reads.value;
    expect(reads.value).toEqual({
      projects: before.projects,
      threads: before.threads,
      archived: before.archived,
      groups: before.groups,
      pinned: before.pinned,
    });

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

    // 8. Nothing the previous release wrote is gone.
    const after = await census(rig.dbPath);
    expect(after.schemaVersion).toBe(atBoot.schemaVersion);
    report.censusDeltas = compareCensus(before, after, before.schemaVersion, after.schemaVersion);
    report.emptyTables = Object.entries(after.tables).filter(([, n]) => n === 0).map(([name]) => name);

    // 9. The launcher commits by discarding the snapshot.
    if (report.gate.status === 409) {
      const discard = await runUpdateCommand(rig, ['__update-discard', '--id', updateID], 10 * MINUTE);
      expect(updateResult(discard.events), discard.stderr).toMatchObject({ type: 'result', outcome: 'ok' });
    }

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
    report.topUpDeltas = compareCensus(after, final, after.schemaVersion, final.schemaVersion, topped.value.growth);
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

