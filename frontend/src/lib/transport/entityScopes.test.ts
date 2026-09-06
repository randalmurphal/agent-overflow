import { afterEach, beforeEach, expect, it } from 'vitest';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { __resetEntityIndexForTest, noteAutomation, noteProject, noteThread, noteWorkflowItem } from './entityIndex';
import { __resetScopesForTest, setCarriedSessionScopes } from './scopes';
import { automationHasScope, projectHasScope, threadHasScope, workflowItemHasScope } from './entityScopes';

beforeEach(() => {
  __resetEntityIndexForTest();
  __resetScopesForTest();
  setCarriedSessionScopes('writable', ['threads:operate', 'attachments:write', 'threads:autonomy', 'git:operate']);
  setCarriedSessionScopes('readonly', ['threads:read']);
});

afterEach(resetStagedBackends);

it('refuses ambiguous unknown entities instead of borrowing HOME permissions', () => {
  stageBackend({ id: 'readonly' });
  expect(threadHasScope('threads:operate', 'unknown')).toBe(false);
  expect(projectHasScope('git:operate', 'unknown')).toBe(false);
  expect(workflowItemHasScope('threads:autonomy', 'unknown')).toBe(false);
  expect(automationHasScope('threads:autonomy', 'unknown')).toBe(false);
});

it('checks the owning computer for each visible entity and for a draft’s project', () => {
  for (const computer of ['writable', 'readonly']) {
    noteProject(`project-${computer}`, computer);
    noteThread(`thread-${computer}`, computer, 0);
    noteWorkflowItem(`run-${computer}`, computer);
    noteAutomation(`automation-${computer}`, computer);
    const granted = computer === 'writable';
    expect(threadHasScope('threads:operate', `thread-${computer}`)).toBe(granted);
    expect(threadHasScope('attachments:write', null, `project-${computer}`)).toBe(granted);
    expect(projectHasScope('git:operate', `project-${computer}`)).toBe(granted);
    expect(workflowItemHasScope('threads:autonomy', `run-${computer}`)).toBe(granted);
    expect(automationHasScope('threads:autonomy', `automation-${computer}`)).toBe(granted);
  }
});

it('changes grants with a moved conversation rather than retaining the source’s grants', () => {
  noteThread('moving', 'writable', 0);
  expect(threadHasScope('attachments:write', 'moving')).toBe(true);
  noteThread('moving', 'readonly', 1);
  expect(threadHasScope('attachments:write', 'moving')).toBe(false);
  noteThread('moving', 'writable', 2);
  expect(threadHasScope('attachments:write', 'moving')).toBe(true);
});
