// Settings > Issues > Agents: Read tickets always on, Change tickets and
// Ask before changing several at once on until turned off, each saved at
// once in the setup's agents section, and a rule turned back on stored as
// none, so a project on the built-in rules has no section.

import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import type { Setup, SetupView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TrackerAgentsSettings from './TrackerAgentsSettings.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { readSidebarCatalogs } from '../../../test/helpers/sidebarCatalogs';
import { trackerConnection } from '../../../test/helpers/trackerConnections';
import { noteProject } from '../../transport/entityIndex';
import { addProjectLocal } from '../../stores/projects.svelte';

let stored: SetupView;
let saves: Setup[];

beforeEach(async () => {
  setBindingMock('ListProjects', async () => []);
  await readSidebarCatalogs();
  noteProject('p1', '');
  addProjectLocal({ id: 'p1', name: 'web', path: '/repos/web', createdAt: 1, updatedAt: 1, sortPosition: 0, archived: false });
  setBindingMock('TrackerConnections', async () => [trackerConnection()]);
  stored = { revision: '', setup: { status_groups: [], ticket_pins: [], start_work: [] } } as SetupView;
  saves = [];
  setBindingMock('ProjectTrackerSetup', async () => stored);
  setBindingMock('SetProjectTrackerSetup', async (_project: string, _revision: string, setup: Setup) => {
    saves.push(setup);
    stored = { revision: `r${saves.length}`, setup } as SetupView;
    return { saved: true, stored };
  });
});

const checked = (name: string) => screen.getByRole('switch', { name }).getAttribute('aria-checked');

describe('<TrackerAgentsSettings>', () => {
  it('shows the built-in rules, saves each off at once, and stores a rule back on as none', async () => {
    render(TrackerAgentsSettings);
    const read = await screen.findByRole('switch', { name: 'Read tickets' });
    expect(read).toBeDisabled();
    expect(read.title).toBe('Always on');
    expect([checked('Read tickets'), checked('Change tickets'), checked('Ask before changing several at once')]).toEqual(['true', 'true', 'true']);

    await fireEvent.click(screen.getByRole('switch', { name: 'Change tickets' }));
    await waitFor(() => expect(checked('Change tickets')).toBe('false'));
    expect(saves[0].agents).toEqual({ change_tickets: false });
    await fireEvent.click(screen.getByRole('switch', { name: 'Ask before changing several at once' }));
    await waitFor(() => expect(checked('Ask before changing several at once')).toBe('false'));
    expect(saves[1].agents).toEqual({ change_tickets: false, ask_several: false });

    await fireEvent.click(screen.getByRole('switch', { name: 'Change tickets' }));
    await waitFor(() => expect(saves).toHaveLength(3));
    expect(saves[2].agents).toEqual({ ask_several: false });
    await fireEvent.click(screen.getByRole('switch', { name: 'Ask before changing several at once' }));
    await waitFor(() => expect(saves).toHaveLength(4));
    expect('agents' in saves[3], 'a project on the built-in rules has no section').toBe(false);
  });
});
