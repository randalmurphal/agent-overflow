import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { SRC_ROOT } from '../../../test/sourceScan';
import { BROWSER_TOOLS_SERVER } from '../../utils/browserTools';
import {
  AO_TOOL_SERVERS,
  REMOTE_TOOLS_SERVER,
  THREAD_TOOLS_SERVER,
  WORKFLOW_TOOLS_SERVER,
  aoToolFacts,
  aoToolPresentation,
  aoToolServer,
  isAoToolMeta,
  remoteRunCommand,
  type AoToolNames,
} from './aoTools';

function meta(server: string, tool: string, input: Record<string, unknown> = {}) {
  return { mcp: { server, tool }, input };
}

const id = '98312d67-1111-2222-3333-444444444444';
// What a row can name: an attached computer, the thread's jobs, its live pages.
const named = 'feedbeef-1111-2222-3333-444444444444';
const namedRun = 'c0ffee00-1111-2222-3333-444444444444';
const names: AoToolNames = {
  computer: (computer) => (computer === 'mac' ? 'Macaroni-air' : ''),
  job: (request) => (request === id ? 'Go tests' : ''),
  page: (page) => (page === 'p1' ? 'Checkout' : ''),
  thread: (thread) => (named.startsWith(thread) ? 'Release notes' : ''),
  threadId: (thread) => (named.startsWith(thread) ? named : ''),
  run: (run) => (run === namedRun ? 'digest · Weekly summary' : ''),
};

