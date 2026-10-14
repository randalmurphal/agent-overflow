import { describe, expect, it } from 'vitest';
import { BROWSER_TOOLS_SERVER } from '../../utils/browserTools';
import { NO_AO_NAMES, REMOTE_TOOLS_SERVER, THREAD_TOOLS_SERVER, WORKFLOW_TOOLS_SERVER, type AoToolNames } from './aoTools';
import {
  aoResultView,
  aoToolError,
  browserResultView,
  parseFirstJson,
  remoteResultView,
  threadResultView,
  workflowResultView,
} from './aoToolResults';

const tid = '7f2c9a41-aaaa-bbbb-cccc-dddddddddddd';
const rid = '1a2b3c4d-aaaa-bbbb-cccc-dddddddddddd';
const names: AoToolNames = {
  ...NO_AO_NAMES,
  thread: (id) => (id === tid ? 'Docs slice' : ''),
  run: (id) => (id === rid ? 'digest · Weekly summary' : ''),
};

const json = (value: unknown) => JSON.stringify(value);

describe('AO tool errors', () => {
  it('splits the server code off the message', () => {
    expect(aoToolError('[thread_not_found] No thread matches "abc".')).toEqual({ code: 'thread_not_found', message: 'No thread matches "abc".' });
    expect(aoToolError('[workflow_failed] Line one\nline two')).toEqual({ code: 'workflow_failed', message: 'Line one\nline two' });
    expect(aoToolError('Element not found: #go')).toEqual({ code: '', message: 'Element not found: #go' });
  });
});

describe('parseFirstJson', () => {
  it('reads a reply followed by a note', () => {
    expect(parseFirstJson('{"a":"}"}\nNote: the page reloaded.')).toEqual({ a: '}' });
    expect(parseFirstJson('[1,2] trailing')).toEqual([1, 2]);
    expect(parseFirstJson('plain text')).toBeUndefined();
    expect(parseFirstJson('{"broken":')).toBeUndefined();
  });
});

