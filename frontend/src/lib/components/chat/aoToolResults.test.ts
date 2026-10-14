import { describe, expect, it } from 'vitest';
import { BROWSER_TOOLS_SERVER } from '../../utils/browserTools';
import { NO_AO_NAMES, REMOTE_TOOLS_SERVER, THREAD_TOOLS_SERVER, WORKFLOW_TOOLS_SERVER, type AoToolNames } from './aoTools';
import {
  aoResultView,
  aoToolError,
  browserResultView,
  parseFirstJson,
  readFirstJson,
  remoteResultView,
  threadResultView,
  workflowResultView,
} from './aoToolResults';

const tid = '7f2c9a41-aaaa-bbbb-cccc-dddddddddddd';
const rid = '1a2b3c4d-aaaa-bbbb-cccc-dddddddddddd';
const names: AoToolNames = {
  ...NO_AO_NAMES,
  thread: (id) => (id === tid ? 'Docs slice' : ''),
  threadId: (id) => (id === tid || (id.length >= 6 && tid.startsWith(id)) ? tid : ''),
  run: (id) => (id === rid ? 'digest · Weekly summary' : ''),
};

const json = (value: unknown) => JSON.stringify(value);

describe('AO tool errors', () => {
  it('splits the server code off the message', () => {
    expect(aoToolError('[thread_not_found] No thread matches "abc".')).toEqual({ code: 'thread_not_found', message: 'No thread matches "abc".' });
    expect(aoToolError('[workflow_failed] Line one\nline two')).toEqual({ code: 'workflow_failed', message: 'Line one\nline two' });
    expect(aoToolError('Element not found: #go')).toEqual({ code: '', message: 'Element not found: #go' });
  });

  it('reads a code only at the start of the text', () => {
    expect(aoToolError('Navigation failed: [net_error] refused')).toEqual({ code: '', message: 'Navigation failed: [net_error] refused' });
  });
});

