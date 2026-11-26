// The per-project Issues pages on their shared frame (Fields, Starting
// work, Agents), each as Status groups behaves: a save that lost to
// another said, with what is stored shown; a save whose outcome is
// unknown said in the computer's words, and the setup read again; a
// setup that cannot be read said with Try again; a section a newer build
// saved said with no control; and a device that may not change it offered
// no change.

import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import type { Component } from 'svelte';
import type { Setup, SetupView, SiteFields, SiteStatuses } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TrackerFieldsSettings from './TrackerFieldsSettings.svelte';
import TrackerStartWorkSettings from './TrackerStartWorkSettings.svelte';
import TrackerAgentsSettings from './TrackerAgentsSettings.svelte';
import ComputerSettingsPage from './ComputerSettingsPage.svelte';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { readSidebarCatalogs } from '../../../test/helpers/sidebarCatalogs';
import { trackerBoardChoice, trackerConnection } from '../../../test/helpers/trackerConnections';
import { noteProject } from '../../transport/entityIndex';
import { TransportError } from '../../transport/wsClient';
import { setCarriedSessionScopes } from '../../transport/scopes';
import { addProjectLocal } from '../../stores/projects.svelte';

const SITE = 'https://example.atlassian.net';

interface PageCase {
  name: string;
  Page: Component;
  /** The test ID stem of the page. */
  page: string;
  section: string;
  /** The control one change is made with. */
  control: () => HTMLElement;
  /** The setup that control's change stores, from `setup`. */
  changed: (setup: Setup) => Setup;
  /** What the control shows of `setup`. */
  shows: (setup: Setup) => string;
  /** What the control shows now. */
  showing: () => string;
  what: string;
}

const pinned = () => screen.queryAllByTestId('tracker-fields-pinned').filter((el) => el.dataset.id === 'reporter');

const CASES: PageCase[] = [
  {
    name: 'Fields',
    Page: TrackerFieldsSettings,
    page: 'fields',
    section: 'ticket_pins',
    control: () => pinned()[0],
    changed: (setup) => ({ ...setup, ticket_pins: [{ tracker: 'jira', site: SITE, fields: [] }] }),
    shows: (setup) => String(setup.ticket_pins?.some((p) => p.fields?.includes('reporter')) ?? false),
    showing: () => String(pinned().length > 0),
    what: 'pinned fields and sections',
  },
  {
    name: 'Starting work',
    Page: TrackerStartWorkSettings,
    page: 'start-work',
    section: 'start_work',
    control: () => screen.getByRole('switch', { name: 'Assign it to me' }),
    changed: (setup) => ({ ...setup, start_work: [{ tracker: 'jira', site: SITE, project: 'WEB', assign: false }] }),
    shows: (setup) => String(setup.start_work?.find((d) => d.project === 'WEB')?.assign ?? true),
    showing: () => screen.getByRole('switch', { name: 'Assign it to me' }).getAttribute('aria-checked')!,
    what: 'Start work defaults',
  },
  {
    name: 'Agents',
    Page: TrackerAgentsSettings,
    page: 'agents',
    section: 'agents',
    control: () => screen.getByRole('switch', { name: 'Change tickets' }),
    changed: (setup) => ({ ...setup, agents: { change_tickets: false } }),
    shows: (setup) => String(setup.agents?.change_tickets ?? true),
    showing: () => screen.getByRole('switch', { name: 'Change tickets' }).getAttribute('aria-checked')!,
    what: 'Agents settings',
  },
];

const FIELDS: SiteFields[] = [{ tracker: 'jira', site: SITE, fields: [{ id: 'reporter', name: 'Reporter', kind: 'user', value: {} }] }];
const STATUSES: SiteStatuses[] = [{ tracker: 'jira', site: SITE, statuses: [{ id: '10001', name: 'In Progress', group: 'in_progress' }] }];

let stored: SetupView;
let saves: { revision: string; setup: Setup }[];

function view(setup: Partial<Setup>, revision: string, newer?: string[]): SetupView {
  return { revision, setup: { status_groups: [], ticket_pins: [{ tracker: 'jira', site: SITE, fields: ['reporter'] }], start_work: [], ...setup }, newer } as SetupView;
}