describe('thread tool results', () => {
  it('reads a spawn ack as its thread, state and outcome, with the note while work continues', () => {
    const view = threadResultView('thread_spawn', json({
      token: '8613ba28-3563-47bd-b92a-8aa6f3f50ef5', kind: 'spawn', thread_id: tid, title: 'Docs slice',
      state: 'running', outcome: 'backgrounded', revision: 0, notify: true, note: 'Still working.',
    }), { prompt: 'p', worktree: 'docs/slice', base: 'main', model: 'opus', effort: 'high' }, names)!;
    expect(view.outcome).toBe('running · backgrounded');
    expect(view.link).toEqual({ kind: 'thread', id: tid, label: 'Docs slice', computerId: '', computerName: '' });
    expect(view.facts).toEqual([
      { label: 'branch', value: 'docs/slice' },
      { label: 'base', value: 'main' },
      { label: 'model', value: 'opus' },
      { label: 'effort', value: 'high' },
      { label: 'notify', value: 'armed' },
      { label: 'request', value: '8613ba28' },
    ]);
    expect(view.note).toBe('Still working.');
  });

  it('drops the note and shows the answer once the call carried it', () => {
    const view = threadResultView('thread_send', json({
      token: 't', thread_id: tid, title: 'Docs slice', state: 'finished', outcome: 'settled',
      answer_kind: 'final', answer: 'Done.\nAll green.', note: 'stale',
    }), { message: 'm' }, names)!;
    expect(view.outcome).toBe('finished');
    expect(view.preview).toEqual({ label: 'final', text: 'Done.\nAll green.' });
    expect(view.note).toBe('');
  });

  it('names a thread on another computer', () => {
    const view = threadResultView('thread_ask', json({ token: 't', thread_id: 'x1', title: 'Build', computer_id: 'c2', computer: 'Desk', state: 'blocked', outcome: 'blocked' }), {}, names)!;
    expect(view.link).toMatchObject({ id: 'x1', label: 'Build', computerId: 'c2', computerName: 'Desk' });
    expect(view.outcome).toBe('blocked on a person · blocked');
  });

  it('reads a reply as accepted with the thread it answered', () => {
    const view = threadResultView('thread_reply', json({ token: 't', accepted: true, state: 'replied', source_thread_id: tid }), {}, names)!;
    expect(view.outcome).toBe('accepted · replied');
    expect(view.link?.label).toBe('Docs slice');
  });

  it('lists one line per request a status call reports, with the answer\'s first lines', () => {
    const view = threadResultView('thread_status', json({
      requests: [
        { token: 'a', kind: 'send', thread_id: tid, title: 'Docs slice', state: 'finished', answer_kind: 'final', answer: 'Shipped.' },
        { token: 'b', kind: 'spawn', thread_id: 'x2', title: 'Tests', state: 'running' },
      ],
      woke_on: 'a',
    }), {}, names)!;
    expect(view.outcome).toBe('2 requests · woke on an answer');
    expect(view.lines.map((l) => [l.title, l.detail, l.text])).toEqual([
      ['Docs slice', 'send · finished · final', 'Shipped.'],
      ['Tests', 'spawn · running', ''],
    ]);
    expect(view.lines[0].link?.id).toBe(tid);
  });

  it('lists search hits from both result shapes with their snippet and count', () => {
    const solo = threadResultView('thread_search', json({
      rows: [{ thread_id: tid, title: 'Docs slice', state: 'idle', snippet: 'the deploy failed', last_message: { role: 'assistant', text: 'Done' } }],
      more: true,
    }), {}, names)!;
    expect(solo.outcome).toBe('1 thread · more');
    expect(solo.lines[0]).toMatchObject({ title: 'Docs slice', detail: 'idle · assistant spoke last', text: 'the deploy failed' });
    const agent = threadResultView('thread_search', json({ rows: [{ thread_id: 'w1', title: 'Worker', state: 'running', standing_agent: false, spawned_by: tid }, { thread_id: 'a1', title: 'Inbox agent', state: 'idle', standing_agent: true }] }), {}, names)!;
    expect(agent.lines.map((l) => l.detail)).toEqual(['running · spawned by Docs slice', 'idle · standing agent']);
    const grouped = threadResultView('thread_search', json({
      computers: [
        { computer_id: 'c1', computer: 'Laptop', rows: [{ thread_id: 'a', title: 'A', state: 'running' }] },
        { computer_id: 'c2', computer: 'Desk', error: 'did not answer', error_code: 'thread_computer_unreachable' },
      ],
    }), {}, names)!;
    expect(grouped.outcome).toBe('1 thread');
    expect(grouped.lines[0].detail).toBe('running · Laptop');
    expect(grouped.facts).toEqual([{ label: 'Desk', value: 'did not answer' }]);
  });

  it('reads a show as its size and a transcript to fold', () => {
    const view = threadResultView('thread_show', json({
      thread_id: tid, title: 'Docs slice', state: 'idle', window: 'tail', items: 11, bytes: 8873, done: true,
      transcript: '--- turn 1 ---\n[user] hi',
    }), {}, names)!;
    expect(view.outcome).toBe('tail · 11 items · 8.7 KB');
    expect(view.link?.label).toBe('Docs slice');
    expect(view.preview).toEqual({ label: 'transcript', text: '--- turn 1 ---\n[user] hi' });
    const agent = threadResultView('thread_show', json({ thread_id: 'a1', title: 'Inbox agent', window: 'tail', items: 1, bytes: 10, standing_agent: true, spawned_by: tid }), {}, names)!;
    expect(agent.facts).toEqual([{ label: 'kind', value: 'standing agent' }, { label: 'spawned by', value: 'Docs slice' }]);
  });

  it('reads an item read and an item search', () => {
    expect(threadResultView('thread_item', json({ thread_id: tid, item_id: 'i', size: 4096, offset: 0, bytes: 1024, text: 'abc', eof: false }), {}, names)!.outcome)
      .toBe('1.0 KB of 4.0 KB');
    const found = threadResultView('thread_item', json({ thread_id: tid, item_id: 'i', size: 9, offset: 0, bytes: 0, eof: false, query: 'FAIL', match_count: 2, matches: [{ offset: 10, context: 'x FAIL y' }, { offset: 90, context: 'FAIL' }] }), {}, names)!;
    expect(found.outcome).toBe('2 matches');
    expect(found.lines.map((l) => l.title)).toEqual(['byte 10', 'byte 90']);
  });

  it('reads a cancel, a loop and a group change', () => {
    expect(threadResultView('thread_cancel', json({ token: 't', state: 'cancelled', effect: 'turn_interrupted' }), {}, names)!.outcome)
      .toBe('turn interrupted · cancelled');
    expect(threadResultView('thread_loop', json({ kind: 'loop', label: 'board', state: 'active', note: 'n' }), {}, names)!.outcome).toBe('active');
    expect(threadResultView('thread_group', json({ group: 'Bugs', action: 'deleted', ungrouped: 3 }), {}, names)!.outcome)
      .toBe('deleted Bugs · 3 threads ungrouped');
  });

  it('reads a reply it has no decoder for as facts, never as JSON', () => {
    const view = threadResultView('thread_options', json({ provider: 'claude', models: ['a', 'b'], nested: { x: 1 } }), {}, names)!;
    expect(view.facts).toEqual([
      { label: 'provider', value: 'claude' },
      { label: 'models', value: '2 items' },
      { label: 'nested', value: '1 field' },
    ]);
  });

  it('leaves text that is not JSON to render as it is', () => {
    expect(threadResultView('thread_show', 'plain text', {}, names)).toBeNull();
  });
});