describe('parseFirstJson', () => {
  it('reads a reply followed by a note', () => {
    expect(parseFirstJson('{"a":"}"}\nNote: the page reloaded.')).toEqual({ a: '}' });
    expect(parseFirstJson('[1,2] trailing')).toEqual([1, 2]);
    expect(parseFirstJson('plain text')).toBeUndefined();
    expect(parseFirstJson('{"broken":')).toBeUndefined();
  });

  it('reads any JSON value, a bare string or number included, and the text after it', () => {
    expect(readFirstJson('"a title"\nThe page navigated.')).toEqual({ value: 'a title', rest: 'The page navigated.' });
    expect(readFirstJson('42\nnote')).toEqual({ value: 42, rest: 'note' });
    expect(readFirstJson('null')).toEqual({ value: null, rest: '' });
    expect(readFirstJson('true story')).toEqual({ value: true, rest: 'story' });
    expect(readFirstJson('42abc')).toBeNull();
    expect(readFirstJson('plain text')).toBeNull();
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
    const view = threadResultView('thread_send', json({ token: 't', thread_id: 'x1', title: 'Build', computer_id: 'c2', computer: 'Desk', state: 'blocked', outcome: 'blocked' }), {}, names)!;
    expect(view.link).toMatchObject({ id: 'x1', label: 'Build', computerId: 'c2', computerName: 'Desk' });
    expect(view.outcome).toBe('blocked on a person · blocked');
  });

  it('names and links the thread asked, never the hidden copy that answers', () => {
    const view = threadResultView('thread_ask', json({ token: 't', thread_id: 'fork-1', title: 'Docs slice (ask)', state: 'running', outcome: 'backgrounded' }), { thread_id: tid, question: 'Why?' }, names)!;
    expect(view.link).toMatchObject({ kind: 'thread', id: tid, label: 'Docs slice' });
    expect(view.facts).toContainEqual({ label: 'asked', value: 'a read-only copy' });
  });

  it('links an ask addressed by an id prefix to the full id, and leaves an unknown prefix as text', () => {
    const ack = json({ token: 't', thread_id: 'fork-1', state: 'running', outcome: 'backgrounded' });
    const byPrefix = threadResultView('thread_ask', ack, { thread_id: tid.slice(0, 8), question: 'Why?' }, names)!;
    expect(byPrefix.link).toMatchObject({ kind: 'thread', id: tid, label: 'Docs slice' });
    expect(byPrefix.facts.find((f) => f.label === 'thread')).toBeUndefined();
    const unknown = threadResultView('thread_ask', ack, { thread_id: 'target-thr', question: 'Why?' }, names)!;
    expect(unknown.link).toBeNull();
    expect(unknown.facts).toContainEqual({ label: 'thread', value: 'target-thr' });
  });

  it('reads a list reply as lines, never as JSON', () => {
    const view = threadResultView('thread_status', json([{ token: 'abcdef0123', state: 'running' }, { thread_id: tid, state: 'idle' }]), {}, names)!;
    expect(view.outcome).toBe('2 items');
    expect(view.lines.map((l) => [l.title, l.detail])).toEqual([['request abcdef01', 'running'], ['Docs slice', 'idle']]);
    expect(view.lines[1].link).toMatchObject({ kind: 'thread', id: tid });
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
    // The context starts and ends mid-line; the row shows the line that matched.
    const midLine = threadResultView('thread_item', json({ thread_id: tid, item_id: 'i', size: 9, query: 'needle', matches: [{ offset: 40, context: 'arsing 56\nline 57\nNEEDLE panic: overran\nline 59\nline 6' }] }), {}, names)!;
    expect(midLine.lines[0].text).toBe('NEEDLE panic: overran');
    // The header names the item; the body does not list its id.
    expect(found.facts.map((f) => f.label)).not.toContain('item');
  });

  it('titles a request with no thread by its kind, once', () => {
    const view = threadResultView('thread_status', json({ requests: [{ token: 'r', kind: 'remind', state: 'accepted', answer_kind: 'note' }] }), {}, names)!;
    expect(view.lines[0]).toMatchObject({ title: 'remind', detail: 'accepted · note' });
  });

  it('reads a cancel, a loop and a group change', () => {
    expect(threadResultView('thread_cancel', json({ token: 't', state: 'cancelled', effect: 'turn_interrupted' }), {}, names)!.outcome)
      .toBe('turn interrupted · cancelled');
    expect(threadResultView('thread_loop', json({ kind: 'loop', label: 'board', state: 'active', note: 'n' }), {}, names)!.outcome).toBe('active');
    expect(threadResultView('thread_group', json({ group: 'Bugs', action: 'deleted', ungrouped: 3 }), {}, names)!.outcome)
      .toBe('deleted Bugs · 3 threads ungrouped');
  });

  it('reads a reply it has no decoder for as facts, never as JSON', () => {
    const view = threadResultView('thread_options', json({ provider: 'claude', models: ['a', 'b'], nested: { x: 1, deep: { a: 1, b: { c: 2 } } } }), {}, names)!;
    expect(view.facts).toEqual([
      { label: 'provider', value: 'claude' },
      { label: 'models', value: 'a, b' },
      // An object nested in a fact reads as its field count, never its JSON.
      { label: 'nested', value: 'x: 1\ndeep: 2 fields' },
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

describe('workflow replies routed by tool', () => {
  it('reads run_watch as where the run is, its transitions and the repair sentence', () => {
    const view = workflowResultView('run_watch', json({
      itemId: rid,
      cursor: 12,
      transitions: [
        { seq: 11, at: 1, itemId: rid, phaseId: 'collect', attempt: 1, from: 'running', to: 'needs-human', reason: 'gate', resting: true },
        { seq: 12, at: 2, itemId: 'child-1', to: 'running', resting: false },
      ],
      run: { itemId: rid, workflowId: 'digest', goal: 'Weekly summary', state: 'needs-human', reason: 'gate', phaseId: 'review', resting: true, repair: 'Approve or reject with run_resolve.' },
    }), names)!;
    expect(view.outcome).toBe('needs-human · resting · 2 transitions');
    expect(view.link).toMatchObject({ kind: 'run', id: rid, label: 'digest · Weekly summary' });
    expect(view.facts).toEqual([{ label: 'phase', value: 'review' }, { label: 'reason', value: 'gate' }]);
    expect(view.lines.map((l) => [l.title, l.detail])).toEqual([
      ['collect', 'running → needs-human · gate'],
      ['run', 'started running'],
    ]);
    expect(view.lines[1].link).toMatchObject({ kind: 'run', id: 'child-1' });
    expect(view.note).toBe('Approve or reject with run_resolve.');
  });

  it('reads run_amend as what changed and when the run reads it', () => {
    const view = workflowResultView('run_amend', json({
      itemId: rid, names: ['budget', 'target'], seeds: { budget: 4, target: 'main', other: 'x' }, phaseId: 'build',
      effect: 'next_phase', appliesNote: 'The run reads the new values when build ends.', callerNote: 'Its caller will not carry this change.',
    }), names)!;
    expect(view.outcome).toBe('amended budget, target · next phase');
    expect(view.link).toMatchObject({ kind: 'run', id: rid });
    expect(view.facts).toEqual([{ label: 'phase', value: 'build' }, { label: 'budget', value: '4' }, { label: 'target', value: 'main' }]);
    expect(view.note).toBe('The run reads the new values when build ends. Its caller will not carry this change.');
  });

  it('reads run_guide as the entries waiting, who left them, and when the run reads them', () => {
    const view = workflowResultView('run_guide', json({
      itemId: rid, pending: 2, maxPending: 8, by: 'operator', state: 'running', phaseId: 'build',
      deliversNote: 'The run reads this when it enters its next phase.', quarantineNote: 'An unreadable entry was set aside.',
    }), names)!;
    expect(view.outcome).toBe('2 entries waiting of 8 · running');
    expect(view.facts).toEqual([{ label: 'by', value: 'operator' }, { label: 'phase', value: 'build' }]);
    expect(view.note).toBe('The run reads this when it enters its next phase. An unreadable entry was set aside.');
  });

  it('reads memory_add as where the note went', () => {
    const view = workflowResultView('memory_add', json({ itemId: 'r9', rootId: rid, kind: 'lesson', wave: 1, path: '.ao/memory/notes.jsonl' }), names)!;
    expect(view.outcome).toBe('lesson added · wave 1');
    expect(view.link).toMatchObject({ kind: 'run', id: rid, label: 'digest · Weekly summary' });
    expect(view.facts).toEqual([{ label: 'file', value: '.ao/memory/notes.jsonl' }]);
  });

  it('reads memory_list as one line per note', () => {
    const view = workflowResultView('memory_list', json({
      itemId: rid, rootId: rid, path: '.ao/memory/notes.jsonl', total: 5, skipped: 1,
      notes: [
        { kind: 'lesson', text: 'Run the linter first.', files: ['a.go'], provenance: { runId: rid, phaseId: 'build', attempt: 1, wave: 0 }, at: 1791900000000 },
        { kind: 'fact', text: 'CI takes 9 minutes.', provenance: { runId: rid, wave: 0 }, at: 1791900000000 },
      ],
    }), names)!;
    expect(view.outcome).toBe('2 of 5 notes · 1 unreadable');
    expect(view.lines.map((l) => [l.title, l.text])).toEqual([['lesson', 'Run the linter first.'], ['fact', 'CI takes 9 minutes.']]);
    expect(view.lines[0].detail).toMatch(/^build · 1 file · /);
  });

  it('reads run_inspect as the run, where its work is, its children and guidance', () => {
    const view = workflowResultView('run_inspect', json({
      run: { itemId: rid, workflowId: 'digest', goal: 'Weekly summary', state: 'running', currentPhaseId: 'build', currentPhaseOrdinal: 2, phaseCount: 3, resting: false },
      branch: 'wf/digest', baseBranch: 'main', worktreePath: '/w/digest',
      children: [{ itemId: 'c1', workflowId: 'wave', state: 'completed', parentPhaseId: 'build' }],
      guidance: [{ text: 'Skip the docs.', at: 1, ageSeconds: 90, by: 'operator' }],
    }), names)!;
    expect(view.outcome).toBe('running');
    expect(view.facts).toContainEqual({ label: 'branch', value: 'wf/digest · from main' });
    expect(view.facts).toContainEqual({ label: 'guidance', value: '1 entry waiting' });
    expect(view.lines.map((l) => l.title)).toEqual(['wave', 'guidance']);
    expect(view.lines[0].link).toMatchObject({ kind: 'run', id: 'c1' });
    expect(view.lines[1].text).toBe('Skip the docs.');
  });

  it('reads run_narrative as the account, folded', () => {
    const view = workflowResultView('run_narrative', json({ itemId: rid, phaseId: 'build', attempt: 2, path: 'n.md', present: true, bytes: 2048, truncated: true, content: '# Account' }), names)!;
    expect(view.outcome).toBe('2.0 KB · clipped');
    expect(view.facts).toContainEqual({ label: 'phase', value: 'build · attempt 2' });
    expect(view.preview).toEqual({ label: 'account', text: '# Account' });
  });

  it('reads run_control answers: a scheduled resume and an action whose run could not be read', () => {
    const scheduled = workflowResultView('run_control', json({ armedAt: '2026-10-14T09:00:00Z', run: { itemId: rid, workflowId: 'digest', state: 'paused' } }), names)!;
    expect(scheduled.outcome).toBe('paused');
    expect(scheduled.facts.map((f) => f.label)).toContain('resumes');
    const done = workflowResultView('run_control', json({ itemId: rid, action: 'cancel', done: true, readError: 'database is locked' }), names)!;
    expect(done.outcome).toBe('cancel · done');
    expect(done.link).toMatchObject({ kind: 'run', id: rid });
    expect(done.facts).toContainEqual({ label: 'read error', value: 'database is locked' });
  });
});

describe('browser tool results', () => {
  it('reads a page state and a snapshot\'s text', () => {
    const view = browserResultView('browser_snapshot', json({ id: 'p1', url: 'https://example.test/', title: 'Example', canGoBack: false, text: 'Hello\nWorld', elements: [{ nodeId: 'n', selector: '#a', tag: 'a' }] }), names)!;
    expect(view.outcome).toBe('Example');
    expect(view.facts).toEqual([{ label: 'url', value: 'https://example.test/' }, { label: 'elements', value: '1' }]);
    expect(view.preview).toEqual({ label: 'page text', text: 'Hello\nWorld' });
  });

  it('reads an evaluation\'s value and the note after it', () => {
    // An object reads as facts and a list as lines, never as JSON.
    const view = browserResultView('browser_evaluate', '{"off":true,"top":900,"box":{"w":10,"h":20}}\nThe page navigated.', names)!;
    expect(view.outcome).toBe('returned');
    expect(view.facts).toEqual([
      { label: 'off', value: 'true' },
      { label: 'top', value: '900' },
      { label: 'box', value: 'w: 10\nh: 20' },
    ]);
    expect(view.preview).toBeNull();
    expect(view.note).toBe('The page navigated.');
    const rows = browserResultView('browser_evaluate_readonly', json([{ name: 'Save', state: 'enabled' }, 'plain']), names)!;
    expect(rows.outcome).toBe('returned · 2 items');
    expect(rows.lines.map((l) => [l.title, l.detail])).toEqual([['Save', 'enabled'], ['plain', '']]);
    expect(rows.preview).toBeNull();
    // A scalar reads as itself, and the WebKit note after it is the note.
    const title = browserResultView('browser_evaluate', '"a title"\nWebKit evaluates in an isolated world.', names)!;
    expect(title.preview?.text).toBe('a title');
    expect(title.note).toBe('WebKit evaluates in an isolated world.');
    const count = browserResultView('browser_evaluate', '42\nWebKit evaluates in an isolated world.', names)!;
    expect(count.preview?.text).toBe('42');
    expect(count.note).toBe('WebKit evaluates in an isolated world.');
  });

  it('lists pages', () => {
    const view = browserResultView('browser_pages', json([{ id: 'p1', url: 'https://a.test', title: 'A', selected: true }]), names)!;
    expect(view.lines[0]).toMatchObject({ title: 'A', detail: 'https://a.test · selected' });
    expect(browserResultView('browser_pages', '[]', names)!.outcome).toBe('no pages');
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
    const logs = aoResultView(REMOTE_TOOLS_SERVER, 'remote_jobs', json([{ id: 'j1', name: 'Go tests', status: 'running' }]), {}, NO_AO_NAMES)!;
    expect(logs.lines[0]).toMatchObject({ title: 'Go tests', detail: 'running' });
  });

  it('reads each computer as offline with its error, or as its system, projects and tools', () => {
    const view = aoResultView(REMOTE_TOOLS_SERVER, 'remote_computers', json([
      {
        id: 'c1', name: 'Build box', enabled: true,
        projects: [{ id: 'p1', name: 'app', path: '/w/app', worktrees: [] }],
        environment: { executionOS: 'linux', architecture: 'amd64', hostOS: 'windows', environment: 'wsl', distribution: 'Ubuntu', executables: { git: '/usr/bin/git', go: '/usr/bin/go' } },
      },
      { id: 'c2', name: 'Laptop', enabled: true, projects: [], error: 'The computer is not reachable.' },
      { id: 'c3', name: 'Mini', enabled: true, projects: [], environment: null, environmentError: 'environment read timed out' },
      {
        id: 'c4', name: 'Desk', enabled: true, projects: [],
        environment: { executionOS: 'linux', architecture: 'arm64', hostOS: 'windows', environment: 'wsl', executables: {} },
      },
    ]), {}, NO_AO_NAMES)!;
    expect(view.outcome).toBe('4 computers · 1 offline');
    expect(view.lines.map((l) => [l.title, l.detail, l.text])).toEqual([
      ['Build box', 'linux/amd64 · WSL Ubuntu · 1 project', 'git, go'],
      ['Laptop', 'offline', 'The computer is not reachable.'],
      ['Mini', '0 projects', 'environment read timed out'],
      // WSL is named only with its distribution; without one the platform says it.
      ['Desk', 'linux/arm64 · 0 projects', ''],
    ]);
  });
});

describe('workflow_schema', () => {
  it('reads the authoring schema as its fields and one line per definition, never as JSON', () => {
    const schema = {
      $schema: 'https://json-schema.org/draft/2020-12/schema',
      title: 'Agent Overflow Workflow',
      type: 'object',
      required: ['id', 'name', 'phases'],
      properties: { id: { type: 'string' }, name: { type: 'string' }, phases: { type: 'array' } },
      $defs: {
        phase: { type: 'object', properties: { id: {}, prompt: {}, gate: {} } },
        effort: { description: 'Reasoning tier.', enum: ['low', 'high'] },
        loopBound: { description: 'How many times.', anyOf: [{ type: 'integer' }, { type: 'string' }] },
      },
    };
    const view = workflowResultView('workflow_schema', json(schema), names)!;
    expect(view.outcome).toBe('Agent Overflow Workflow · 3 fields · 3 definitions');
    expect(view.facts).toEqual([
      { label: 'required', value: 'id, name, phases' },
      { label: 'fields', value: 'id, name, phases' },
    ]);
    expect(view.lines.map((l) => [l.title, l.detail, l.text])).toEqual([
      ['phase', 'object', 'id, prompt, gate'],
      ['effort', '2 values', 'low, high'],
      ['loopBound', '2 forms', 'How many times.'],
    ]);
    expect(view.preview).toBeNull();
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
