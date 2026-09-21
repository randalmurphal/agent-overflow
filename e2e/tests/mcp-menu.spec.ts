// MCP menu preferences, runtime status, blocked controls and repeated toggles
// through the production SPA, transport, config writer and mock Codex session.
import { test, expect, type SeedResult } from './fixtures.js';
import { advanceGate, awaitGate, setScenario } from './thread-tools-helpers.js';

interface McpRow {
  name: string;
  disabled: boolean;
  status: string;
  toggleDisabledReason?: string;
}

for (const compact of [false, true]) {
  test.describe(compact ? 'compact MCP menu' : 'desktop MCP menu', () => {
    if (compact) test.use({ viewport: { width: 430, height: 932 }, isMobile: true, hasTouch: true });
    test('saved preference and runtime status stay separate through toggles', async ({ harness, page }) => {
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        providerHome: [{ path: '.codex/config.toml', content: '[mcp_servers.srv]\ncommand = "fixture-only"\nenabled = false\n[mcp_servers.blocked]\ncommand = "fixture-only"\n' }],
        projects: [{ name: 'mcp-menu', repo: {}, threads: [{
          title: 'MCP states', provider: 'codex',
          turns: [{ userText: 'Ready?', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }],
        }] }],
      });
      const { path, threadIds } = seed.projects[0];
      const thread = threadIds[0];
      const capture = (state: string) => ({ capture: { var: 'MCP_STATE', from: state, pattern: '(.+)' } });
      const onStart: unknown[] = [capture('disabled')];
      for (let i = 0; i < 4; i++) {
        onStart.push({ waitSignal: { name: `toggle-${i}` } }, capture(i % 2 === 0 ? 'connected' : 'disabled'), {
          emit: { lines: [JSON.stringify({ jsonrpc: '2.0', method: 'mcpServer/startupStatus/updated', params: {
            threadId: '${THREAD_ID}', name: 'srv', status: i % 2 === 0 ? 'ready' : 'cancelled',
          } })] },
        });
      }
      onStart.push({ waitSignal: { name: 'finished' } });
      await setScenario(harness, path, {
        version: 1, name: 'mcp-menu', provider: 'codex', turns: [], onStart,
        codex: { responses: {
          'mcpServerStatus/list': '{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{"data":[{"name":"srv","runtimeStatus":"${MCP_STATE}","tools":{}},{"name":"blocked","runtimeStatus":"disabled"},{"name":"external","runtimeStatus":"connected"}]}}',
          'config/mcpServer/reload': '{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{}}',
        } },
      });
      await harness.rpc('StartSession', thread);
      const mock = await awaitGate(harness, 'toggle-0', path);
      await harness.open(page);
      await page.getByText('MCP states', { exact: true }).click();
      const menu = page.getByRole('menu', { name: 'MCP servers' });
      async function openMenu() {
        // Resolve the visible control at click time as the toolbar changes size.
        await page.locator('[data-testid="composer-mcp-trigger"]:visible, [data-testid="composer-pickers-rollup"]:visible').first().click();
        const picker = page.getByRole('menuitem', { name: /^MCP servers/ });
        await expect(menu.or(picker)).toBeVisible();
        if (await picker.isVisible()) await picker.click();
        await expect(menu).toBeVisible();
      }
      await openMenu();
      const row = (name: string) => menu.getByRole('menuitem').filter({ has: page.getByText(name, { exact: true }) });
      await expect(row('srv')).toContainText('Off');
      await expect(row('srv').locator('[data-mcp-enabled]')).toHaveAttribute('data-mcp-enabled', 'false');
      await expect(row('srv').getByRole('button', { name: 'Reconnect srv' })).toHaveCount(0);
      await expect(row('blocked')).toContainText('Blocked');
      await expect(row('blocked')).toHaveAttribute('aria-disabled', 'true');
      await expect(row('blocked').locator('[data-mcp-enabled]')).toHaveAttribute('data-mcp-enabled', 'true');
      await expect(row('external')).toHaveAttribute('aria-disabled', 'true');
      await expect(row('external')).toHaveAttribute('title', /managed outside/);
      await expect(harness.rpc('SetThreadMcpServerEnabled', thread, 'blocked', true)).rejects.toThrow();
      await expect(harness.rpc('ReconnectMcpServer', thread, 'blocked')).rejects.toThrow();
      for (let i = 0; i < 4; i++) {
        const enabled = i % 2 === 0;
        await expect(row('srv')).not.toHaveAttribute('aria-disabled', 'true');
        await row('srv').click();
        await expect.poll(async () => {
          const rows = await harness.rpc<McpRow[]>('ListThreadMcpServers', thread);
          return rows.find((r) => r.name === 'srv')?.disabled;
        }).toBe(!enabled);
        const nextGate = awaitGate(harness, i === 3 ? 'finished' : `toggle-${i + 1}`, path);
        await advanceGate(harness, mock.mockId, `toggle-${i}`);
        await nextGate;
        // Reopening also proves the persisted preference survives a fresh listing.
        await page.keyboard.press('Escape');
        await openMenu();
        await expect(row('srv')).toContainText(enabled ? 'Connected' : 'Off');
        await expect(row('srv').locator('[data-mcp-enabled]')).toHaveAttribute('data-mcp-enabled', String(enabled));
        await expect(row('srv').getByRole('button', { name: 'Reconnect srv' })).toHaveCount(enabled ? 1 : 0);
      }
    });
  });
}
