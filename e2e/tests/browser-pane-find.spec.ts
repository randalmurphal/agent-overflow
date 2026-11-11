// Find in the browser pane (mod+f) through the backend's engine find.
//
// Coverage: mod+f in the browser pane opens the shared find bar under its
// header; a query runs on the page the pane shows and reports its position,
// Enter and Shift+Enter step with wrap, a miss reads No results, Escape in the
// bar closes it. A navigation to another document ends the search on the
// backend, which closes the bar, and closing the pane ends it, twice. The
// harness runs the fake engine, whose find searches the page title
// (fake_engine.go) and which has no native key gate: an Escape pressed in a
// real page, and the escape event it raises, are covered by the
// AO_NATIVE_BROWSER_SMOKE gate and the unit tests. Unit and component
// coverage:
// browserFind.svelte.test.ts, BrowserPane.test.ts, PaneFindBar.test.ts,
// internal/browser/find_test.go.
import { test, expect, type SeedResult } from './fixtures.js';

// BrowserCompanionFind's method id (frontend/src/lib/transport/methodRoutes.ts).
const FIND_METHOD_ID = 1528700844;

interface BrowserState {
  activePageId?: string;
  pages?: Array<{ id: string; url: string }>;
}

test('mod+f finds in the shown browser page and closes when the page navigates away', async ({ harness, page }) => {
  // The pane mount and find id each find action names: a search belongs to
  // its mount's find, so only that find can see whether the pane's search
  // is still there.
  let findMount = '';
  let findId = '';
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && frame.methodId === FIND_METHOD_ID && frame.params?.[1]?.mount) {
        findMount = frame.params[1].mount;
        findId = frame.params[1].findId;
      }
      server.send(message);
    });
    server.onMessage((message) => socket.send(message));
  });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'browser-find',
      repo: {},
      threads: [{
        title: 'Browser find',
        turns: [{ userText: 'Open a page', items: [{ kind: 'assistant_text', summary: 'Opened.' }] }],
      }],
    }],
  });
  const threadId = seed.projects[0].threadIds[0];
  await harness.rpc('StartSession', threadId);
  await harness.open(page);
  await page.getByText('Browser find').click();
  const workspace = { projectId: seed.projects[0].projectId, workspacePath: '' };
  const first = await harness.rpc<string>('WriteWorkspaceFile', workspace, 'alpha-beta-alpha.html', '<!doctype html><title>x</title>');
  const second = await harness.rpc<string>('WriteWorkspaceFile', workspace, 'gamma.html', '<!doctype html><title>y</title>');
  const opened = await harness.rpc<BrowserState>('BrowserCompanionDo', threadId, { kind: 'new' });
  const pageId = opened.activePageId!;
  await harness.rpc('BrowserCompanionDo', threadId, { kind: 'show', pageId });

  const pane = page.getByTestId('browser-pane');
  await expect(pane).toBeVisible();
  const address = pane.getByRole('textbox', { name: 'Address' });
  const navigate = async (target: string, name: string) => {
    await address.fill(target);
    await address.press('Enter');
    await expect.poll(async () => {
      const state = await harness.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
      return state.pages?.find((candidate) => candidate.id === pageId)?.url ?? '';
    }).toContain(name);
  };
  await navigate(first, 'alpha-beta-alpha.html');

  await address.focus();
  await address.press('ControlOrMeta+f');
  const bar = pane.getByTestId('pane-find-bar');
  await expect(bar).toBeVisible();
  await expect(page.getByRole('dialog', { name: /search/i })).toHaveCount(0);
  const input = bar.getByTestId('pane-find-input');
  await expect(input).toBeFocused();
  await expect(input).toHaveAttribute('placeholder', 'Find in page');
  const count = bar.getByTestId('pane-find-count');

  await input.fill('alpha');
  await expect(count).toHaveText('1/2');
  await input.press('Enter');
  await expect(count).toHaveText('2/2');
  await input.press('Enter');
  await expect(count).toHaveText('1/2');
  await input.press('Shift+Enter');
  await expect(count).toHaveText('2/2');
  await input.fill('zzz');
  await expect(count).toHaveText('No results');
  await expect(bar.getByTestId('pane-find-next')).toBeDisabled();

  await input.press('Escape');
  await expect(bar).toHaveCount(0);

  // Reopened, the query runs again; the page then leaves the document and
  // the backend ends the search, which closes the bar.
  await address.focus();
  await address.press('ControlOrMeta+f');
  await expect(bar).toBeVisible();
  await bar.getByTestId('pane-find-input').fill('beta');
  await expect(count).toHaveText('1/1');
  await harness.rpc('BrowserCompanionDo', threadId, { kind: 'navigate', pageId, address: second });
  await expect(bar).toHaveCount(0);

  // Closing the pane ends the search on the backend, each time.
  for (let round = 0; round < 2; round++) {
    await address.focus();
    await address.press('ControlOrMeta+f');
    await bar.getByTestId('pane-find-input').fill('gamma');
    await expect(count).toHaveText('1/1');
    const mount = findMount;
    const id = findId;
    expect(mount).not.toBe('');
    expect(id).not.toBe('');
    await pane.getByRole('button', { name: 'Close browser' }).click();
    await expect(pane).toHaveCount(0);
    // The pane's search is gone: a step from its find changes nothing and
    // says the search is not its own.
    const step = await harness.rpc<{ foreign: boolean; active: boolean }>('BrowserCompanionFind', threadId, { kind: 'next', pageId, mount, findId: id });
    expect(step).toMatchObject({ foreign: true, active: false });
    await harness.rpc('BrowserCompanionDo', threadId, { kind: 'show', pageId });
    await expect(pane).toBeVisible();
  }
});
