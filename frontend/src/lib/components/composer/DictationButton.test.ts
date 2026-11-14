import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';

import DictationButton from './DictationButton.svelte';
import { dictationFixture } from '../../../test/helpers/dictation';

describe('<DictationButton>', () => {
  it('starts on click, shows the stepped level while recording, and stops into a transcript', async () => {
    const f = dictationFixture();
    const { getByTestId } = render(DictationButton, { props: { session: f.session } });
    const button = getByTestId('composer-dictate') as HTMLButtonElement;
    expect(button.dataset.phase).toBe('idle');
    expect(button.getAttribute('aria-label')).toMatch(/^Dictate/);

    await fireEvent.click(button);
    await vi.waitFor(() => expect(button.dataset.phase).toBe('recording'));
    f.tick();
    await tick();
    expect(button.dataset.level).toBe('4');
    expect(button.getAttribute('aria-pressed')).toBe('true');
    expect(button.getAttribute('aria-label')).toBe('Stop dictation');

    await fireEvent.click(button);
    await vi.waitFor(() => expect(f.inserted).toEqual(['hello world']));
    expect(button.dataset.phase).toBe('idle');
    expect(button.dataset.level).toBeUndefined();
  });

  it('is disabled with a spinner while transcribing', async () => {
    const f = dictationFixture();
    f.remote.finish.mockReturnValueOnce(new Promise<string>(() => {}));
    const { getByTestId } = render(DictationButton, { props: { session: f.session } });
    const button = getByTestId('composer-dictate') as HTMLButtonElement;
    await f.session.start();
    f.tick();
    void f.session.stop();
    await vi.waitFor(() => expect(button.dataset.phase).toBe('transcribing'));
    expect(button.disabled).toBe(true);
    expect(button.getAttribute('aria-label')).toBe('Transcribing…');
  });

  it('Escape abandons the recording', async () => {
    const f = dictationFixture();
    const { getByTestId } = render(DictationButton, { props: { session: f.session } });
    const button = getByTestId('composer-dictate');
    await f.session.start();
    await tick();
    await fireEvent.keyDown(button, { key: 'Escape' });
    expect(f.session.phase).toBe('idle');
    expect(f.remote.cancel).toHaveBeenCalledWith('rec-1');
    expect(f.inserted).toEqual([]);
  });

  it('shows why the last attempt failed in its title', async () => {
    const f = dictationFixture({ openMicrophone: () => Promise.reject(Object.assign(new Error('x'), { name: 'NotAllowedError' })) });
    const { getByTestId } = render(DictationButton, { props: { session: f.session } });
    await f.session.start();
    await tick();
    const button = getByTestId('composer-dictate');
    expect(button.dataset.error).toBe('true');
    expect(button.getAttribute('title')).toBe('Microphone access is blocked.');
  });

  it('keeps the textarea focused under a touch tap', () => {
    const f = dictationFixture();
    const { getByTestId } = render(DictationButton, { props: { session: f.session } });
    const touch = new PointerEvent('pointerdown', { pointerType: 'touch', bubbles: true, cancelable: true });
    getByTestId('composer-dictate').dispatchEvent(touch);
    expect(touch.defaultPrevented).toBe(true);
  });
});
