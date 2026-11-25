// The fake issue trackers from a spec: seed a fake Jira site, connect the
// app to it, and list its board on a seeded project. The fixture shape is
// internal/harness/trackerfake's (jira_fixture.go); the harness serves it
// on loopback (internal/harnessrpc/tracker.go), and the app reaches it
// through its tracker origin.

import { writeFile } from 'node:fs/promises';

import type { Locator, Page } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import { expect, type SeedResult } from './fixtures.js';

export const JIRA_SITE = 'example.atlassian.net';
export const JIRA_ACCOUNT = 'user1@example.com';
export const JIRA_TOKEN = 'fake-jira-token-1';

/** The one scrum board the seed holds, and its active sprint. */
export const BOARD_ID = '1';
export const SPRINT_ID = '1';

/** What `TrackerConnections` and `AddTrackerConnection` answer. */
export interface TrackerConnection {
  id: string;
  kind: 'jira' | 'tempo' | 'linear';
  site: string;
  account: string;
  parentId?: string;
  keyFilePath: string;
  problem?: { kind: string; message: string } | null;
}

/** A board the app is connected to, listed on a seeded project. */
export interface ConnectedBoard {
  projectId: string;
  /** The threads seeded on the project, in the order asked for. */
  threadIds: string[];
  connectionId: string;
  /** The site as the connection names it, which a thread link names. */
  site: string;
  /** The fake Jira site's base URL, for writes a person makes in Jira. */
  jiraURL: string;
}

/**
 * More for the fake Jira site, in its fixture shape: projects, boards and
 * issues (trackerfake.JiraProject, JiraBoard and JiraIssue) beside the
 * seed's own, where an issue's created and updated times are the seed's,
 * and `poker` for the planning poker add-on (trackerfake.JiraPoker).
 */
export interface ExtraJira {
  projects: Array<{ id: string; key: string; name: string }>;
  boards: Array<{ id: number; name: string; type: 'scrum' | 'kanban'; project: string; columns: Array<{ name: string; statuses: string[] }> }>;
  issues?: Array<{
    id: string; key: string; project: string; type: string; status: string; summary: string; sprints?: number[]; assignee?: string;
    /** An Atlassian document. */
    description?: unknown;
    comments?: Array<{ id: string; author: string; body: unknown; created: string }>;
    dueDate?: string;
    /** Other fields' values by field ID (trackerfake.JiraIssue.Values). */
    values?: Record<string, unknown>;
    changelog?: Array<{ id: string; author?: string; created: string; items: Array<{ fieldId: string; field: string; fieldtype: string; from?: string | null; fromString?: string | null; to?: string | null; toString?: string | null }> }>;
    /** Files attached to the issue (trackerfake.JiraAttachment): `content`
     * is base64, `mediaId` the media node ID its text names it by. */
    attachments?: Array<{ id: string; filename: string; mimeType?: string; content: string; author?: string; created?: string; mediaId?: string }>;
  }>;
  /** Other people who sign in to the site (trackerfake.JiraMember), each
   * a user of `users` with a token of their own. */
  members?: Array<{ account: string; token: string }>;
  /** More users of the site beside User 1 (trackerfake.JiraUser). */
  users?: Array<{ accountId: string; displayName: string; email?: string; timeZone?: string }>;
  /** The site's custom fields (trackerfake.JiraField). */
  fields?: Array<{ id: string; name: string; schema: { type: string; items?: string; custom?: string; customId?: number }; options?: Array<{ id: string; value: string }> }>;
  resolutions?: Array<{ id: string; name: string }>;
  /** Links between issues (trackerfake.JiraLink): outward relates to inward by the type's outward name. */
  links?: Array<{ id: string; type: string; outward: string; inward: string }>;
  /** WEB's issue types with their edit screens (trackerfake.JiraIssueType) in place of the fake's. */
  issueTypes?: Array<{ id: string; name: string; subtask?: boolean; hierarchyLevel?: number; screen: Array<{ field: string; required?: boolean }> }>;
  /** WEB's workflow moves in place of one per status (trackerfake.JiraTransition). */
  transitions?: Array<{ id: string; name: string; to: string; from?: string[]; screen?: Array<{ field: string; required?: boolean }> }>;
  /** The field WEB's board estimates in. */
  estimation?: string;
  /** The add-on, with the votes already cast (trackerfake.JiraPoker). */
  poker?: { rounds?: Array<{ issue: string; revealed?: boolean; votes?: Array<{ id: string; account: string; vote: number; created: string }> }> };
}

/**
 * Seeds the fake Jira site: project WEB with one scrum board, its active
 * sprint holding WEB-1 and WEB-2, and WEB-3 in the backlog, and `extra`.
 * Then seeds a project, adds and verifies a Jira connection to the site,
 * and lists the board on the project. `threads` are seeded on the
 * project (HarnessSeed's thread shape).
 */
