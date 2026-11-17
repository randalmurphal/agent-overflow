// The link an AO tool result shows, in both layouts. A thread_reply answers a
// caller that can be on another computer: its link names that computer and
// reads the thread there, or is plain text when this client cannot reach it.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import AoToolLink from './AoToolLink.svelte';
import { threadResultView } from './aoToolResults';
import { NO_AO_NAMES } from './aoTools';
import { getPinnedBackend } from '../../transport/backends';
import { HOME_BACKEND } from '../../transport/backendKey';
import { resetStagedBackends, stageBackend, REMOTE_BACKEND_UUID } from '../../../test/helpers/backends';
import { installPaneMocks } from '../../../test/helpers/chat';
import { setBindingMock } from '../../../test/mocks/bindings-app';

function replyLink(computerId: string, computerName: string) {
  const ack = { token: 't', accepted: true, state: 'replied', source_thread_id: 'caller-thread', source_computer: computerName, source_computer_id: computerId };
  return threadResultView('thread_reply', JSON.stringify(ack), {}, NO_AO_NAMES)!.link!;
}

describe('<AoToolLink> for a reply to a caller on another computer', () => {
  beforeEach(() => {
    installPaneMocks();
    resetStagedBackends();
    stageBackend({ id: HOME_BACKEND, name: 'This computer', backendId: 'home-uuid' });
  });
  afterEach(() => resetStagedBackends());

  it('names that computer and reads the caller\'s thread there', async () => {
    stageBackend({ id: 'studio', backendId: REMOTE_BACKEND_UUID, name: 'Studio' });
    const read: Array<string | null> = [];
    setBindingMock('GetThread', async () => {
      read.push(getPinnedBackend());
      return null;
    });
    const view = render(AoToolLink, { props: { link: replyLink('studio', 'Studio'), viewer: HOME_BACKEND } });
    expect(view.getByTestId('ao-tool-link-computer').textContent).toBe('on Studio');
    await fireEvent.click(view.getByRole('button'));
    expect(read).toEqual(['studio']);
  });

  it('names that computer and opens nothing when this client does not reach it', () => {
    const view = render(AoToolLink, { props: { link: replyLink('unpaired', 'Old laptop'), viewer: HOME_BACKEND } });
    expect(view.getByTestId('ao-tool-link-computer').textContent).toBe('on Old laptop');
    expect(view.queryByRole('button')).toBeNull();
  });
});
