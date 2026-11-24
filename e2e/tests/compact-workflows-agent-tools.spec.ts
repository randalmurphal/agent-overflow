// A run started and watched from a thread through the workflow tools
// (docs/specs/agent-workflow-tools.md), under the compact layout: the
// backgrounded run_start row, the wake that reported the run's rest and the
// turn that read it, on a phone-width thread screen. The setup is the one run
// that puts them there; the tool contract, on both providers, is
// workflows-agent-tools.spec.ts's.

import { test, expect } from './fixtures.js';
import { setScenario, threadToolsScenario } from './thread-tools-helpers.js';
import {
  captureRunFromThread,
  doneFlowYaml,
  finishMigration,
  migratorTurns,
  seedToolsProject,
  sendMigration,
  threadScenario,
} from './workflows-agent-tools-helpers.js';

test('a run started and watched through the workflow tools reads on the compact thread screen', async ({
  harness,
  page,
}) => {
  const project = await seedToolsProject(
    harness,
    'workflow-tools-compact',
    [{ name: 'migrator', yaml: doneFlowYaml('migrator', 'codex') }],
    { title: 'Workflow driver', provider: 'claude' },
  );
  await setScenario(
    harness,
    project.path,
    threadToolsScenario({
      name: threadScenario('claude'),
      provider: 'claude',
      echoUserMessage: true,
      turns: migratorTurns(),
      afterTurns: 'repeatLast',
    }),
  );
  await sendMigration(harness, 'claude', project);
  const { backgrounded } = await finishMigration(harness, 'claude', project);

  await captureRunFromThread(harness, page, 'Workflow driver', backgrounded.start.itemId, 'workflow-tools-compact.png');
  const viewport = page.viewportSize()!;
  const row = page.getByTestId('tool-call-card').filter({ hasText: 'migrator' }).first();
  const box = (await row.boundingBox())!;
  expect(box.x).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width).toBeLessThanOrEqual(viewport.width);
});
