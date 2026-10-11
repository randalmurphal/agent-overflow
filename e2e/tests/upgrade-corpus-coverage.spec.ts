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
// spec left behind would count as coverage the corpus does not have.
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import { COVERAGE_CORPUS_SCALE, coverageAllowlist } from './upgrade-corpus-config.js';
import { assertCoverage, assertScenariosClassified, measureCoverage, readSchema } from './upgrade-corpus-coverage.js';
import { corpusPlan, seedCorpus } from './upgrade-corpus-generator.js';
import { topUp } from './upgrade-corpus-topup.js';
import { buildVersion } from './upgrade-corpus-tools.js';

test('the corpus and the top-up write everything the schema can hold, or the allowlist says why not', async () => {
  test.setTimeout(5 * 60_000);
  let harness: HarnessApp | undefined;
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
    const coverage = await measureCoverage(info.dbPath, schema);
    const report = assertCoverage(schema, coverage, coverageAllowlist);
    expect(report).toEqual({ uncovered: [], stale: [] });
  } finally {
    await harness?.close();
  }
});
