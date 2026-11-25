// Tracker connection rows as TrackerConnections answers them, for the
// Issues settings tests. Test data only: example.atlassian.net, User 1.

import type { TrackerBoardChoice, TrackerConnection, TrackerProblem } from '../../lib/stores/bindings';
import { State as KeyFileState } from '../../../bindings/agent-overflow/internal/keyfile/models';

export { KeyFileState };

/** A verified Jira connection with a token in its key file. */
export function trackerConnection(overrides: Partial<TrackerConnection> = {}): TrackerConnection {
  return {
    id: 'jira-1',
    kind: 'jira',
    site: 'https://example.atlassian.net',
    account: 'user1@example.com',
    label: '',
    enabled: true,
    verifiedAccountId: 'acc-user1',
    verifiedName: 'User 1',
    verifiedAt: 1_700_000_000_000,
    tokenExpiresAt: 0,
    createdAt: 1_690_000_000_000,
    keyFilePath: '/home/user/.config/agent-overflow/tracker-keys/jira-1',
    keyFileState: KeyFileState.StatePresent,
    // Written before the check.
    keyFileModifiedAt: KEY_FILE_SAVED_AT,
    ...overrides,
  };
}

/** When trackerConnection's key file was saved. */
export const KEY_FILE_SAVED_AT = 1_699_000_000_000;

/** A failure of a call whose token came from the key file saved at `keyFileModifiedAt` (trackerConnection's by default). */
export function trackerProblem(kind: TrackerProblem['kind'], message: string, keyFileModifiedAt = KEY_FILE_SAVED_AT): TrackerProblem {
  return { kind, message, keyFileModifiedAt };
}

/** A board a project lists, as ProjectTrackerBoards answers it. */
export function trackerBoardChoice(overrides: Partial<TrackerBoardChoice> = {}): TrackerBoardChoice {
  return {
    project: 'p1',
    connection: 'jira-1',
    connection_kind: 'jira',
    connection_label: '',
    connection_enabled: true,
    verified_account: 'acc-user1',
    board: '1',
    name: 'WEB board',
    tracker_project: 'WEB',
    opens_first: false,
    ...overrides,
  };
}
