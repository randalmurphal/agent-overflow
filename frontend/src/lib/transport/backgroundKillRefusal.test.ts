import { describe, expect, it } from 'vitest';
import { TransportError } from './wsClient';
import { isBackgroundKillRefusal, refusedBackgroundAgents } from './backgroundKillRefusal';

describe('backgroundKillRefusal', () => {
  it('reads the agents a refused Stop named, normalising each element', () => {
    const err = new TransportError('background_agents_running', 'Stopping now would also stop 2 background agents.', {
      backgroundAgents: [
        { launchItemId: 'tu-a', description: 'gate watcher', runState: 'parked', transcriptRootId: 'tu-a' },
        { launchItemId: 'tu-carrier', description: 'reviewer', runState: 'running', transcriptRootId: 'tu-b' },
      ],
    });
    expect(isBackgroundKillRefusal(err)).toBe(true);
    expect(refusedBackgroundAgents(err)).toEqual([
      { launchItemId: 'tu-a', description: 'gate watcher', runState: 'parked', transcriptRootId: 'tu-a' },
      { launchItemId: 'tu-carrier', description: 'reviewer', runState: 'running', transcriptRootId: 'tu-b' },
    ]);
  });

  it('drops a malformed element, defaults an unknown state to running, and roots a missing transcript at the launch', () => {
    const err = new TransportError('background_agents_running', 'refused', {
      backgroundAgents: [
        { launchItemId: 'tu-a', description: 7, runState: 'done' },
        { description: 'no launch id' },
        'not an object',
        null,
      ],
    });
    expect(refusedBackgroundAgents(err)).toEqual([
      { launchItemId: 'tu-a', description: '', runState: 'running', transcriptRootId: 'tu-a' },
    ]);
  });

  it('answers an empty list for a refusal whose payload is unreadable, so the asker still asks', () => {
    expect(refusedBackgroundAgents(new TransportError('background_agents_running', 'refused'))).toEqual([]);
    expect(refusedBackgroundAgents(new TransportError('background_agents_running', 'refused', { backgroundAgents: { not: 'a list' } }))).toEqual([]);
  });

  it('is null for every other error, and never carries the payload on another code', () => {
    expect(refusedBackgroundAgents(new Error('background_agents_running'))).toBeNull();
    const other = new TransportError('bad_params', 'nope', { backgroundAgents: [{ launchItemId: 'x' }] });
    expect(isBackgroundKillRefusal(other)).toBe(false);
    expect(other.backgroundAgents).toBeUndefined();
    expect(refusedBackgroundAgents(other)).toBeNull();
  });
});