beforeEach(async () => {
  setBindingMock('ListProjects', async () => []);
  await readSidebarCatalogs();
  noteProject('p1', '');
  addProjectLocal({ id: 'p1', name: 'web', path: '/repos/web', createdAt: 1, updatedAt: 1, sortPosition: 0, archived: false });
  setBindingMock('TrackerConnections', async () => [trackerConnection()]);
  setBindingMock('ProjectTrackerBoards', async () => [trackerBoardChoice()]);
  setBindingMock('ProjectTrackerFields', async () => FIELDS);
  setBindingMock('ProjectTrackerStatuses', async () => STATUSES);
  stored = view({}, 'r0');
  saves = [];
  setBindingMock('ProjectTrackerSetup', async () => stored);
  setBindingMock('SetProjectTrackerSetup', async (_project: string, revision: string, setup: Setup) => {
    saves.push({ revision, setup });
    if (revision !== stored.revision) return { saved: false, refused: "this change was not saved: the project's tracker setup changed after it was read; it is shown as it is now", stored };
    stored = { revision: `r${saves.length}`, setup } as SetupView;
    return { saved: true, stored };
  });
});

describe.each(CASES)('the $name page', (c) => {
  async function shown(): Promise<void> {
    render(c.Page);
    await waitFor(() => expect(c.control()).toBeTruthy());
  }

  it('saves a change at once and shows what is stored', async () => {
    await shown();
    await fireEvent.click(c.control());
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0].revision).toBe('r0');
    expect(c.shows(saves[0].setup)).toBe(c.shows(c.changed(view({}, 'r0').setup)));
    await waitFor(() => expect(c.showing()).toBe(c.shows(stored.setup)));
  });

  it('says a save that lost to another was not saved, and shows what is stored', async () => {
    await shown();
    // Another window saved since, and this window has not heard it yet.
    stored = view({}, 'other');
    await fireEvent.click(c.control());
    const note = await screen.findByTestId(`settings-tracker-${c.page}-save-note`);
    expect(note.textContent?.trim()).toBe("This change was not saved: the project's tracker setup changed after it was read; it is shown as it is now. Make the change again.");
    expect(c.showing()).toBe(c.shows(stored.setup));
  });

  it('says a save whose outcome is unknown in the computer\'s words, and reads the setup again', async () => {
    await shown();
    const sentence = "this change may or may not be stored: the computer that holds the project could not finish updating /x/tracker.json (sync failed); read the project's tracker setup again before you change it again";
    setBindingMock('SetProjectTrackerSetup', async (_project: string, _revision: string, setup: Setup) => {
      stored = { revision: 'r9', setup } as SetupView;
      throw new TransportError('tracker_setup_unsure', sentence, { detail: { ref: 'r1', method: 'SetProjectTrackerSetup', at: 1, chain: [sentence] } });
    });
    const before = c.showing();
    await fireEvent.click(c.control());
    const note = await screen.findByTestId(`settings-tracker-${c.page}-save-note`);
    expect(note.textContent?.trim()).toBe(`T${sentence.slice(1)}.`);
    await waitFor(() => expect(c.showing(), 'what the file holds after it').not.toBe(before));
  });

  it('says a setup file that cannot be read, offers Try again and no control', async () => {
    const unreadable = new TransportError('tracker_setup_unreadable', "this project's tracker setup cannot be read from /x/tracker.json on the computer that holds the project (unexpected end of JSON input); fix the file or remove it, and it is read again at its next use");
    setBindingMock('ProjectTrackerSetup', async () => { throw unreadable; });
    render(c.Page);
    const error = await screen.findByTestId(`settings-tracker-${c.page}-error`);
    expect(error.textContent).toContain('/x/tracker.json');
    expect(screen.queryAllByRole('switch').length + pinned().length).toBe(0);
    setBindingMock('ProjectTrackerSetup', async () => stored);
    await fireEvent.click(screen.getByTestId(`settings-tracker-${c.page}-retry`));
    await waitFor(() => expect(c.control()).toBeTruthy());
    expect(screen.queryByTestId(`settings-tracker-${c.page}-error`)).toBeNull();
  });

  it(`says a newer build saved its section, and offers no control`, async () => {
    stored = view({}, 'r1', [c.section]);
    render(c.Page);
    expect((await screen.findByTestId(`settings-tracker-${c.page}-newer`)).textContent).toContain(`saved this project's ${c.what}`);
    expect(screen.queryAllByRole('switch').length + pinned().length).toBe(0);
  });

  describe('on an attached computer', () => {
    beforeEach(() => {
      stageBackend();
      noteProject('p1', 'laptop');
    });

    it('offers no change to a device that may not operate its trackers', async () => {
      setCarriedSessionScopes('laptop', ['tracker:read']);
      render(ComputerSettingsPage, { props: { backend: 'laptop', Page: c.Page, needsComputer: false } });
      await waitFor(() => expect(c.control()).toBeTruthy());
      expect(c.control()).toBeDisabled();
      expect(c.control().title).toBe(`This device may not change a project's ${c.what} on this computer.`);
      resetStagedBackends();
    });
  });
});
