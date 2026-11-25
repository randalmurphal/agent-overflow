// Drift guard for the two hand-kept id tables: `ROUTE_BY_ID_FAMILY` here
// and `RESULT_FAMILIES` inside ./entityIndex.ts.
//
// A Wails method id is a hash of the method's name, so it moves the moment
// the method is renamed and the bindings are regenerated. A table that
// drifts does not fail to compile — it silently stops matching, and what
// stops working is routing: the call quietly reverts to the home backend,
// which is invisible on a single-backend app, which is every developer's
// machine. So the tripwire has to be a test.
//
// The generated `METHOD_ROUTES` is the reference, because methodgen writes
// it from the same scan that writes internal/transport/methods_gen.go: an
// id that is no longer in it is an id that no longer exists.
import { describe, expect, it } from 'vitest';

import { ROUTE_BY_ID_FAMILY, familyBackend } from './methodFamilies';
import { METHOD_ROUTES } from './methodRoutes';
import generatedBindings from '../../../bindings/agent-overflow/app.ts?raw';
import {
  __resetEntityIndexForTest,
  noteFamilyRowsFromCall,
  noteTerminal,
  noteThread,
  noteProject,
  noteWorkflowItem,
  terminalBackend,
  workflowItemBackend,
} from './entityIndex';

const WORKFLOW_GET_ITEM = 70120675;
const WORKFLOW_LIST_ITEMS = 3037887964;
const GIT_STATUS_UNSUBSCRIBE = 3263989430;
const LIST_TERMINALS = 2445206506;
const WRITE_TERMINAL = 146795716;

describe('the id-family route table', () => {
  it('covers only methods the generator parked on home', () => {
    // That is the whole reason this table exists. A method the generator
    // routes to a thread or a project needs no family, and shadowing one
    // here would be two answers to one question. An id the generated table
    // no longer has at all is a method that is not bound any more.
    for (const id of Object.keys(ROUTE_BY_ID_FAMILY).map(Number)) {
      expect(METHOD_ROUTES[id], `id ${id} is not a bound method parked on home`).toBe('home');
    }
  });

  it('pins every tracker method that names a connection or a subscription', () => {
    // A connection, and a subscription its computer minted, mean one
    // computer; unpinned, the call goes home and finds neither there.
    const bound = /^export function (\w+)\((connectionID|subscriptionID): string[^\n]*\n(?:[^\n]*\n)*?\s*return \$Call\.ByID\((\d+)/gm;
    const missing: string[] = [];
    let seen = 0;
    for (const [, name, arg, id] of generatedBindings.matchAll(bound)) {
      if (!name.includes('Tracker')) continue;
      seen++;
      const want = arg === 'connectionID' ? 'trackerConnection' : 'subscription';
      if (ROUTE_BY_ID_FAMILY[Number(id)] !== want) missing.push(`${name} (${id}) as ${want}`);
    }
    expect(seen).toBeGreaterThan(40);
    expect(missing).toEqual([]);
  });
});

describe('familyBackend', () => {
  it('routes declared object fields without mistaking neighboring IDs for an owner', () => {
    __resetEntityIndexForTest();
    noteWorkflowItem('run', 'gpu');
    noteProject('project', 'mac');
    expect(familyBackend(595194384, [{ projectId: 'project', itemId: 'run' }])).toBe('mac');
    expect(familyBackend(595194384, [{ itemId: 'run', workflowId: 'run' }])).toBeUndefined();
    __resetEntityIndexForTest();
  });
  it('rechecks every thread in a group operation after one moves computers', () => {
    __resetEntityIndexForTest();
    noteThread('a', 'mac', 0);
    noteThread('b', 'mac', 0);
    expect(familyBackend(2514763466, [['a', 'b'], ''])).toBe('mac');
    noteThread('b', 'gpu', 1);
    expect(() => familyBackend(2514763466, [['a', 'b'], ''])).toThrow('same computer');
    expect(() => familyBackend(2514763466, [['b', 'a'], ''])).toThrow('same computer');
    expect(() => familyBackend(2514763466, [['missing', 'b'], ''])).toThrow('no longer available');
    expect(() => familyBackend(2514763466, [['b', 'missing'], ''])).toThrow('no longer available');
    __resetEntityIndexForTest();
  });
  it('answers undefined for a method that names no family', () => {
    expect(familyBackend(WORKFLOW_LIST_ITEMS, ['project-1'])).toBeUndefined();
  });

  it('answers undefined for an id the index has never seen, so the route decides', () => {
    __resetEntityIndexForTest();
    expect(familyBackend(WORKFLOW_GET_ITEM, ['item-1'])).toBeUndefined();
    __resetEntityIndexForTest();
  });

  it('answers the backend a terminal was opened on', () => {
    __resetEntityIndexForTest();
    noteTerminal('term-1', 'laptop');
    expect(familyBackend(WRITE_TERMINAL, ['term-1'])).toBe('laptop');
    __resetEntityIndexForTest();
  });

  it('answers undefined for a missing or non-string first argument', () => {
    expect(familyBackend(GIT_STATUS_UNSUBSCRIBE, [])).toBeUndefined();
    expect(familyBackend(GIT_STATUS_UNSUBSCRIBE, [7])).toBeUndefined();
    expect(familyBackend(GIT_STATUS_UNSUBSCRIBE, [''])).toBeUndefined();
  });
});

describe('learning family ids from what a call answered', () => {
  it('indexes a list answer row by row', () => {
    __resetEntityIndexForTest();
    noteFamilyRowsFromCall(
      WORKFLOW_LIST_ITEMS,
      [{ id: 'item-a' }, { id: 'item-b' }, null, { id: '' }],
      'laptop',
    );
    expect(workflowItemBackend('item-a')).toBe('laptop');
    expect(workflowItemBackend('item-b')).toBe('laptop');
    __resetEntityIndexForTest();
  });

  it('indexes a single-row answer under its own id key', () => {
    __resetEntityIndexForTest();
    // ListTerminals rows carry `terminalID`, not `id` — reading the wrong
    // property would index nothing and every later write would go home.
    noteFamilyRowsFromCall(LIST_TERMINALS, [{ terminalID: 'term-9' }], 'laptop');
    expect(terminalBackend('term-9')).toBe('laptop');
    __resetEntityIndexForTest();
  });

  it('does nothing for a method that answers no family', () => {
    __resetEntityIndexForTest();
    noteFamilyRowsFromCall(WRITE_TERMINAL, [{ id: 'x' }], 'laptop');
    expect(workflowItemBackend('x')).toBeUndefined();
    expect(terminalBackend('x')).toBeUndefined();
  });
});
