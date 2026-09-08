import { beforeEach, expect, it } from 'vitest';
import { page } from 'vitest/browser';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import '../../../app.css';
import ActivityRailBackgroundBody from './ActivityRailBackgroundBody.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { deriveTrayTasks } from '../../utils/backgroundTray';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

beforeEach(resetBindingMocks);

it.each([360, 1280])('keeps remote command, stop and bounded log readable at %ipx', async (width) => {
  await page.viewport(width, 800);
  const job = { computerId: 'nexus', requestId: 'training-job', workspace: `/work/${'long-project-name/'.repeat(12)}` };
  const tasks = deriveTrayTasks([makeItem({ id: 'remote-job:nexus:training-job', toolName: 'remote_command',
    isBackground: true, status: 'running', summary: `Nexus · python train.py ${'--option=value '.repeat(30)}`,
    meta: JSON.stringify({ remoteJob: job }) })], Date.now(), 200);
  setBindingMock('ReadThreadRemoteLog', async () => ({ text: '0123456789'.repeat(1638), offset: 20, expired: false }));
  const view = render(ActivityRailBackgroundBody, { tasks, provider: 'codex', threadId: 'thread', runningCount: 1 });
  const toggle = view.getByRole('button', { name: /Show remote job log/ });
  await fireEvent.click(toggle);
  await waitFor(() => expect(view.container.querySelector('pre')).not.toBeNull());
  for (const element of [toggle, view.getByRole('button', { name: 'Stop Remote Job' }), view.getByRole('button', { name: 'Refresh log' }), view.container.querySelector('pre')!]) {
    const rect = element.getBoundingClientRect();
    expect(rect.width).toBeGreaterThan(0);
    expect(rect.left).toBeGreaterThanOrEqual(0);
    expect(rect.right).toBeLessThanOrEqual(width);
  }
  expect(view.container.querySelector('pre')!.getBoundingClientRect().height).toBeLessThanOrEqual(192);
});