describe('AO tool presentation', () => {
  it('names the servers the Go side registers', () => {
    expect(REMOTE_TOOLS_SERVER).toBe('ao-remote-tools');
    expect(THREAD_TOOLS_SERVER).toBe('ao-thread-tools');
    expect(WORKFLOW_TOOLS_SERVER).toBe('ao-workflow-tools');
    expect(Object.keys(AO_TOOL_SERVERS).sort()).toEqual(
      [BROWSER_TOOLS_SERVER, REMOTE_TOOLS_SERVER, THREAD_TOOLS_SERVER, WORKFLOW_TOOLS_SERVER].sort(),
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

    it('presents every tool the thread tools server lists in its own words', () => {
      const schemas = readFileSync(join(SRC_ROOT, '..', '..', 'internal', 'threadtools', 'schemas.go'), 'utf8');
      const list = /var ToolNames = \[\]string\{([^}]*)\}/.exec(schemas)?.[1] ?? '';
      const tools = [...list.matchAll(/"(thread_[a-z_]+)"/g)].map((match) => match[1]);
      expect(tools.length).toBeGreaterThan(10);
      for (const tool of tools) {
        // A tool the table does not know lists its inputs as key=value.
        const p = aoToolPresentation(meta(THREAD_TOOLS_SERVER, tool, { unlisted: 'x' }), names)!;
        expect(p.fullWhat, tool).not.toContain('unlisted=');
      }
    });

    it.each([
      ['thread_search', { query: 'flaky test' }, 'search', '"flaky test"'],
      ['thread_search', {}, 'search', 'recent threads'],
      ['thread_search', { state: 'running', spawned_by_me: true }, 'search', 'running threads spawned here'],
      ['thread_search', { group: 'Bugs' }, 'search', 'threads in Bugs'],
      ['thread_search', { thread_id: tid }, 'search', 'in thread 7f2c9a41'],
      ['thread_search', { thread_id: named, query: 'deploy' }, 'search', '"deploy" in Release notes'],
      ['thread_show', { thread_id: '7f2c9a41' }, 'read', 'thread 7f2c9a41 · last 20 turns'],
      // A typed prefix reads whole: the short form would cut what the model wrote.
      ['thread_show', { thread_id: '7f2c9a41-aaaa' }, 'read', 'thread 7f2c9a41-aaaa · last 20 turns'],
      ['thread_show', { thread_id: named, window: 'last_answer' }, 'read', 'Release notes · last answer'],
      ['thread_show', { thread_id: tid, window: 'around', item_id: 'it-9' }, 'read', 'thread 7f2c9a41 · around it-9'],
      ['thread_show', { thread_id: tid, window: 'head', turns: 5 }, 'read', 'thread 7f2c9a41 · first 5 turns'],
      ['thread_show', { thread_id: tid, window: 'all', to_file: true }, 'read', 'thread 7f2c9a41 · whole thread'],
      ['thread_item', { thread_id: tid, item_id: 'it-9' }, 'item', 'thread 7f2c9a41 · item it-9'],
      ['thread_item', { thread_id: tid, item_id: 'toolu_0123456789', query: 'FAIL' }, 'item', 'thread 7f2c9a41 · item toolu_01 · find "FAIL"'],
      ['thread_item', { thread_id: tid, item_id: 'it-9', lines: '10-20' }, 'item', 'thread 7f2c9a41 · item it-9 · lines 10-20'],
      ['thread_item', { thread_id: tid }, 'item', 'thread 7f2c9a41'],
      ['thread_options', {}, 'opts', 'spawn options'],
      ['thread_spawn', { prompt: 'Port the fix\nto Windows' }, 'spawn', 'Port the fix'],
      ['thread_spawn', { prompt: 'Port the fix', title: 'Windows port' }, 'spawn', 'Windows port'],
      ['thread_send', { thread_id: tid, message: 'ping' }, 'send', 'thread 7f2c9a41 · ping'],
      ['thread_send', { thread_id: named.slice(0, 8), message: 'rebase\nplease' }, 'send', 'Release notes · rebase'],
      ['thread_send', { message: 'ping' }, 'send', 'ping'],
      ['thread_ask', { thread_id: tid, question: 'which branch?' }, 'ask', 'thread 7f2c9a41 · which branch?'],
      ['thread_reply', { token: 't', text: 'done' }, 'reply', 'done'],
      ['thread_status', { tokens: ['a', 'b'] }, 'status', '2 requests'],
      ['thread_status', { thread_ids: [named], wait_seconds: 120 }, 'status', 'Release notes · wait 2m'],
      ['thread_status', {}, 'status', 'all requests'],
      ['thread_cancel', { token: 'abcdef0123456789' }, 'cancel', 'request abcdef01'],
      ['thread_cancel', { thread_id: tid }, 'cancel', 'thread 7f2c9a41'],
      ['thread_update', { thread_ids: [tid, tid] }, 'update', '2 threads'],
      ['thread_update', { thread_ids: [tid] }, 'update', 'thread 7f2c9a41'],
      ['thread_group', { group_id: 'g1', rename: 'Release' }, 'group', 'rename g1 → Release'],
      ['thread_group', { group: 'Bugs', pin: 'front' }, 'group', 'pin Bugs front'],
      ['thread_group', { group: 'Bugs', delete: true }, 'group', 'delete Bugs'],
      ['thread_remind', { note: 'check the queue' }, 'remind', 'check the queue'],
      ['thread_loop', { label: 'board', note: 'check', every_seconds: 300 }, 'loop', 'board · every 5m'],
      ['thread_loop', { label: 'board', stop: true }, 'loop', 'stop board'],
      ['thread_watch', { label: 'mrs', command: './poll', every_seconds: 90 }, 'watch', 'mrs · every 90s'],
      ['thread_watch', { label: 'tail', command: './tail', long_running: true }, 'watch', 'tail · long-running'],
      ['thread_cancel', { label: 'board' }, 'cancel', 'board'],
      ['thread_attention', { kind: 'review', reason: 'Notes ready\nsee the doc' }, 'flag', 'review · Notes ready'],
      ['thread_attention', { kind: 'none' }, 'flag', 'lowered'],
      ['thread_new_assistant', { name: 'Ops', role: 'Watch the deploys.' }, 'create', 'Ops'],
    ])('%s', (tool, input, label, what) => {
      const p = aoToolPresentation(meta(THREAD_TOOLS_SERVER, tool, input), names)!;
      expect(p).toMatchObject({ icon: 'speech-bubble', label, what, headerLabel: `Thread ${label}` });
    });

    it('ends a schedule call with the next fire its reply named, dated against the row, not today', () => {
      // A row from another year: whether the fire reads as a time or a date
      // and time depends on the row's day alone.
      const rowAt = new Date(2024, 2, 5, 9, 0, 0, 0);
      const at = new Date(2024, 2, 5, 14, 32, 0, 0);
      const time = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' }).format(at);
      const stamp = (tool: string, input: Record<string, unknown>, fire: Date, from?: Date) => ({
        ...meta(THREAD_TOOLS_SERVER, tool, input),
        next_fire_at: fire.getTime(),
        ...(from ? { next_fire_from: from.getTime() } : {}),
      });
      expect(aoToolPresentation(stamp('thread_loop', { label: 'board', every_seconds: 300 }, at, rowAt), names)!.what)
        .toBe(`board · every 5m · next ${time}`);
      expect(aoToolPresentation(stamp('thread_remind', { note: 'deploy' }, at, rowAt), names)!.what).toBe(`deploy · next ${time}`);
      const nextDay = new Date(2024, 2, 6, 0, 15, 0, 0);
      const day = new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'short', day: 'numeric' }).format(nextDay);
      const dayTime = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' }).format(nextDay);
      expect(aoToolPresentation(stamp('thread_remind', { note: 'deploy' }, nextDay, rowAt), names)!.what)
        .toBe(`deploy · next ${day} ${dayTime}`);
      // A row with no stamped day of its own says the date.
      const atDay = new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'short', day: 'numeric' }).format(at);
      expect(aoToolPresentation(stamp('thread_remind', { note: 'deploy' }, at), names)!.what).toBe(`deploy · next ${atDay} ${time}`);
    });

    it('carries the computer a cross-computer call names', () => {
      const p = aoToolPresentation(meta(THREAD_TOOLS_SERVER, 'thread_show', { computer_id: 'mac', thread_id: tid }), names)!;
      expect(p).toMatchObject({ computerId: 'mac', computerName: 'Macaroni-air' });
    });

    it('names what the reply named, from the stamp triage put on the row', () => {
      const stamped = (tool: string, input: Record<string, unknown>, aoResult: Record<string, unknown>) => ({
        ...meta(THREAD_TOOLS_SERVER, tool, input),
        ao_result: aoResult,
      });
      // The new thread's title beats the prompt's first line.
      const spawn = aoToolPresentation(stamped('thread_spawn', { prompt: 'You own the docs slice.\nRead first' }, {
        token: 'tok-1', thread_id: tid, title: 'Docs slice', state: 'running', outcome: 'backgrounded',
      }), names)!;
      expect(spawn.what).toBe('Docs slice');
      expect(spawn.result).toMatchObject({ token: 'tok-1', threadId: tid, state: 'running', outcome: 'backgrounded' });
      // A prefix the model typed reads as the title the reply resolved.
      expect(aoToolPresentation(stamped('thread_send', { thread_id: '7f2c9a', message: 'rebase' }, { title: 'Windows launcher' }), names)!.what)
        .toBe('Windows launcher · rebase');
      // A reply names its receiver through the thread list.
      expect(aoToolPresentation(stamped('thread_reply', { token: 't', text: 'Done.\nDetails' }, { thread_id: named }), names)!.what)
        .toBe('Release notes · Done.');
      // No stamp: the prompt's first line, as before the reply was read.
      expect(aoToolPresentation(meta(THREAD_TOOLS_SERVER, 'thread_spawn', { prompt: 'You own the docs slice.\nRead first' }), names)!.what)
        .toBe('You own the docs slice.');
    });

    it('reads a prompt the wire projection clipped to its opening', () => {
      const clipped = `${'Own the reporting slice.'}\n${'context '.repeat(50)}`.slice(0, 512) + '…';
      const p = aoToolPresentation({ ...meta(THREAD_TOOLS_SERVER, 'thread_spawn', { prompt: clipped }), wireElision: { input: ['prompt'] } })!;
      expect(p.what).toBe('Own the reporting slice.');
    });

    it('still presents a tool the table does not know', () => {
      const p = aoToolPresentation(meta(THREAD_TOOLS_SERVER, 'thread_future', { thread_id: tid }))!;
      expect(p).toMatchObject({ icon: 'speech-bubble', label: 'future', headerLabel: 'Thread future' });
    });
  });

  describe('workflow tools', () => {
    const rid = '1a2b3c4d-aaaa-bbbb-cccc-dddddddddddd';

    it.each([
      ['workflow_list', {}, 'list', 'workflows'],
      ['workflow_validate', { id: 'release' }, 'check', 'release'],
      ['workflow_validate', { path: '/cfg/workflows/draft.yaml' }, 'check', '/cfg/workflows/draft.yaml'],
      ['workflow_schema', {}, 'read', 'workflow schema'],
      ['workflow_new', { starter: 'blank', id: 'release', scope: 'project' }, 'new', 'release from blank'],
      ['run_start', { workflow_id: 'release', goal: 'Ship 2.4\nthen tag' }, 'start', 'release · "Ship 2.4"'],
      ['run_start', { workflow_id: 'release' }, 'start', 'release'],
      ['run_status', { run_id: rid, wait_seconds: 120 }, 'status', 'run 1a2b3c4d'],
      ['run_watch', { run_id: rid, tree: true }, 'watch', 'run 1a2b3c4d and its called runs'],
      ['run_output', { run_id: rid }, 'output', 'run 1a2b3c4d'],
      ['run_inspect', { run_id: rid, phase_id: 'review' }, 'probe', 'run 1a2b3c4d · phase review'],
      ['run_narrative', { run_id: rid, phase_id: 'build' }, 'story', 'run 1a2b3c4d · phase build'],
      ['run_list', { active_only: true }, 'list', 'active runs'],
      ['run_list', {}, 'list', 'runs'],
      ['run_control', { run_id: rid, action: 'retry_failed_units' }, 'control', 'retry failed units run 1a2b3c4d'],
      ['run_control', { run_id: rid, action: 'resume', phase_id: 'plan' }, 'control', 'resume run 1a2b3c4d · phase plan'],
      ['run_amend', { run_id: rid, seeds: { budget: 4, target: 'main' } }, 'amend', 'run 1a2b3c4d · budget, target'],
      ['run_guide', { run_id: rid, text: 'Skip the docs\nfor now' }, 'guide', 'run 1a2b3c4d · Skip the docs'],
      ['run_resolve', { run_id: rid, decision: 'approve' }, 'decide', 'approve run 1a2b3c4d'],
      ['memory_add', { kind: 'warning', text: 'The CI cache lies' }, 'note', 'warning: The CI cache lies'],
      ['memory_list', { kind: 'handoff' }, 'notes', 'handoff'],
      ['memory_list', {}, 'notes', 'campaign notes'],
    ])('%s', (tool, input, label, what) => {
      const p = aoToolPresentation(meta(WORKFLOW_TOOLS_SERVER, tool, input))!;
      expect(p).toMatchObject({ icon: 'checklist', label, what, headerLabel: `Workflow ${label}` });
    });

    it('names a run by this client\'s name for it, else by the workflow the reply named', () => {
      expect(aoToolPresentation(meta(WORKFLOW_TOOLS_SERVER, 'run_status', { run_id: namedRun }), names)!.what).toBe('digest · Weekly summary');
      const stamped = {
        ...meta(WORKFLOW_TOOLS_SERVER, 'run_status', { run_id: rid }),
        ao_result: { run_id: rid, workflow_id: 'release', state: 'running', outcome: 'backgrounded' },
      };
      expect(aoToolPresentation(stamped, names)!.what).toBe('release run 1a2b3c4d · running');
      const started = {
        ...meta(WORKFLOW_TOOLS_SERVER, 'run_start', { workflow_id: 'release', goal: 'Ship 2.4' }),
        ao_result: { run_id: rid, workflow_id: 'release', state: 'queued' },
      };
      expect(aoToolPresentation(started, names)!.what).toBe('release · "Ship 2.4" · queued');
    });

    it('still presents a tool the table does not know, by its own name', () => {
      const p = aoToolPresentation(meta(WORKFLOW_TOOLS_SERVER, 'run_future', { run_id: rid }))!;
      expect(p).toMatchObject({ icon: 'checklist', label: 'run_future', headerLabel: 'Workflow run_future' });
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
      ['browser_session', { name: 'Signup flow' }, 'use', 'Signup flow'],
      ['browser_visibility', { visible: true, page_id: 'p1' }, 'show', 'Checkout'],
      ['browser_visibility', { visible: false }, 'show', 'hide companion'],
      ['browser_viewport', { action: 'set', width: 1280, height: 720 }, 'size', '1280×720'],
      ['browser_close_page', { page_id: 'p1' }, 'close', 'Checkout'],
      ['browser_snapshot', {}, 'read', ''],
      ['browser_screenshot', { full_page: true }, 'shot', 'full page'],
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
      ['browser_clipboard', { action: 'write_text', text: 'abc' }, 'clip', 'write_text "abc"'],
      ['browser_console_logs', { filter: 'error' }, 'logs', 'error'],
      ['browser_downloads', { action: 'wait' }, 'files', 'wait'],
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

    it('reads an object input as one key and value per line and a list as its items, never JSON', () => {
      const facts = aoToolFacts(meta(WORKFLOW_TOOLS_SERVER, 'run_start', {
        workflow_id: 'digest', goal: 'Weekly summary', seeds: { goal: 'Weekly summary', budget: 4 }, tags: ['a', 'b'],
      }));
      expect(facts.find((f) => f.label === 'seeds')!.value).toBe('goal: Weekly summary\nbudget: 4');
      expect(facts.find((f) => f.label === 'tags')!.value).toBe('a, b');
    });

    it('lists what the header and the reply leave out: the threads of a list, the requests, never ids', () => {
      // The header names the one thread; the reply says whether notify armed.
      expect(aoToolFacts(meta(THREAD_TOOLS_SERVER, 'thread_send', { thread_id: named, message: 'hi', notify: true }), names)).toEqual([
        { label: 'message', value: 'hi' },
      ]);
      expect(aoToolFacts(meta(THREAD_TOOLS_SERVER, 'thread_update', { thread_ids: [named, '7f2c9a41-x'], archived: true }), names)).toEqual([
        { label: 'threads', value: 'Release notes, thread 7f2c9a41-x' },
        { label: 'archived', value: 'true' },
      ]);
      expect(aoToolFacts(meta(THREAD_TOOLS_SERVER, 'thread_status', { tokens: ['abcdef0123456789', 'fedcba9876543210'] }), names)).toEqual([
        { label: 'requests', value: 'abcdef01, fedcba98' },
      ]);
      expect(aoToolFacts(meta(WORKFLOW_TOOLS_SERVER, 'run_output', { run_id: namedRun }), names)).toEqual([]);
      // A spawn's overrides show in the reply's facts, its title as the link.
      expect(aoToolFacts(meta(THREAD_TOOLS_SERVER, 'thread_spawn', { prompt: 'p', title: 't', model: 'opus', notify: true }), names)).toEqual([
        { label: 'prompt', value: 'p' },
      ]);
    });

    it('counts the home files a spawn or send attaches rather than listing their paths', () => {
      expect(aoToolFacts(meta(THREAD_TOOLS_SERVER, 'thread_spawn', { prompt: 'p', home_files: ['/home/a.md', '/home/b.md'] }))).toEqual([
        { label: 'files', value: '2 files attached' },
        { label: 'prompt', value: 'p' },
      ]);
    });

    it('keeps a long input\'s lines for the body to fold', () => {
      const prompt = 'First line\nSecond line';
      const facts = aoToolFacts(meta(THREAD_TOOLS_SERVER, 'thread_spawn', { prompt }));
      expect(facts).toEqual([{ label: 'prompt', value: prompt }]);
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