export async function connectFakeJiraBoard(
  harness: HarnessApp,
  name: string,
  extra?: ExtraJira,
  threads: ReadonlyArray<Record<string, unknown>> = [],
): Promise<ConnectedBoard> {
  const jiraURL = await seedFakeJira(harness, extra);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name, repo: {}, threads }] });
  const { projectId, threadIds } = seed.projects[0];
  return { ...await connectFakeJira(harness, projectId), threadIds, jiraURL };
}

/**
 * Adds and verifies a Jira connection to the seeded fake site (seedFakeJira)
 * and lists its board on `projectId`.
 */
export async function connectFakeJira(harness: HarnessApp, projectId: string): Promise<{ projectId: string; connectionId: string; site: string }> {
  const added = await harness.rpc<TrackerConnection>('AddTrackerConnection', { kind: 'jira', site: JIRA_SITE, account: JIRA_ACCOUNT });
  // The token a person would paste into the key file the app named.
  await writeFile(added.keyFilePath, JIRA_TOKEN, { mode: 0o600 });
  const verified = await harness.rpc<TrackerConnection>('VerifyTrackerConnection', added.id);
  if (verified.problem) throw new Error(`the fake Jira refused the connection: ${JSON.stringify(verified.problem)}`);
  // As the board picker sends it: built from the project's empty list,
  // with the name and project it showed.
  await harness.rpc('SetProjectTrackerBoards', projectId, [], [{ connection: added.id, board: BOARD_ID, opens_first: true, name: 'WEB board', tracker_project: 'WEB' }]);
  return { projectId, connectionId: added.id, site: verified.site };
}

/**
 * Seeds the fake Jira site alone (see connectFakeJiraBoard) and answers
 * its base URL.
 */
export async function seedFakeJira(harness: HarnessApp, extra?: ExtraJira): Promise<string> {
  const day = new Date(Date.now() - 48 * 3600_000).toISOString();
  const sprintEnd = new Date(Date.now() + 12 * 24 * 3600_000).toISOString();
  const origins = await harness.rpc<{ jira: Record<string, string> }>('HarnessTrackerSeed', {
    jira: [{
      site: JIRA_SITE, account: 'acc-user1', token: JIRA_TOKEN,
      users: [{ accountId: 'acc-user1', displayName: 'User 1', email: JIRA_ACCOUNT, timeZone: 'UTC' }, ...(extra?.users ?? [])],
      ...(extra?.members ? { members: extra.members } : {}),
      projects: [{
        id: '10000', key: 'WEB', name: 'Web',
        ...(extra?.issueTypes ? { issueTypes: extra.issueTypes } : {}),
        ...(extra?.transitions ? { transitions: extra.transitions } : {}),
      }, ...(extra?.projects ?? [])],
      boards: [{ id: 1, name: 'WEB board', type: 'scrum', project: 'WEB', ...(extra?.estimation ? { estimation: extra.estimation } : {}), columns: [
        { name: 'To Do', statuses: ['10000'] }, { name: 'In Progress', statuses: ['10001'] }, { name: 'Done', statuses: ['10002'] },
      ] }, ...(extra?.boards ?? [])],
      ...(extra?.fields ? { fields: extra.fields } : {}),
      ...(extra?.resolutions ? { resolutions: extra.resolutions } : {}),
      ...(extra?.links ? { links: extra.links } : {}),
      sprints: [{ id: 1, board: 1, name: 'Sprint 1', state: 'active', start: day, end: sprintEnd }],
      issues: [
        { id: '10101', key: 'WEB-1', project: 'WEB', type: '10001', summary: 'First story', sprints: [1], status: '10001', created: day, updated: day },
        { id: '10102', key: 'WEB-2', project: 'WEB', type: '10002', summary: 'Second task', sprints: [1], status: '10000', created: day, updated: day },
        { id: '10103', key: 'WEB-3', project: 'WEB', type: '10002', summary: 'Backlog task', status: '10000', created: day, updated: day },
        ...(extra?.issues ?? []).map((issue) => ({ ...issue, created: day, updated: day })),
      ],
      ...(extra?.poker ? { poker: extra.poker } : {}),
    }],
  });
  const jiraURL = origins.jira[JIRA_SITE];
  if (!jiraURL) throw new Error(`HarnessTrackerSeed served no ${JIRA_SITE}: ${JSON.stringify(origins)}`);
  return jiraURL;
}

/** A request to the fake Jira as the seeded user, as a person working in Jira makes it. */
async function inJira(jiraURL: string, method: string, path: string, body: unknown): Promise<Response> {
  return fetch(`${jiraURL}/rest/api/3/${path}`, {
    method,
    headers: {
      'Content-Type': 'application/json',
      authorization: `Basic ${Buffer.from(`${JIRA_ACCOUNT}:${JIRA_TOKEN}`).toString('base64')}`,
    },
    body: JSON.stringify(body),
  });
}

/** Renames an issue in the fake Jira, as a person editing it there would. */
export async function renameJiraIssue(board: ConnectedBoard, key: string, summary: string): Promise<void> {
  const resp = await inJira(board.jiraURL, 'PUT', `issue/${key}`, { fields: { summary } });
  if (resp.status !== 204) throw new Error(`the fake Jira refused the rename of ${key}: HTTP ${resp.status} ${await resp.text()}`);
}

