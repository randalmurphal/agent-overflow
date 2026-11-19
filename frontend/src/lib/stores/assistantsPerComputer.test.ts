// Each computer shows or hides its own assistants (docs/specs/standing-
// agents.md, "Turning assistants off"): the entry points that act on an
// assistant follow the setting of the computer it lives on, and New
// Assistant the computer it creates on, whatever this computer's says.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { makeSettings } from '../../test/helpers/settings';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { HOME_BACKEND } from '../transport/backendKey';
import { __resetEntityIndexForTest, noteProject, noteThread } from '../transport/entityIndex';
import { assistantsOffNote, hiddenAssistantThread } from './assistants.svelte';
import { closePreviousThreadsDialog, getPreviousThreads, openPreviousThreadsDialog } from './assistantPastThreads.svelte';
import {
  closeAssistantProjectsDialog,
  closeNewAssistantDialog,
  getAssistantProjectsOpening,
  getNewAssistantRequest,
  openAssistantProjectsDialog,
  openNewAssistantDialog,
} from './newAssistant.svelte';
import { setSelectedBackend } from './selectedBackend.svelte';
import { applySettingsSnapshot, resetSettingsForTest } from './settings.svelte';

describe('assistants on each computer by its own setting', () => {
  beforeEach(() => {
    stageBackend();
    noteThread('here', HOME_BACKEND);
    noteThread('there', 'laptop');
    noteProject('project-there', 'laptop');
    setBindingMock('ListAssistantPastThreads', async () => ({ threads: [], next: null }));
    // This computer has them off, the laptop on.
    applySettingsSnapshot(makeSettings({ assistantsEnabled: false }));
    applySettingsSnapshot(makeSettings({ assistantsEnabled: true }), 'laptop');
  });

  afterEach(() => {
    closeNewAssistantDialog();
    closeAssistantProjectsDialog();
    closePreviousThreadsDialog();
    setSelectedBackend(HOME_BACKEND);
    resetStagedBackends();
    __resetEntityIndexForTest();
    resetSettingsForTest();
  });

  it('hides the threads of the computer with them off only', () => {
    expect(hiddenAssistantThread({ id: 'here', agentId: 'ops' })).toBe(true);
    expect(hiddenAssistantThread({ id: 'there', agentId: 'scout' })).toBe(false);
    expect(hiddenAssistantThread({ id: 'here' })).toBe(false);
  });

  it('opens New Assistant for the computer it creates on', () => {
    openNewAssistantDialog({});
    expect(getNewAssistantRequest()).toBeNull();
    openNewAssistantDialog({ projectId: 'project-there' });
    expect(getNewAssistantRequest()?.projectId).toBe('project-there');
    closeNewAssistantDialog();
    setSelectedBackend('laptop');
    openNewAssistantDialog({});
    expect(getNewAssistantRequest()?.selectedBackend).toBe('laptop');
  });

  it("opens an assistant's Projects and past threads by its own computer", () => {
    openAssistantProjectsDialog({ id: 'here', agentId: 'ops' });
    expect(getAssistantProjectsOpening()).toBeNull();
    openAssistantProjectsDialog({ id: 'there', agentId: 'scout' });
    expect(getAssistantProjectsOpening()?.agentId).toBe('scout');

    openPreviousThreadsDialog({ id: 'here', agentId: 'ops' }, null);
    expect(getPreviousThreads()).toBeNull();
    openPreviousThreadsDialog({ id: 'there', agentId: 'scout' }, null);
    expect(getPreviousThreads()?.opening.agentId).toBe('scout');
  });

  it('names the computer with them off', () => {
    expect(assistantsOffNote(HOME_BACKEND)).toMatch(/^Assistants are turned off in Settings on .+\. Turn them on to continue this thread\.$/);
  });
});
