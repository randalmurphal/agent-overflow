import { describe, expect, it } from 'vitest';
import { BROWSER_TOOLS_SERVER } from '../../utils/browserTools';
import { AO_TOOL_SERVERS, REMOTE_TOOLS_SERVER, aoToolPresentation, isAoToolMeta, remoteRunCommand } from './aoTools';

function meta(server: string, tool: string, input: Record<string, unknown> = {}) {
  return { mcp: { server, tool }, input };
}

describe('AO tool presentation', () => {
  it('names the servers the Go side registers', () => {
    expect(REMOTE_TOOLS_SERVER).toBe('ao-remote-tools');
    expect(Object.keys(AO_TOOL_SERVERS).sort()).toEqual([BROWSER_TOOLS_SERVER, REMOTE_TOOLS_SERVER].sort());
  });

  it('leaves native tools and other MCP servers alone', () => {
    expect(aoToolPresentation(null)).toBeNull();
    expect(aoToolPresentation({ input: { file_path: 'a.ts' } })).toBeNull();
    expect(aoToolPresentation(meta('docs', 'lookup', { q: 'wails' }))).toBeNull();
    expect(aoToolPresentation({ mcp: 'ao-remote-tools' })).toBeNull();
    expect(isAoToolMeta(meta('playwright', 'browser_click'))).toBe(false);
    expect(isAoToolMeta(meta(REMOTE_TOOLS_SERVER, 'remote_run'))).toBe(true);
  });

  describe('remote tools', () => {
    it('shows the command a remote_run call runs, quoted where boundaries matter', () => {
      const p = aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_run', {
        computer_id: 'mac', project_id: 'p', request_id: 'r', argv: ['runner', 'two words', '', 'C:\\x'],
      }))!;
      expect(p).toMatchObject({
        icon: 'monitor', label: 'run', headerLabel: 'Remote run', computerId: 'mac', computerName: '',
        what: 'runner "two words" "" "C:\\\\x"',
      });
    });

    it('describes a script by its interpreter', () => {
      expect(remoteRunCommand({ script: 'print(1)', interpreter: ['python3', '-u'] })).toBe('python3 -u script');
    });

    it('prefers a pre-rendered command and computer name from a tray projection', () => {
      const p = aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_run', {
        computer_id: 'mac', computer_name: 'Macaroni-air', command: 'make test', label: 'Tests',
      }))!;
      expect(p.what).toBe('make test');
      expect(p.computerName).toBe('Macaroni-air');
    });

    it('refers to jobs by a short id and quotes searches', () => {
      const id = '98312d67-1111-2222-3333-444444444444';
      expect(aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_status', { computer_id: 'mac', request_id: id }))).toMatchObject({ label: 'status', what: 'job 98312d67' });
      expect(aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_cancel', { request_id: id }))!.what).toBe('job 98312d67');
      expect(aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_read_log', { request_id: id, offset: -1 }))).toMatchObject({ label: 'log', what: 'job 98312d67' });
      expect(aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_search_log', { request_id: id, query: 'FAIL' }))).toMatchObject({ label: 'search', what: '"FAIL" in job 98312d67' });
      expect(aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_fetch_log', { request_id: id }))).toMatchObject({ label: 'fetch', what: 'log of job 98312d67' });
      expect(aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_fetch_artifact', { request_id: id, path: 'out/report.html' }))).toMatchObject({ label: 'fetch', what: 'out/report.html from job 98312d67' });
      expect(aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_jobs'))).toMatchObject({ label: 'list', what: 'remote jobs', headerLabel: 'Remote list' });
      expect(aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_computers'))).toMatchObject({ label: 'list', what: 'computers' });
    });

    it('still presents a tool the table does not know, by the family icon and its arguments', () => {
      const p = aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_future', { computer_id: 'mac', depth: 2, deep: { a: 1 } }))!;
      expect(p).toMatchObject({ icon: 'monitor', label: 'future', headerLabel: 'Remote future', computerId: 'mac' });
      expect(p.what).toBe('computer_id="mac", depth=2, deep={"a":1}');
    });
  });

  describe('browser tools', () => {
    it.each([
      ['browser_open', { url: 'https://example.test/a' }, 'open', 'https://example.test/a'],
      ['browser_new_page', {}, 'open', 'new page'],
      ['browser_open_file', { path: '/tmp/report.html' }, 'open', '/tmp/report.html'],
      ['browser_pages', {}, 'list', 'pages'],
      ['browser_select_page', { page_id: 'p1' }, 'select', 'page p1'],
      ['browser_label_page', { page_id: 'p1', label: 'checkout' }, 'label', 'page p1 as "checkout"'],
      ['browser_session', { name: 'Signup flow' }, 'session', 'Signup flow'],
      ['browser_visibility', { visible: true, page_id: 'p1' }, 'show', 'page p1'],
      ['browser_visibility', { visible: false }, 'show', 'hide companion'],
      ['browser_viewport', { action: 'set', width: 1280, height: 720 }, 'viewport', '1280×720'],
      ['browser_close_page', { page_id: 'p1' }, 'close', 'page p1'],
      ['browser_snapshot', {}, 'snapshot', ''],
      ['browser_screenshot', { full_page: true }, 'capture', 'full page'],
      ['browser_locator', { action: 'click', locator: { role: 'button', name: 'Save' } }, 'locate', 'click button "Save"'],
      ['browser_locator', { action: 'fill', locator: { css: '#email' }, value: 'a@b' }, 'locate', 'fill #email'],
      ['browser_click', { selector: '#submit' }, 'click', '#submit'],
      ['browser_pointer', { action: 'click', x: 10, y: 20 }, 'pointer', 'click at 10,20'],
      ['browser_dom', { action: 'type', node_id: 'n7', text: 'hi' }, 'dom', 'type node n7'],
      ['browser_type', { selector: '#q', text: 'hello world' }, 'type', '"hello world" into #q'],
      ['browser_press', { key: 'Enter' }, 'press', 'Enter'],
      ['browser_press', { keys: ['Control', 'L'] }, 'press', 'Control+L'],
      ['browser_scroll', { y: 400 }, 'scroll', 'by 400'],
      ['browser_scroll', { selector: '#list', x: 10, y: -20 }, 'scroll', '#list by 10,-20'],
      ['browser_wait', { milliseconds: 500 }, 'wait', '500 ms'],
      ['browser_wait', { selector: '.done', state: 'visible' }, 'wait', 'for .done visible'],
      ['browser_wait', { url: '**/done' }, 'wait', 'for **/done'],
      ['browser_history', { action: 'back' }, 'history', 'back'],
      ['browser_evaluate', { expression: 'document.title' }, 'eval', 'document.title'],
      ['browser_evaluate_readonly', { expression: 'location.href' }, 'eval', 'location.href'],
      ['browser_clipboard', { action: 'write_text', text: 'abc' }, 'clipboard', 'write_text "abc"'],
      ['browser_console_logs', { filter: 'error' }, 'console', 'error'],
      ['browser_downloads', { action: 'wait' }, 'download', 'wait'],
      ['browser_assets', { action: 'list' }, 'assets', 'list'],
    ])('%s', (tool, input, label, what) => {
      const p = aoToolPresentation(meta(BROWSER_TOOLS_SERVER, tool, input))!;
      expect(p).toMatchObject({ icon: 'globe', label, what, headerLabel: `Browser ${label}`, computerId: '' });
    });
  });

  it('keeps the body to one bounded line', () => {
    const p = aoToolPresentation(meta(BROWSER_TOOLS_SERVER, 'browser_evaluate', {
      expression: `document\n  .querySelectorAll('a')\n  ${'.map(x => x.href)'.repeat(20)}`,
    }))!;
    expect(p.what).not.toContain('\n');
    expect(p.what.length).toBeLessThanOrEqual(160);
    expect(p.what.endsWith('…')).toBe(true);
  });
});