/**
 * Logs `seconds` on `key` in the fake Jira at 09:00 UTC of `day`
 * (YYYY-MM-DD), the seeded user's zone, and answers the worklog's ID.
 */
export async function logJiraWork(jiraURL: string, key: string, day: string, seconds: number): Promise<string> {
  const resp = await inJira(jiraURL, 'POST', `issue/${key}/worklog`, { started: `${day}T09:00:00.000+0000`, timeSpentSeconds: seconds });
  if (resp.status !== 201) throw new Error(`the fake Jira refused the worklog on ${key}: HTTP ${resp.status} ${await resp.text()}`);
  return ((await resp.json()) as { id: string }).id;
}

/** Changes a worklog's time in the fake Jira, as a person editing it there would. */
export async function editJiraWorklog(jiraURL: string, key: string, id: string, seconds: number): Promise<void> {
  const resp = await inJira(jiraURL, 'PUT', `issue/${key}/worklog/${id}`, { timeSpentSeconds: seconds });
  if (resp.status !== 200) throw new Error(`the fake Jira refused the edit of worklog ${id}: HTTP ${resp.status} ${await resp.text()}`);
}

/** The fake Linear workspace's one team, which is its one board. */
export const LINEAR_TEAM = '00000000-0000-4000-8000-00000000000a';

/**
 * Seeds a fake Linear workspace with one team and its states, then seeds
 * a project, adds and verifies a Linear connection to it, and lists the
 * team's board on the project, opening first.
 */
export async function connectFakeLinearBoard(harness: HarnessApp, name: string): Promise<{ projectId: string; connectionId: string }> {
  const viewer = '00000000-0000-4000-8000-000000000001';
  const apiKey = 'lin_api_fake_key_1';
  const state = (n: number, stateName: string, type: string) => ({ id: `00000000-0000-4000-8000-0000000000${20 + n}`, team: LINEAR_TEAM, name: stateName, type, position: n });
  await harness.rpc('HarnessTrackerSeed', {
    linear: [{
      urlKey: 'example', name: 'Example', apiKey, viewer,
      users: [{ id: viewer, name: 'User 1', email: JIRA_ACCOUNT }],
      teams: [{ id: LINEAR_TEAM, key: 'WEB', name: 'Web', timezone: 'UTC', members: [viewer] }],
      states: [state(0, 'Todo', 'unstarted'), state(1, 'In Progress', 'started'), state(2, 'Done', 'completed')],
    }],
  });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name, repo: {} }] });
  const projectId = seed.projects[0].projectId;
  const added = await harness.rpc<TrackerConnection>('AddTrackerConnection', { kind: 'linear', site: 'example' });
  await writeFile(added.keyFilePath, apiKey, { mode: 0o600 });
  const verified = await harness.rpc<TrackerConnection>('VerifyTrackerConnection', added.id);
  if (verified.problem) throw new Error(`the fake Linear refused the connection: ${JSON.stringify(verified.problem)}`);
  await harness.rpc('SetProjectTrackerBoards', projectId, [], [{ connection: added.id, board: LINEAR_TEAM, opens_first: true, name: 'Web', tracker_project: LINEAR_TEAM }]);
  return { projectId, connectionId: added.id };
}

/** Opens the Issues pane from the tools row; on compact the thread screen holds it. */
export async function openIssuesPane(page: Page, compact: boolean): Promise<Locator> {
  await page.getByTestId('sidebar-tool-issues').click();
  if (compact) await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
  const pane = page.getByTestId('issues-pane');
  await expect(pane).toBeVisible();
  return pane;
}

/** A click, or a tap on compact. */
export async function press(target: Locator, compact: boolean): Promise<void> {
  await (compact ? target.tap() : target.click());
}

/** One request the fake trackers logged (trackerfake.Request). */
export interface LoggedRequest { route?: string; path: string; query?: Record<string, string[]>; status: number; held?: boolean; body?: unknown }

/** The fake's requests to `route`; with `issue` ([key, ID]), those naming it either way. */
export async function trackerRequests(harness: HarnessApp, route: string, issue?: readonly [string, string]): Promise<LoggedRequest[]> {
  const log = await harness.rpc<{ requests: LoggedRequest[] }>('HarnessTrackerRequests', 0);
  return log.requests.filter((r) => r.route === route && (!issue || r.path.split('/').some((part) => issue.includes(part))));
}

/** Opens Settings > Issues > Connections or Boards on `page`. */
export async function openIssuesPage(page: Page, name: 'Connections' | 'Boards'): Promise<void> {
  if (!(await page.getByRole('tablist', { name: 'Settings Sections' }).isVisible())) {
    await page.getByTestId('sidebar-settings-button').click();
  }
  await page.getByRole('tablist', { name: 'Settings Sections' }).getByRole('tab', { name, exact: true }).click();
}
