import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import OwnMemoryBanner from './OwnMemoryBanner.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { makeSettings } from '../../../test/helpers/settings';
import { makeThread } from '../../../test/helpers/chat';
import type { NativeMemoryStatus } from '../../types/models';

describe('<OwnMemoryBanner>', () => {
  // A pane switch installs the next assistant in place. The computer's
  // answer names each provider, so the incoming assistant's provider shows
  // its own answer at once, never the previous provider's while a read is
  // out.
  it("shows only the thread's provider's answer when the thread changes provider", async () => {
    const saved = makeSettings({ claudeOwnMemoryOff: 'never', codexOwnMemoryOff: 'never' });
    setBindingMock('GetSettings', async () => saved);
    await loadSettings();
    const answers: Array<(status: NativeMemoryStatus) => void> = [];
    setBindingMock('GetNativeMemoryStatus', () => new Promise((resolve) => { answers.push(resolve); }));
    const view = render(OwnMemoryBanner, {
      props: { thread: makeThread({ id: 'claude-assistant', agentId: 'agent-claude', provider: 'claude', projectId: undefined }) },
    });
    await expect.poll(() => answers.length).toBe(1);
    answers[0]({ claude: { on: true, error: '' }, codex: { on: false, error: '' } });
    await expect.poll(() => view.queryByTestId('own-memory-banner')?.textContent ?? '').toContain("Claude's own memory is also on");

    await view.rerender({
      thread: makeThread({ id: 'codex-assistant', agentId: 'agent-codex', provider: 'codex', projectId: undefined }),
    });
    expect(view.container.textContent).not.toContain("Codex's own memory");
    await expect.poll(() => view.queryByTestId('own-memory-banner')).toBeNull();
  });
});
