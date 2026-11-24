// The upgrade corpus coverage gate. The release upgrade check
// (docs/architecture/release-upgrade-check.md) migrates the corpus the
// previous release published and tops it up with the candidate's own
// writes; this gate, on every pull request, proves the generator and the
// top-up still write every table, every column and every enumerated CHECK
// value the current schema can hold, or that coverageAllowlist says why
// not, and that every mock scenario is classified. A schema change lands
// with the top-up step or the allowlist entry that keeps the corpus
// representative; the failure names which.
//
// The gate measures a backend of its own, not the worker's: a row another
// spec left behind would count as coverage the corpus does not have. A
// failure leaves that backend's diagnostics and its store
// (agent-overflow.db with its -wal and -shm files) in the test's output
// directory, which CI uploads; AO_E2E_KEEP_FAILED_ROOT=1 keeps its whole
// data root as well.
import { access, copyFile, rm } from 'node:fs/promises';
import * as path from 'node:path';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import type { TestInfo } from '@playwright/test';
import { expect, test } from './fixtures.js';
import { COVERAGE_CORPUS_SCALE, coverageAllowlist } from './upgrade-corpus-config.js';
import { assertCoverage, assertScenariosClassified, measureCoverage, readSchema } from './upgrade-corpus-coverage.js';
import { corpusPlan, seedCorpus } from './upgrade-corpus-generator.js';
import { topUp } from './upgrade-corpus-topup.js';
import { buildVersion } from './upgrade-corpus-tools.js';

test('the corpus and the top-up write everything the schema can hold, or the allowlist says why not', async ({}, testInfo) => {
  test.setTimeout(5 * 60_000);
  let harness: HarnessApp | undefined;
  let failed = false;
  try {
    harness = await launchHarness();
    const info = await harness.rpc<{ dbPath: string }>('HarnessInfo');
    const library = await harness.rpc<{ library: Array<{ name: string }> }>('HarnessListScenarios');
    assertScenariosClassified(library.library.map((entry) => entry.name));

    const plan = corpusPlan(COVERAGE_CORPUS_SCALE);
    await seedCorpus(harness, plan, 60_000);
    const topped = await topUp(harness, await buildVersion());
    test.info().annotations.push({ type: 'top-up', description: JSON.stringify(topped.ms) });

    const schema = await readSchema(info.dbPath);
    expect(schema.length).toBeGreaterThan(0);
    const coverage = await measureCoverage(info.dbPath, schema, coverageAllowlist);
    const report = assertCoverage(schema, coverage, coverageAllowlist);
    expect(report).toEqual({ uncovered: [], stale: [] });
  } catch (error) {
    failed = true;
    throw error;
  } finally {
    if (harness && failed) await keepFailedStore(harness, testInfo);
    else await harness?.close();
  }
});

/**
 * keepFailedStore stops a failed run's backend with its data root in
 * place and writes its diagnostics and its store into the test's output
 * directory. The store is the database with its -wal and -shm files,
 * copied only once close() has verified the backend's process tree is
 * gone: a stop that had to kill the backend leaves committed rows in the
 * WAL alone. The copy is the store as the backend's shutdown left it, not
 * as the failing assertion saw it. The root is removed only when all three
 * copied (a source file that does not exist counts as copied) and
 * AO_E2E_KEEP_FAILED_ROOT is not 1; otherwise it stays as the evidence. A
 * backend that survives keeps its root and gets no copy. Every part that
 * fails is attached to the test instead of thrown, so the test's own
 * failure stays the one it reports.
 */
async function keepFailedStore(harness: HarnessApp, testInfo: TestInfo): Promise<void> {
  const dir = testInfo.outputDir;
  const problems: string[] = [];
  const attempt = async (label: string, step: () => Promise<void>): Promise<boolean> => {
    try {
      await step();
      return true;
    } catch (error) {
      problems.push(`${label}: ${(error as Error).message}`);
      return false;
    }
  };
  harness.keepDataRoot();
  await attempt('diagnostics', () => harness.writeDiagnostics(dir));
  if (await attempt('stop', () => harness.close())) {
    const db = path.join(harness.bootstrap.dataDir, 'agent-overflow.db');
    let complete = true;
    for (const suffix of ['', '-wal', '-shm']) {
      const copied = await attempt(`store${suffix}`, () => copyStoreFile(db + suffix, path.join(dir, `agent-overflow.db${suffix}`)));
      complete &&= copied;
    }
    if (complete && process.env.AO_E2E_KEEP_FAILED_ROOT !== '1') {
      await attempt('data root removal', () => rm(harness.bootstrap.dataRoot, { recursive: true, force: true }));
    } else if (!complete) {
      problems.push(`data root kept at ${harness.bootstrap.dataRoot}: the store did not copy whole`);
    }
  }
  if (problems.length > 0) {
    await testInfo.attach('failed store not kept', { body: problems.join('\n'), contentType: 'text/plain' });
  }
}

/**
 * copyStoreFile copies one file of the store. A source that does not exist
 * (a WAL a clean close removed) copies nothing; any other failure, on
 * either side, is thrown.
 */
async function copyStoreFile(from: string, to: string): Promise<void> {
  try {
    await access(from);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return;
    throw error;
  }
  await copyFile(from, to);
}
