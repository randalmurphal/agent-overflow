import { describe, expect, it } from 'vitest';
import {
  getSettingsComputer,
  getSettingsSection,
  isSettingsOpen,
  openSettingsOverlay,
  openTrackerBoardsSettings,
  requestedTrackerBoardsProject,
  takeRequestedTrackerBoardsProject,
} from './settingsOverlay.svelte';

describe('the Boards deep link', () => {
  it('opens Boards on the project’s computer and holds the project until the page takes it', () => {
    openTrackerBoardsSettings('p9', 'laptop');
    expect(isSettingsOpen()).toBe(true);
    expect(getSettingsSection()).toBe('tracker-boards');
    expect(getSettingsComputer()).toBe('laptop');
    expect(requestedTrackerBoardsProject()).toBe('p9');
    takeRequestedTrackerBoardsProject();
    expect(requestedTrackerBoardsProject()).toBeNull();
  });

  it('is forgotten by any other open, so a later visit keeps its own choice', () => {
    openTrackerBoardsSettings('p9', 'laptop');
    openSettingsOverlay('tracker-boards');
    expect(requestedTrackerBoardsProject()).toBeNull();
  });
});
