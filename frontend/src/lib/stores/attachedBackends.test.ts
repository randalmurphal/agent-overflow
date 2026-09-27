import { afterEach, expect, it } from 'vitest';
import { threadMachine } from './attachedBackends.svelte';
import { draftPlaceholderId } from './draftPlaceholderId';
import { __resetEntityIndexForTest, noteProject, noteThread } from '../transport/entityIndex';
import { HOME_BACKEND } from '../transport/backendKey';

afterEach(__resetEntityIndexForTest);

it('places a draft placeholder on the computer of the project its id names', () => {
  noteProject('remote-project', 'laptop');
  const draft = draftPlaceholderId('main', 'remote-project', 'chat');
  expect(threadMachine(draft, null)).toBe('laptop');
  expect(threadMachine(draft, 'remote-project')).toBe('laptop');
});

it('places a thread by its own row before any project', () => {
  noteProject('remote-project', 'laptop');
  noteThread('thread-1', HOME_BACKEND, 0);
  expect(threadMachine('thread-1', 'remote-project')).toBe(HOME_BACKEND);
  expect(threadMachine('unknown-thread', 'remote-project')).toBe('laptop');
});
