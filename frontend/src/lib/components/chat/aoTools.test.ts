import { describe, expect, it } from 'vitest';
import { BROWSER_TOOLS_SERVER } from '../../utils/browserTools';
import {
  AO_TOOL_SERVERS,
  REMOTE_TOOLS_SERVER,
  THREAD_TOOLS_SERVER,
  aoToolFacts,
  aoToolPresentation,
  aoToolServer,
  isAoToolMeta,
  remoteResultView,
  remoteRunCommand,
  threadResultView,
  type AoToolNames,
} from './aoTools';

function meta(server: string, tool: string, input: Record<string, unknown> = {}) {
  return { mcp: { server, tool }, input };
}

const id = '98312d67-1111-2222-3333-444444444444';
// What a row can name: an attached computer, the thread's jobs, its live pages.
const names: AoToolNames = {
  computer: (computer) => (computer === 'mac' ? 'Macaroni-air' : ''),
  job: (request) => (request === id ? 'Go tests' : ''),
  page: (page) => (page === 'p1' ? 'Checkout' : ''),
};

describe('AO tool presentation', () => {
  it('names the servers the Go side registers', () => {
    expect(REMOTE_TOOLS_SERVER).toBe('ao-remote-tools');
    expect(THREAD_TOOLS_SERVER).toBe('ao-thread-tools');
    expect(Object.keys(AO_TOOL_SERVERS).sort()).toEqual(
      [BROWSER_TOOLS_SERVER, REMOTE_TOOLS_SERVER, THREAD_TOOLS_SERVER].sort(),
    );
  });

  it('leaves native tools and other MCP servers alone', () => {
    expect(aoToolPresentation(null)).toBeNull();
    expect(aoToolPresentation({ input: { file_path: 'a.ts' } })).toBeNull();
    expect(aoToolPresentation(meta('docs', 'lookup', { q: 'wails' }))).toBeNull();
    expect(aoToolPresentation({ mcp: 'ao-remote-tools' })).toBeNull();
    expect(isAoToolMeta(meta('playwright', 'browser_click'))).toBe(false);
    expect(isAoToolMeta(meta(REMOTE_TOOLS_SERVER, 'remote_run'))).toBe(true);
    expect(aoToolServer(meta('playwright', 'browser_click'))).toBeNull();
    expect(aoToolServer(meta(BROWSER_TOOLS_SERVER, 'browser_click'))).toBe(BROWSER_TOOLS_SERVER);
  });

  describe('remote tools', () => {
    it('shows the command a remote_run call runs, quoted where boundaries matter', () => {
      const p = aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_run', {
        computer_id: 'mac', project_id: 'p', request_id: 'r', argv: ['runner', 'two words', '', 'C:\\x'],
      }))!;
      expect(p).toMatchObject({
        icon: 'monitor', label: 'run', headerLabel: 'Remote run', computerId: 'mac', computerName: '', requestId: 'r',
        what: 'runner "two words" "" "C:\\\\x"',
      });
    });

    it('names the computer through the names a row can see', () => {
      const p = aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_run', { computer_id: 'mac', argv: ['make'] }), names)!;
      expect(p.computerName).toBe('Macaroni-air');
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

    it('refers to jobs by their label, and by a short id only when this client cannot name them', () => {
      expect(aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_status', { computer_id: 'mac', request_id: id }), names)).toMatchObject({ label: 'status', what: 'Go tests', computerName: 'Macaroni-air', requestId: id });
      expect(aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_cancel', { request_id: id }), names)!.what).toBe('Go tests');
      expect(aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_search_log', { request_id: id, query: 'FAIL' }), names)!.what).toBe('"FAIL" in Go tests');
      expect(aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_fetch_log', { request_id: id }), names)!.what).toBe('log of Go tests');
      expect(aoToolPresentation(meta(REMOTE_TOOLS_SERVER, 'remote_fetch_artifact', { request_id: id, path: 'out/report.html' }), names)!.what).toBe('out/report.html from Go tests');
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

  describe('thread tools', () => {
    const tid = '7f2c9a41-aaaa-bbbb-cccc-dddddddddddd';

    it.each([
      ['thread_search', { query: 'flaky test' }, 'search', '"flaky test"'],
      ['thread_search', {}, 'search', 'threads'],
      ['thread_search', { thread_id: tid }, 'search', 'thread 7f2c9a41'],
      ['thread_show', { thread_id: '7f2c9a41' }, 'read', 'thread 7f2c9a41'],
      ['thread_item', { thread_id: tid, item_id: 'it-9' }, 'item', 'it-9 in thread 7f2c9a41'],
      ['thread_item', { thread_id: tid }, 'item', 'thread 7f2c9a41'],
      ['thread_options', {}, 'options', 'spawn options'],
      ['thread_spawn', { prompt: 'Port the fix\nto Windows' }, 'spawn', 'Port the fix'],
      ['thread_send', { thread_id: tid, message: 'ping' }, 'send', 'ping → thread 7f2c9a41'],
      ['thread_send', { message: 'ping' }, 'send', 'ping'],
      ['thread_ask', { thread_id: tid, question: 'which branch?' }, 'ask', 'which branch? → thread 7f2c9a41'],
      ['thread_reply', { token: 't', text: 'done' }, 'reply', 'done'],
      ['thread_status', { tokens: ['a', 'b'] }, 'status', '2 requests'],
      ['thread_status', {}, 'status', 'requests'],
      ['thread_cancel', { token: 't' }, 'cancel', 'request'],
      ['thread_cancel', { thread_id: tid }, 'cancel', 'thread 7f2c9a41'],
      ['thread_update', { thread_ids: [tid, tid] }, 'update', '2 threads'],
      ['thread_update', { thread_ids: [tid] }, 'update', 'thread 7f2c9a41'],
      ['thread_group', { group_id: 'g1', rename: 'Release' }, 'group', 'rename g1 → Release'],
      ['thread_group', { group: 'Bugs', pin: 'front' }, 'group', 'pin Bugs front'],
      ['thread_group', { group: 'Bugs', delete: true }, 'group', 'delete Bugs'],
      ['thread_remind', { note: 'check the queue' }, 'remind', 'check the queue'],
    ])('%s', (tool, input, label, what) => {
      const p = aoToolPresentation(meta(THREAD_TOOLS_SERVER, tool, input), names)!;
      expect(p).toMatchObject({ icon: 'speech-bubble', label, what, headerLabel: `Thread ${label}` });
    });

    it('carries the computer a cross-computer call names', () => {
      const p = aoToolPresentation(meta(THREAD_TOOLS_SERVER, 'thread_show', { computer_id: 'mac', thread_id: tid }), names)!;
      expect(p).toMatchObject({ computerId: 'mac', computerName: 'Macaroni-air' });
    });

    it('reads a reply for the thread name the arguments could only abbreviate', () => {
      expect(threadResultView(JSON.stringify({ thread_id: tid, title: 'Windows launcher', state: 'running' })))
        .toEqual({ title: 'Windows launcher', state: 'running', summary: '' });
      expect(threadResultView(JSON.stringify({ rows: [{ thread_id: tid }, { thread_id: tid }] })))
        .toEqual({ title: '', state: '', summary: '2 threads' });
      expect(threadResultView(JSON.stringify({ computers: [{ rows: [{}] }, { rows: [{}, {}] }] })))
        .toEqual({ title: '', state: '', summary: '3 threads' });
      expect(threadResultView(JSON.stringify({ rows: [{}] }))!.summary).toBe('1 thread');
      expect(threadResultView('not json')).toBeNull();
      expect(threadResultView(JSON.stringify({ ok: true }))).toBeNull();
      expect(threadResultView(JSON.stringify([1, 2]))).toBeNull();
    });

    it('still presents a tool the table does not know', () => {
      const p = aoToolPresentation(meta(THREAD_TOOLS_SERVER, 'thread_future', { thread_id: tid }))!;
      expect(p).toMatchObject({ icon: 'speech-bubble', label: 'future', headerLabel: 'Thread future' });
    });
  });

  describe('browser tools', () => {
    it.each([
      ['browser_open', { url: 'https://example.test/a' }, 'open', 'https://example.test/a'],
      ['browser_new_page', {}, 'open', 'new page'],
      ['browser_open_file', { path: '/tmp/report.html' }, 'open', '/tmp/report.html'],
      ['browser_pages', {}, 'list', 'pages'],
      ['browser_select_page', { page_id: 'p1' }, 'select', 'Checkout'],
      ['browser_select_page', { page_id: 'p9' }, 'select', 'page'],
      ['browser_label_page', { page_id: 'p1', label: 'checkout' }, 'label', 'Checkout as "checkout"'],
      ['browser_session', { name: 'Signup flow' }, 'session', 'Signup flow'],
      ['browser_visibility', { visible: true, page_id: 'p1' }, 'show', 'Checkout'],
      ['browser_visibility', { visible: false }, 'show', 'hide companion'],
      ['browser_viewport', { action: 'set', width: 1280, height: 720 }, 'viewport', '1280×720'],
      ['browser_close_page', { page_id: 'p1' }, 'close', 'Checkout'],
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
      const p = aoToolPresentation(meta(BROWSER_TOOLS_SERVER, tool, input), names)!;
      expect(p).toMatchObject({ icon: 'globe', label, what, headerLabel: `Browser ${label}`, computerId: '', requestId: '' });
    });
  });

  describe('the expanded body', () => {
    it('lists the computer and job by name and the inputs the header leaves out, never a bare id', () => {
      const facts = aoToolFacts(meta(REMOTE_TOOLS_SERVER, 'remote_run', {
        computer_id: 'mac', project_id: 'proj', request_id: id, workspace_path: '/w', argv: ['make'], timeout_seconds: 600, label: 'Build',
      }), names);
      expect(facts).toEqual([
        { label: 'computer', value: 'Macaroni-air' },
        { label: 'workspace', value: '/w' },
        { label: 'timeout', value: '600' },
        { label: 'label', value: 'Build' },
      ]);
      expect(aoToolFacts(meta(REMOTE_TOOLS_SERVER, 'remote_search_log', { computer_id: 'mac', request_id: id, query: 'FAIL', max_bytes: 4096 }), names)).toEqual([
        { label: 'computer', value: 'Macaroni-air' },
        { label: 'job', value: 'Go tests' },
        { label: 'query', value: 'FAIL' },
        { label: 'read limit', value: '4096' },
      ]);
      expect(aoToolFacts(meta(BROWSER_TOOLS_SERVER, 'browser_click', { page_id: 'p1', selector: '#go' }), names)).toEqual([
        { label: 'page', value: 'Checkout' },
        { label: 'selector', value: '#go' },
      ]);
      expect(aoToolFacts(meta(BROWSER_TOOLS_SERVER, 'browser_snapshot', {}))).toEqual([]);
      expect(aoToolFacts(meta('docs', 'lookup', { q: 'x' }))).toEqual([]);
    });

    it('shows the script a run carries, bounded', () => {
      const facts = aoToolFacts(meta(REMOTE_TOOLS_SERVER, 'remote_run', { computer_id: 'mac', script: 'x'.repeat(5000), interpreter: ['bash'] }));
      const script = facts.find((f) => f.label === 'script')!;
      expect(script.value.length).toBe(2000);
      expect(script.value.endsWith('…')).toBe(true);
      expect(facts.find((f) => f.label === 'computer')!.value).toBe('mac');
    });

    it('reads a remote reply as its outcome and output, and leaves other text alone', () => {
      const reply = JSON.stringify({ id, computerId: 'mac', state: 'succeeded', exitCode: 0, startedAt: 1000, finishedAt: 13500, output: 'ok\n', outputHead: 'go: downloading\n', outputHint: 'Older output was discarded.' });
      expect(remoteResultView(reply)).toEqual({ outcome: 'succeeded · exit 0 · 12.5s', error: '', head: 'go: downloading\n', output: 'ok\n', hint: 'Older output was discarded.' });
      expect(remoteResultView(JSON.stringify({ id, state: 'running', backgrounded: true, exitCode: -1, startedAt: 1000 }))).toMatchObject({ outcome: 'running in the background' });
      expect(remoteResultView(JSON.stringify({ id, state: 'failed', exitCode: 2, error: 'tests failed' }))).toMatchObject({ outcome: 'failed · exit 2', error: 'tests failed' });
      expect(remoteResultView('plain text')).toBeNull();
      expect(remoteResultView(JSON.stringify({ pages: [] }))).toBeNull();
    });
  });

  it('keeps the body to one bounded line', () => {
    const p = aoToolPresentation(meta(BROWSER_TOOLS_SERVER, 'browser_evaluate', {
      expression: `document\n  .querySelectorAll('a')\n  ${'.map(x => x.href)'.repeat(20)}`,
    }))!;
    expect(p.what).not.toContain('\n');
    expect(p.what.length).toBeLessThanOrEqual(160);
    expect(p.what.endsWith('…')).toBe(true);
    expect(p.fullWhat).toContain('\n');
    expect(p.fullWhat.length).toBeGreaterThan(160);
  });
});
