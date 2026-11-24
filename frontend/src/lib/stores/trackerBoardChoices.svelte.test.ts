// The board picker's list: one read per computer that grants tracker:read,
// pinned to it, the newest read winning, a failure kept beside the last
// answer, and a detached computer's boards gone.

import { describe, expect, it } from 'vitest';
import { tick } from 'svelte';
import type { BoardChoice } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import {
  readTrackerBoardChoices,
  trackerBoardChoiceErrors,
  trackerBoardChoices,
  trackerBoardChoicesPending,
} from './trackerBoardChoices.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { detachBackend, takePinnedBackend } from '../transport/backends';
import { setCarriedSessionScopes } from '../transport/scopes';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { stageBackend } from '../../test/helpers/backends';
import { answerAsV0019 } from '../../test/helpers/olderComputer';

function board(id: string): BoardChoice {
  return {
    project: 'p1', connection: 'c1', connection_kind: 'jira', connection_label: 'Work', connection_enabled: true,
    verified_account: 'acc-user1', board: id, name: `Board ${id}`, tracker_project: 'WEB', opens_first: false,
  } as BoardChoice;
}

describe('the boards the picker lists', () => {
  it('reads every computer, pinned to it, and lists their boards in attach order', async () => {
    stageBackend();
    setCarriedSessionScopes('laptop', ['tracker:read']);
    const pins: string[] = [];
    setBindingMock('TrackerBoardChoices', async () => {
      const pin = String(takePinnedBackend() ?? '');
      pins.push(pin);
      return pin === 'laptop' ? [board('2')] : [board('1')];
    });
    expect(trackerBoardChoicesPending()).toBe(true);
    await readTrackerBoardChoices();
    expect(pins.sort()).toEqual([HOME_BACKEND, 'laptop'].sort());
    expect(trackerBoardChoices().map((c) => [c.backend, c.choice.board])).toEqual([[HOME_BACKEND, '1'], ['laptop', '2']]);
    expect(trackerBoardChoicesPending()).toBe(false);

    detachBackend('laptop');
    expect(trackerBoardChoices().map((c) => c.choice.board)).toEqual(['1']);
  });

  it('keeps the newest read of a computer when an older one answers after it', async () => {
    const answers: Array<(rows: BoardChoice[]) => void> = [];
    setBindingMock('TrackerBoardChoices', () => new Promise<BoardChoice[]>((resolve) => answers.push(resolve)));
    const first = readTrackerBoardChoices();
    const second = readTrackerBoardChoices();
    answers[1]([board('new')]);
    answers[0]([board('old')]);
    await Promise.all([first, second]);
    expect(trackerBoardChoices().map((c) => c.choice.board)).toEqual(['new']);
  });

  it('keeps the last answer beside a failure, and says nothing of a computer it could not reach', async () => {
    setBindingMock('TrackerBoardChoices', async () => [board('1')]);
    await readTrackerBoardChoices();
    setBindingMock('TrackerBoardChoices', async () => {
      throw new TransportError('method_error', 'database is locked');
    });
    await readTrackerBoardChoices();
    expect(trackerBoardChoices().map((c) => c.choice.board)).toEqual(['1']);
    expect(trackerBoardChoiceErrors().map((e) => e.error.report.reason)).toEqual([expect.stringMatching(/database is locked/i)]);

    setBindingMock('TrackerBoardChoices', async () => {
      throw new DisconnectedError('offline');
    });
    await readTrackerBoardChoices();
    await tick();
    expect(trackerBoardChoiceErrors()).toEqual([]);
    expect(trackerBoardChoices().map((c) => c.choice.board)).toEqual(['1']);
  });

  it('lists no board of a computer on a release without the method', async () => {
    answerAsV0019();
    await readTrackerBoardChoices();
    expect(trackerBoardChoices()).toEqual([]);
    expect(trackerBoardChoiceErrors()).toEqual([]);
    expect(trackerBoardChoicesPending()).toBe(false);
  });
});
