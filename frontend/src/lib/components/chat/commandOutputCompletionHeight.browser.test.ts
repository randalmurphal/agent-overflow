import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import '../../../app.css';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import CommandOutput from './CommandOutput.svelte';

describe('command output completion geometry', () => {
  it('keeps a foreground Claude Bash row at one height across completion', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-height-probe' }));
    const createdAt = Date.now() - 5_000;
    const running = makeItem({
      id: 'bash-height-probe',
      threadId: 'thread-height-probe',
      kind: 'tool_call',
      toolName: 'Bash',
      status: 'running',
      summary: 'Bash: sleep 1',
      createdAt,
      updatedAt: createdAt,
    });
    const completed = makeItem({
      ...running,
      status: 'completed',
      updatedAt: createdAt + 5_000,
    });
    const meta = { command: 'sleep 1', exitCode: 0, lineCount: 0, preview: '' };

    const view = render(CommandOutput, { props: { pane, item: running, meta } });
    await tick();
    const row = view.getByTestId('command-output-row');
    const runningHeight = row.getBoundingClientRect().height;
    expect(view.getByTestId('indicator')).not.toBeNull();
    expect(view.getByTestId('command-output-background-button')).not.toBeNull();

    await view.rerender({ pane, item: completed, meta });
    await tick();
    const completedHeight = row.getBoundingClientRect().height;
    expect(row.querySelector('[data-testid="indicator"]')).toBeNull();
    expect(row.querySelector('[data-testid="command-output-background-button"]')).toBeNull();
    expect(completedHeight).toBe(runningHeight);
  });
});
