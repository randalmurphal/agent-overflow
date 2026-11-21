// The boot reads a computer's thread groups from the same moment as its
// threads: the sidebar lists the threads only once both reads finish, so a
// group read that waited for the thread answer would add its round trip to
// every boot.
import { beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import App from '../../App.svelte';
import { flush, installAnimateShim, installAppDefaults, resetAppState } from './_helpers';
import { setBindingMock } from '../mocks/bindings-app';
import { resetCatalogLoadForTest } from '../../lib/stores/catalogLoad.svelte';
import { resetThreadGroupsForTest } from '../../lib/stores/threadGroups.svelte';
import type { Thread } from '../../lib/types/models';

beforeAll(installAnimateShim);

describe('App integration: sidebar catalog boot', () => {
  beforeEach(() => {
    resetAppState();
    resetCatalogLoadForTest();
    resetThreadGroupsForTest();
    installAppDefaults();
  });

  it('asks for the thread groups before the thread list has answered', async () => {
    let answerThreads: (rows: Thread[]) => void = () => {};
    const listThreads = setBindingMock('ListThreads', () => new Promise<Thread[]>((resolve) => { answerThreads = resolve; }));
    const listGroups = setBindingMock('ListThreadGroups', async () => []);
    render(App);
    await flush(10);
    expect(listThreads).toHaveBeenCalled();
    expect(listGroups).toHaveBeenCalled();
    answerThreads([]);
    await flush(4);
  });
});
