// The fake issue trackers from a spec: seed a fake Jira site, connect the
// app to it, and list its board on a seeded project. The fixture shape is
// internal/harness/trackerfake's (jira_fixture.go); the harness serves it
// on loopback (internal/harnessrpc/tracker.go), and the app reaches it
// through its tracker origin.

import { writeFile } from 'node:fs/promises';

import type { Page } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import type { SeedResult } from './fixtures.js';

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
  connectionId: string;
  /** The fake Jira site's base URL, for writes a person makes in Jira. */
  jiraURL: string;
}

/**
 * More for the fake Jira site, in its fixture shape: projects, boards and
 * issues (trackerfake.JiraProject, JiraBoard and JiraIssue) beside the
 * seed's own, where an issue's created and updated times are the seed's,
 * and `poker: {}` for the planning poker add-on (trackerfake.JiraPoker).
 */
export interface ExtraJira {
  projects: Array<{ id: string; key: string; name: string }>;
  boards: Array<{ id: number; name: string; type: 'scrum' | 'kanban'; project: string; columns: Array<{ name: string; statuses: string[] }> }>;
  issues?: Array<{
    id: string; key: string; project: string; type: string; status: string; summary: string; sprints?: number[]; assignee?: string;
    /** An Atlassian document. */
    description?: unknown;
    comments?: Array<{ id: string; author: string; body: unknown; created: string }>;
  }>;
  poker?: Record<string, never>;
}

/**
 * Seeds the fake Jira site: project WEB with one scrum board, its active
 * sprint holding WEB-1 and WEB-2, and WEB-3 in the backlog, and `extra`.
 * Then seeds a project, adds and verifies a Jira connection to the site,
 * and lists the board on the project.
 */
export async function connectFakeJiraBoard(harness: HarnessApp, name: string, extra?: ExtraJira): Promise<ConnectedBoard> {
  const jiraURL = await seedFakeJira(harness, extra);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name, repo: {} }] });
  const projectId = seed.projects[0].projectId;
  const added = await harness.rpc<TrackerConnection>('AddTrackerConnection', { kind: 'jira', site: JIRA_SITE, account: JIRA_ACCOUNT });
  // The token a person would paste into the key file the app named.
  await writeFile(added.keyFilePath, JIRA_TOKEN, { mode: 0o600 });
  const verified = await harness.rpc<TrackerConnection>('VerifyTrackerConnection', added.id);
  if (verified.problem) throw new Error(`the fake Jira refused the connection: ${JSON.stringify(verified.problem)}`);
  // As the board picker sends it: built from the project's empty list,
  // with the name and project it showed.
  await harness.rpc('SetProjectTrackerBoards', projectId, [], [{ connection: added.id, board: BOARD_ID, opens_first: true, name: 'WEB board', tracker_project: 'WEB' }]);
  return { projectId, connectionId: added.id, jiraURL };
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
      users: [{ accountId: 'acc-user1', displayName: 'User 1', email: JIRA_ACCOUNT, timeZone: 'UTC' }],
      projects: [{ id: '10000', key: 'WEB', name: 'Web' }, ...(extra?.projects ?? [])],
      boards: [{ id: 1, name: 'WEB board', type: 'scrum', project: 'WEB', columns: [
        { name: 'To Do', statuses: ['10000'] }, { name: 'In Progress', statuses: ['10001'] }, { name: 'Done', statuses: ['10002'] },
      ] }, ...(extra?.boards ?? [])],
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

/** Renames an issue in the fake Jira, as a person editing it there would. */
export async function renameJiraIssue(board: ConnectedBoard, key: string, summary: string): Promise<void> {
  const resp = await fetch(`${board.jiraURL}/rest/api/3/issue/${key}`, {
    method: 'PUT',
    headers: {
      'Content-Type': 'application/json',
      authorization: `Basic ${Buffer.from(`${JIRA_ACCOUNT}:${JIRA_TOKEN}`).toString('base64')}`,
    },
    body: JSON.stringify({ fields: { summary } }),
  });
  if (resp.status !== 204) throw new Error(`the fake Jira refused the rename of ${key}: HTTP ${resp.status} ${await resp.text()}`);
}

/** Opens Settings > Issues > Connections or Boards on `page`. */
export async function openIssuesPage(page: Page, name: 'Connections' | 'Boards'): Promise<void> {
  if (!(await page.getByRole('tablist', { name: 'Settings Sections' }).isVisible())) {
    await page.getByTestId('sidebar-settings-button').click();
  }
  await page.getByRole('tablist', { name: 'Settings Sections' }).getByRole('tab', { name, exact: true }).click();
}