describe('workflow tool results', () => {
  it('reads a run start with its wait, notify and read error', () => {
    const view = workflowResultView('run_start', json({
      start: { itemId: rid, workflowId: 'digest', workflowScope: 'project', state: 'queued' },
      run: { itemId: rid, workflowId: 'digest', goal: 'Weekly summary', state: 'running', currentPhaseId: 'collect', currentPhaseOrdinal: 1, phaseCount: 3 },
      outcome: 'backgrounded',
      notify: { armed: false, detail: 'this thread is not bound to the run' },
    }), names)!;
    expect(view.outcome).toBe('running · backgrounded');
    expect(view.link).toEqual({ kind: 'run', id: rid, label: 'digest · Weekly summary', computerId: '', computerName: '' });
    expect(view.facts).toEqual([
      { label: 'phase', value: 'collect · 1 of 3' },
      { label: 'notify', value: 'unavailable · this thread is not bound to the run' },
    ]);
  });

  it('reads a start whose run could not be read back', () => {
    const view = workflowResultView('run_start', json({ start: { itemId: 'r9', workflowId: 'digest', state: 'queued' }, outcome: 'unknown', readError: 'database is locked' }), NO_AO_NAMES)!;
    expect(view.outcome).toBe('queued · unknown');
    expect(view.link?.label).toBe('digest');
    expect(view.facts).toContainEqual({ label: 'read error', value: 'database is locked' });
  });

  it('reads a run status and an output document', () => {
    expect(workflowResultView('run_status', json({ run: { itemId: 'r9', workflowId: 'digest', state: 'completed' }, outcome: 'resting' }), NO_AO_NAMES)!.outcome)
      .toBe('completed · resting');
    const output = workflowResultView('run_output', json({ itemId: 'r9', state: 'completed', resting: true, outputs: { summary: 'All good', count: 3 }, artifacts: ['report.md'] }), NO_AO_NAMES)!;
    expect(output.facts).toEqual([
      { label: 'summary', value: 'All good' },
      { label: 'count', value: '3' },
      { label: 'artifacts', value: 'report.md' },
    ]);
  });

  it('lists runs and workflows', () => {
    const runs = workflowResultView('run_list', json([{ itemId: rid, workflowId: 'digest', state: 'running' }, { itemId: 'r2', workflowId: 'release', goal: 'Ship', state: 'parked' }]), names)!;
    expect(runs.outcome).toBe('2 runs');
    expect(runs.lines.map((l) => l.title)).toEqual(['digest · Weekly summary', 'release · Ship']);
    expect(runs.lines[1].link).toMatchObject({ kind: 'run', id: 'r2' });
    const listing = workflowResultView('workflow_list', json({ workflows: [{ id: 'digest', name: 'Digest', scope: 'project', findings: [] }] }), names)!;
    expect(listing.lines[0]).toMatchObject({ title: 'Digest', detail: 'digest · project' });
  });

  it('reads a validation as its findings', () => {
    const view = workflowResultView('workflow_validate', json({ findings: [{ code: 'unknown_phase', element: 'phases[2]', message: 'No phase named x.' }], bindingStatus: 'ok' }), names)!;
    expect(view.outcome).toBe('1 finding');
    expect(view.lines[0]).toMatchObject({ title: 'phases[2]', detail: 'unknown_phase', text: 'No phase named x.' });
    expect(workflowResultView('workflow_validate', json({ findings: [], bindingStatus: 'ok' }), names)!.outcome).toBe('valid');
  });
});

