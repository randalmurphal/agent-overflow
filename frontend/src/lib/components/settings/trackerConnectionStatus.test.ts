import { describe, expect, it } from 'vitest';
import {
  localDateValue,
  startOfLocalDay,
  TOKEN_WARNING_DAYS,
  tokenExpiry,
  trackerConnectionName,
  trackerConnectionStatus,
} from './trackerConnectionStatus';
import { KeyFileState, trackerConnection, trackerProblem } from '../../../test/helpers/trackerConnections';
import { FailureKind } from '../../../../bindings/agent-overflow/internal/trackerapp/models';

const DAY = 24 * 60 * 60 * 1000;

describe('a connection row’s status', () => {
  it('names who a verified connection is signed in as', () => {
    expect(trackerConnectionStatus(trackerConnection())).toEqual({
      tone: 'ok',
      label: 'Signed in as User 1 (user1@example.com)',
      detail: '',
      needsSetup: false,
    });
    const linear = trackerConnection({ kind: 'linear', site: 'example', account: '' });
    expect(trackerConnectionStatus(linear).label).toBe('Signed in as User 1');
  });

  it('says a check that names nobody (Tempo’s) checked the token, and never who it was signed in as', () => {
    // What a successful Tempo check answers: a time and no account (trackerapp/verify.go).
    const tempo = trackerConnection({ id: 'tempo-1', kind: 'tempo', site: 'https://api.tempo.io', account: '', parentId: 'jira-1', verifiedAccountId: '', verifiedName: '' });
    expect(trackerConnectionStatus(tempo)).toEqual({ tone: 'ok', label: 'Token checked', detail: '', needsSetup: false });
    const refused = { ...tempo, problem: trackerProblem(FailureKind.FailureSetup, 'Tempo refused the token in the key file') };
    expect(trackerConnectionStatus(refused).detail).toBe('Tempo refused the token in the key file.');
  });

  it('shows the setup steps until a token is in the key file and checked', () => {
    const fresh = trackerConnection({ verifiedAt: 0, verifiedName: '', verifiedAccountId: '', checkedTokenInFile: false, keyFileState: KeyFileState.StateEmpty });
    expect(trackerConnectionStatus(fresh)).toMatchObject({ tone: 'warning', label: 'Needs a token', detail: 'The key file is empty.', needsSetup: true });
    const refused = { ...fresh, keyFileState: KeyFileState.StateRefused, keyFileProblem: 'the key file can be read by other users; make it owner-only' };
    expect(trackerConnectionStatus(refused).detail).toBe('the key file can be read by other users; make it owner-only');
    const unchecked = { ...fresh, keyFileState: KeyFileState.StatePresent };
    expect(trackerConnectionStatus(unchecked)).toEqual({
      tone: 'muted',
      label: 'Not checked',
      detail: 'The key file holds a token. Check the connection to see whose it is.',
      needsSetup: true,
    });
  });

  // The computer says whether the file holds the token the check verified;
  // a replaced token, or a check that recorded none, is not signed in, and
  // the row does not claim which.
  it('says who it was last checked as while the token in the file is not the one checked', () => {
    expect(trackerConnectionStatus(trackerConnection({ checkedTokenInFile: false }))).toEqual({
      tone: 'muted',
      label: 'Token not checked',
      detail: 'It was last checked as User 1 (user1@example.com). The token in the key file now has not been checked; check the connection to see whose it is.',
      needsSetup: true,
    });
    const tempo = trackerConnection({ kind: 'tempo', account: '', verifiedAccountId: '', verifiedName: '', checkedTokenInFile: false });
    expect(trackerConnectionStatus(tempo)).toMatchObject({
      label: 'Token not checked',
      detail: 'The token in the key file now has not been checked; check the connection to test it.',
    });
  });

  it('shows a current problem as its kind and the app’s sentence, keeping who it was last checked as', () => {
    const refused = trackerConnection({ problem: trackerProblem(FailureKind.FailureSetup, 'Jira refused the token; make a new one') });
    expect(trackerConnectionStatus(refused)).toEqual({
      tone: 'error',
      label: 'Needs setup',
      detail: 'Jira refused the token; make a new one. It was last checked as User 1 (user1@example.com).',
      needsSetup: true,
    });
    // The steps follow the token, not the failure: one no check verified keeps them.
    const slow = trackerConnection({ problem: trackerProblem(FailureKind.FailureTransient, 'Jira did not answer.') });
    expect(trackerConnectionStatus(slow)).toMatchObject({ tone: 'warning', label: 'Cannot reach the tracker', detail: 'Jira did not answer. It was last checked as User 1 (user1@example.com).', needsSetup: false });
    expect(trackerConnectionStatus({ ...slow, verifiedAt: 0, checkedTokenInFile: false })).toMatchObject({ label: 'Cannot reach the tracker', detail: 'Jira did not answer.', needsSetup: true });
  });

  // A failure the computer says is about a token the file no longer holds,
  // or a key file read from before the file held a token, is history.
  it('reads a failure that is not about the token in the file now as history', () => {
    const before = trackerProblem(FailureKind.FailureSetup, 'Jira refused the token in the key file; put a valid token in it', false);
    const edited = trackerConnection({ checkedTokenInFile: false, problem: before });
    expect(trackerConnectionStatus(edited)).toEqual({
      tone: 'muted',
      label: 'Key file changed since the last failure',
      detail:
        'The token in it now has not been checked. Before the change: Jira refused the token in the key file; put a valid token in it. It was last checked as User 1 (user1@example.com).',
      needsSetup: true,
    });
    const empty = trackerConnection({ verifiedAt: 0, verifiedName: '', verifiedAccountId: '', checkedTokenInFile: false, problem: trackerProblem(FailureKind.FailureSetup, 'the Jira API token file is empty', false) });
    expect(trackerConnectionStatus(empty).detail).toBe('The token in it now has not been checked. Before the change: the Jira API token file is empty.');
    // The checked token back in the file: signed in, whatever failed for another token.
    expect(trackerConnectionStatus({ ...edited, checkedTokenInFile: true }).label).toBe('Signed in as User 1 (user1@example.com)');
  });

  // No token is the fact to act on, whatever the last failure was.
  it('asks for a token when the key file holds none, over any earlier failure', () => {
    const gone = trackerConnection({ keyFileState: KeyFileState.StateMissing, checkedTokenInFile: false, problem: trackerProblem(FailureKind.FailureTransient, 'Jira did not answer') });
    expect(trackerConnectionStatus(gone)).toEqual({ tone: 'warning', label: 'Needs a token', detail: 'There is no key file yet.', needsSetup: true });
  });

  it('says only that it is off when it is off', () => {
    const off = trackerConnection({ enabled: false, problem: trackerProblem(FailureKind.FailureSetup, 'stale') });
    expect(trackerConnectionStatus(off)).toMatchObject({ tone: 'muted', label: 'Off', needsSetup: false });
  });

  it('is named by its label, or by what it connects to', () => {
    expect(trackerConnectionName(trackerConnection({ label: 'Work' }))).toBe('Work');
    expect(trackerConnectionName(trackerConnection())).toBe('example.atlassian.net');
    expect(trackerConnectionName(trackerConnection({ kind: 'linear', site: 'example' }))).toBe('Linear example');
    expect(trackerConnectionName(trackerConnection({ kind: 'tempo', site: 'https://api.tempo.io' }))).toBe('Tempo time logging');
  });
});

