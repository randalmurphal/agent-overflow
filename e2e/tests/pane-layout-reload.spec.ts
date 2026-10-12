// Pane layout durability across a reload.
//
// Coverage: the thread a pane shows is saved the moment the pane shows it,
// not when the thread's loads finish, so a reload while a switched-to
// thread is still loading reopens that thread rather than the one the user
// left; and a pane that started as a new-thread draft saves the thread it
// becomes, so a reload after the first send reopens it.
import { test, expect, type SeedResult } from './fixtures.js';
import type { Page } from '@playwright/test';
import { methodIdByName } from './offhost-helpers.js';

const LIST_RECENT_TURNS = methodIdByName('ListRecentTurns');

function threadPane(page: Page) {
  return page.locator('section[data-pane-kind="thread"]');
}

function seedThread(title: string, answer: string) {
  return {
    title,
    provider: 'claude',
    turns: [{ userText: `open ${title}`, items: [{ kind: 'assistant_text', summary: answer }] }],
  };
}

test('a reload reopens the thread switched to while it was still loading', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'layout-reload',
      repo: {},
      threads: [
        seedThread('Left behind', 'The thread the user left.'),
        seedThread('Switched to', 'The thread the user opened.'),
      ],
    }],
  });
  const switchedId = seed.projects[0].threadIds[1];

  // Hold the switched-to thread's recent-turns read on the first page
  // connection only, so the switch stays in flight across the reload and
  // the reloaded page loads normally.
  let connections = 0;
  let holding = false;
  let heldRequest = '';
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const first = ++connections === 1;
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message)) as { type?: string; id?: string; methodId?: number; params?: unknown[] };
      if (first && holding && frame.type === 'rpc' && frame.methodId === LIST_RECENT_TURNS && frame.params?.[0] === switchedId) {
        heldRequest = frame.id ?? '';
      }
      server.send(message);
    });
    server.onMessage((message) => {
      const frame = JSON.parse(String(message)) as { type?: string; id?: string };
      if (first && heldRequest && frame.type === 'rpc' && frame.id === heldRequest) return;
      socket.send(message);
    });
  });

  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'Left behind' }).click();
  await expect(threadPane(page).getByText('The thread the user left.')).toBeVisible();

  holding = true;
  await page.getByTestId('thread-row').filter({ hasText: 'Switched to' }).click();
  await expect(threadPane(page).getByText('The thread the user opened.')).toBeVisible();
  await expect.poll(() => heldRequest).not.toBe('');

  await page.reload();
  await expect(threadPane(page).getByText('The thread the user opened.')).toBeVisible();
  await expect(threadPane(page).getByText('The thread the user left.')).toHaveCount(0);
});

test('a reload reopens the thread a new-thread draft became', async ({ harness, page }) => {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'layout-new-thread', repo: {}, threads: [] }],
  });
  await harness.open(page);
  const project = page.getByRole('group', { name: 'Project: layout-new-thread' });
  await project.hover();
  await project.getByTestId('project-item-new-thread').click();
  const input = threadPane(page).getByLabel('Message Input');
  await input.fill('Start the new thread');
  await threadPane(page).getByTestId('composer-send').click();
  const sent = threadPane(page).getByTestId('user-message-summary').filter({ hasText: 'Start the new thread' });
  await expect(sent).toBeVisible();
  await expect(threadPane(page).getByTestId('assistant-message-body').first()).toBeVisible();

  await page.reload();
  await expect(sent).toBeVisible();
});