describe('browser tool results', () => {
  it('reads a page state and a snapshot\'s text', () => {
    const view = browserResultView('browser_snapshot', json({ id: 'p1', url: 'https://example.test/', title: 'Example', canGoBack: false, text: 'Hello\nWorld', elements: [{ nodeId: 'n', selector: '#a', tag: 'a' }] }))!;
    expect(view.outcome).toBe('Example');
    expect(view.facts).toEqual([{ label: 'url', value: 'https://example.test/' }, { label: 'elements', value: '1' }]);
    expect(view.preview).toEqual({ label: 'page text', text: 'Hello\nWorld' });
  });

  it('reads an evaluation\'s value and the note after it', () => {
    const view = browserResultView('browser_evaluate', '{"off":true,"top":900}\nThe page navigated.')!;
    expect(view.preview?.text).toBe('{\n  "off": true,\n  "top": 900\n}');
    expect(view.note).toBe('The page navigated.');
    expect(browserResultView('browser_evaluate', '"a title"')!.preview?.text).toBe('"a title"');
  });

  it('lists pages', () => {
    const view = browserResultView('browser_pages', json([{ id: 'p1', url: 'https://a.test', title: 'A', selected: true }]))!;
    expect(view.lines[0]).toMatchObject({ title: 'A', detail: 'https://a.test · selected' });
    expect(browserResultView('browser_pages', '[]')!.outcome).toBe('no pages');
  });
});

describe('remote tool results', () => {
  const id = '98312d67-1111-2222-3333-444444444444';
  it('reads a run reply as its outcome and output, and leaves other text alone', () => {
    const reply = json({ id, computerId: 'mac', state: 'succeeded', exitCode: 0, startedAt: 1000, finishedAt: 13500, output: 'ok\n', outputHead: 'go: downloading\n', outputHint: 'Older output was discarded.' });
    expect(remoteResultView(reply)).toEqual({ outcome: 'succeeded · exit 0 · 12.5s', error: '', head: 'go: downloading\n', output: 'ok\n', hint: 'Older output was discarded.' });
    expect(remoteResultView(json({ id, state: 'running', backgrounded: true, exitCode: -1, startedAt: 1000 }))).toMatchObject({ outcome: 'running in the background' });
    expect(remoteResultView(json({ id, state: 'failed', exitCode: 2, error: 'tests failed' }))).toMatchObject({ outcome: 'failed · exit 2', error: 'tests failed' });
    expect(remoteResultView('plain text')).toBeNull();
    expect(remoteResultView(json({ pages: [] }))).toBeNull();
  });

  it('reads the other remote replies as lines and facts', () => {
    expect(aoResultView(REMOTE_TOOLS_SERVER, 'remote_run', json({ id, state: 'running' }), {}, NO_AO_NAMES)).toBeNull();
    const computers = aoResultView(REMOTE_TOOLS_SERVER, 'remote_computers', json([{ id: 'mac', name: 'Laptop', status: 'online' }]), {}, NO_AO_NAMES)!;
    expect(computers.lines[0]).toMatchObject({ title: 'Laptop', detail: 'online' });
  });
});

describe('aoResultView', () => {
  it('dispatches by server', () => {
    expect(aoResultView(THREAD_TOOLS_SERVER, 'thread_status', json({ requests: [] }), {}, NO_AO_NAMES)!.outcome).toBe('nothing open');
    expect(aoResultView(WORKFLOW_TOOLS_SERVER, 'run_status', json({ run: { itemId: 'r', state: 'running' }, outcome: 'running' }), {}, NO_AO_NAMES)!.outcome).toBe('running');
    expect(aoResultView(BROWSER_TOOLS_SERVER, 'browser_close_page', json({ closed: true }), {}, NO_AO_NAMES)!.facts).toEqual([{ label: 'closed', value: 'true' }]);
    expect(aoResultView('docs', 'lookup', json({ a: 1 }), {}, NO_AO_NAMES)).toBeNull();
  });
});
