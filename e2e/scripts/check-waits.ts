// Runs the gate's wait rules (src/wait-scan.ts) over every spec the
// Playwright projects run (test-files.ts, which playwright.config.ts builds
// its projects from) and the suite files they import. The launcher
// (cmd/ao-harness-e2e/typecheck.go checkSuite) runs it with the typecheck, before
// Playwright, so a sleep or an expiring wait fails the run in about a second.
// Exits 1 with one line per finding.
import * as path from 'node:path';
import { MANUAL_SPEC, PROJECT_FILES, TEST_DIR } from '../test-files.ts';
import { checkWaitScanSamples, projectFiles, scanGate, specFiles } from '../src/wait-scan.ts';

const suite = path.resolve(import.meta.dirname, '..');
const problems = checkWaitScanSamples();
const { findings, unused } = scanGate(suite, specFiles(projectFiles({ testDir: TEST_DIR, testIgnore: MANUAL_SPEC, projects: Object.values(PROJECT_FILES) }, suite)));
for (const finding of findings) {
  problems.push(`${path.relative(suite, finding.file)}:${finding.line}: ${finding.rule}: ${finding.text}`);
}
for (const allowance of unused) {
  problems.push(`${allowance.file}: WAIT_ALLOWED names ${allowance.function}, which no longer holds for a time; remove the entry`);
}
if (problems.length > 0) {
  console.error(problems.join('\n'));
  console.error(
    `\n${problems.length} wait rule finding(s). A spec does not sleep or wait for a timeout to pass: give the product ` +
      'timer a knob (harnessTiming over main_harness.go harnessTimers), advance the page clock, poll the condition, or end an absence at a barrier ' +
      '(HarnessApp.watchEvents). It takes `test` from tests/base-test.ts or tests/fixtures.ts. See e2e/AGENTS.md "Writing specs".',
  );
  process.exit(1);
}
