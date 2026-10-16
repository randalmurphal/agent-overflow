import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { assistantDisplayName, sameAssistantName } from './assistants.svelte';
import nameFolds from '../../../../internal/store/testdata/assistant_name_folds.json';
import { replaceAllThreads } from './threads.svelte';
import { __resetEntityIndexForTest, noteThread } from '../transport/entityIndex';
import { HOME_BACKEND } from '../transport/backendKey';
import { makeThread } from '../../test/helpers/chat';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';

// An assistant's name is unique per computer; two attached computers may each
// hold it, and both rows then name their computer (docs/specs/standing-agents.md,
// "Sidebar").
describe('assistantDisplayName', () => {
  beforeEach(() => {
    __resetEntityIndexForTest();
    stageBackend({ id: 'laptop', name: 'Laptop' });
  });
  afterEach(() => {
    replaceAllThreads([]);
    resetStagedBackends();
  });

  it('suffixes both computers while the same name is on two, and only then', () => {
    const here = makeThread({ id: 'here', title: 'Ops', agentId: 'a1' });
    const there = makeThread({ id: 'there', title: ' ops ', agentId: 'a2' });
    noteThread('here', HOME_BACKEND);
    noteThread('there', 'laptop');
    replaceAllThreads([here, there]);
    expect(assistantDisplayName(there)).toBe(' ops -Laptop');
    expect(assistantDisplayName(here)).toMatch(/^Ops-/);

    replaceAllThreads([here, { ...there, title: 'Scout' }]);
    expect(assistantDisplayName(here)).toBe('Ops');
  });

  it('never suffixes an ordinary thread, nor two threads of one computer', () => {
    const plain = makeThread({ id: 'plain', title: 'Ops' });
    const assistant = makeThread({ id: 'assistant', title: 'Ops', agentId: 'a1' });
    noteThread('plain', 'laptop');
    noteThread('assistant', HOME_BACKEND);
    replaceAllThreads([plain, assistant]);
    expect(assistantDisplayName(assistant)).toBe('Ops');
    expect(assistantDisplayName(plain)).toBe('Ops');
  });

  it('compares names ignoring case and surrounding space', () => {
    expect(sameAssistantName(' Ops', 'OPS ')).toBe(true);
    expect(sameAssistantName('Ops', 'Op s')).toBe(false);
  });
});

// The same cases the store's SameAssistantName is held to.
describe('sameAssistantName folds as the store does', () => {
  it.each(nameFolds)('$a and $b are the same name: $same', ({ a, b, same }) => {
    expect(sameAssistantName(a, b)).toBe(same);
  });
});
