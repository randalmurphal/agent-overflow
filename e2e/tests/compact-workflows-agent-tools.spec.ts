// A run started and watched from a thread through the workflow tools
// (docs/specs/agent-workflow-tools.md), under the compact layout: the
// run_start row and the wake that reported the run's rest read on a
// phone-width thread screen. The tool contract itself is
// workflows-agent-tools.spec.ts's.

import { test, expect } from './fixtures.js';
import {
  answerPhasesDone,
  captureRunFromThread,
  doneFlowYaml,
  seedToolsProject,
  startAndWatchFromThread,
} from './workflows-agent-tools-helpers.js';

test('a run started and watched through the workflow tools reads on the compact thread screen', async ({
  harness,
  page,
}) => {
  await answerPhasesDone(harness, 'codex');
  const project = await seedToolsProject(
    harness,
    'workflow-tools-compact',
    [{ name: 'importer', yaml: doneFlowYaml('importer', 'codex') }],
    { title: 'Workflow driver', provider: 'claude' },
  );
  const { start } = await startAndWatchFromThread(harness, 'claude', project, 'importer');
  expect(start.outcome).toBe('resting');

  await captureRunFromThread(harness, page, 'Workflow driver', start.start.itemId, 'workflow-tools-compact.png');
  const viewport = page.viewportSize()!;
  const row = page.getByTestId('tool-call-card').filter({ hasText: 'importer' }).first();
  const box = (await row.boundingBox())!;
  expect(box.x).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width).toBeLessThanOrEqual(viewport.width);
});
