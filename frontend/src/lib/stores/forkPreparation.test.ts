import { describe, expect, it } from 'vitest';
import { makeThread } from '../../test/helpers/chat';
import { pendingForks, withForkProgress } from './forkPreparation.svelte';
import { mountThreadInPane, openThreadInNewPane } from './panes.svelte';
import { getPaneLayoutItems } from './paneLayout.svelte';

const source = makeThread({ id: 'fork-progress-source', projectId: 'fork-progress-project' });

describe('fork preparation', () => {
  it.each(['success', 'failure'])('shows immediate progress and clears it on %s', async outcome => {
    let resolve!: (value: typeof source) => void;
    let reject!: (error: Error) => void;
    const operation = withForkProgress(source, () => new Promise((yes, no) => { resolve = yes; reject = no; }));
    expect(pendingForks(source.projectId!)).toEqual([source]);
    await expect(withForkProgress(source, async () => source)).resolves.toEqual(source);
    expect(pendingForks(source.projectId!)).toEqual([source]);
    if (outcome === 'success') {
      resolve(source);
      expect(await operation).toEqual(source);
    } else {
      reject(new Error('fork failed'));
      await expect(operation).rejects.toThrow('fork failed');
    }
    expect(pendingForks(source.projectId!)).toEqual([]);
  });

  it('refuses loading a pending catalog row through either navigation path', async () => {
    const before = [...getPaneLayoutItems()];
    const preparing = { ...source, forkPreparing: true };
    await expect(mountThreadInPane(preparing)).rejects.toThrow('still being prepared');
    await expect(openThreadInNewPane(preparing)).rejects.toThrow('still being prepared');
    expect(getPaneLayoutItems()).toEqual(before);
  });
});
