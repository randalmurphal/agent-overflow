// A run started and watched from a thread through the workflow tools
// (docs/specs/agent-workflow-tools.md), under the compact layout: the
// backgrounded run_start row, the wake that reported the run's rest and the
// turn that read it, on a phone-width thread screen. The tool contract itself is
// workflows-agent-tools.spec.ts's.

import { test, expect } from './fixtures.js';
import {
  captureRunFromThread,
  doneFlowYaml,
  seedToolsProject,
  startAndWatchFromThread,
} from './workflows-agent-tools-helpers.js';

test('runs started and watched through the workflow tools read on the compact thread screen', async ({
  harness,
  page,
}) => {
  const project = await seedToolsProject(
    harness,
    'workflow-tools-compact',
    [
      { name: 'importer', yaml: doneFlowYaml('importer', 'codex') },
      { name: 'migrator', yaml: doneFlowYaml('migrator', 'codex') },
    ],
    { title: 'Workflow driver', provider: 'claude' },
  );
  const runs = await startAndWatchFromThread(harness, 'claude', project);
  expect(runs.rested.outcome).toBe('resting');
  expect(runs.backgrounded.outcome).toBe('backgrounded');

  await captureRunFromThread(harness, page, 'Workflow driver', runs.backgrounded.start.itemId, 'workflow-tools-compact.png');
  const viewport = page.viewportSize()!;
  const row = page.getByTestId('tool-call-card').filter({ hasText: 'migrator' }).first();
  const box = (await row.boundingBox())!;
  expect(box.x).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width).toBeLessThanOrEqual(viewport.width);
});
