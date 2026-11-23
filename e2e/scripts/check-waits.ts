// Runs the gate's wait rules (src/wait-scan.ts) over the suite's sources.
// The launcher (cmd/ao-harness-e2e) runs it after the typecheck and before
// Playwright, so a sleep or an expiring wait fails every run in about a
// second. Exits 1 with one line per finding.
import * as path from 'node:path';
import { checkWaitScanSamples, gateSources, scanWaits } from '../src/wait-scan.ts';

const suite = path.resolve(import.meta.dirname, '..');
const problems = checkWaitScanSamples();
for (const source of gateSources(path.join(suite, 'tests'))) {
  for (const finding of scanWaits(source)) {
    problems.push(`${path.relative(suite, finding.file)}:${finding.line}: ${finding.rule}: ${finding.text}`);
  }
}
if (problems.length > 0) {
  console.error(problems.join('\n'));
  console.error(
    `\n${problems.length} wait rule finding(s). A spec does not sleep or wait for a timeout to pass: give the product ` +
      'timer a knob (harnessTiming over main_harness.go harnessTimers), advance the page clock, poll the condition, or end an absence at a barrier ' +
      '(HarnessApp.watchEvents). See e2e/AGENTS.md "Writing specs".',
  );
  process.exit(1);
}