describe('a token’s recorded expiry', () => {
  const now = new Date(2027, 2, 1, 12).getTime();

  it('is silent with none recorded, quiet far off, a warning within the window and an error once passed', () => {
    expect(tokenExpiry(0, now)).toBeNull();
    expect(tokenExpiry(now + (TOKEN_WARNING_DAYS + 1) * DAY, now)).toMatchObject({ tone: 'muted', renew: false });
    expect(tokenExpiry(now + (TOKEN_WARNING_DAYS + 1) * DAY, now)?.text).toMatch(/^Token good until /);
    expect(tokenExpiry(now + TOKEN_WARNING_DAYS * DAY, now)).toEqual({ tone: 'warning', text: 'Token runs out in 14d.', renew: true });
    expect(tokenExpiry(now + 5 * 60 * 60 * 1000, now)).toEqual({ tone: 'warning', text: 'Token runs out in 5h.', renew: true });
    expect(tokenExpiry(now, now)).toMatchObject({ tone: 'error', renew: true });
    expect(tokenExpiry(now - DAY, now)?.text).toMatch(/^Token expired on /);
  });

  it('is entered as a day and kept as that day’s start here', () => {
    const start = startOfLocalDay('2027-03-14');
    expect(start).toBe(new Date(2027, 2, 14).getTime());
    expect(localDateValue(start)).toBe('2027-03-14');
    expect(localDateValue(start + 12 * 60 * 60 * 1000)).toBe('2027-03-14');
    expect(() => startOfLocalDay('')).toThrow('is not a date');
  });
});
