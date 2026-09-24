// The image menu host: which elements raise it (every attachment image
// surface, never a file chip), what its rows call, and how it behaves over
// the lightbox, which must survive every way the menu closes.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { mockAttachmentDownload } from '../../../test/mocks/attachmentTransfer';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { Attachment } from '../../types/attachment';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import {
  canSaveMenuImage,
  copyMenuImage,
  saveMenuImage,
  saveMenuImageLabel,
} from '../../utils/imageMenuActions';
import ImageMenuHost from './ImageMenuHost.svelte';
import UserMessage from './UserMessage.svelte';
import GeneratedImageMessage from './GeneratedImageMessage.svelte';
import ExpandedImageDialog from './ExpandedImageDialog.svelte';
import ComposerAttachmentRow from '../composer/ComposerAttachmentRow.svelte';

vi.mock('../../utils/imageMenuActions', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../utils/imageMenuActions')>()),
  copyMenuImage: vi.fn(async () => {}),
  saveMenuImage: vi.fn(async () => {}),
  canSaveMenuImage: vi.fn(() => true),
  saveMenuImageLabel: vi.fn(() => 'Save Image'),
}));

const image = {
  id: 'att-1',
  threadId: 'thread-1',
  filename: 'hero.png',
  mimeType: 'image/png',
  size: 128,
  kind: 'image',
};
const file = {
  id: 'att-2',
  threadId: 'thread-1',
  filename: 'report.pdf',
  mimeType: 'application/pdf',
  size: 2048,
  kind: 'file',
};
const REF = { kind: 'attachment', threadId: 'thread-1', attachmentId: 'att-1', filename: 'hero.png' };

function pane(): ThreadPane {
  return {
    threadId: 'thread-1',
    paneId: 'pane-1',
    attachmentCacheFor: () => undefined,
    setUserMessageExpanded: () => {},
  } as unknown as ThreadPane;
}

function asRow(attachment: typeof image): Attachment {
  return { ...attachment, relativePath: '', createdAt: 1 } as Attachment;
}

function menu(): HTMLElement | null {
  return document.querySelector('[role="menu"][aria-label="Image Actions"]');
}

function item(label: string): HTMLElement {
  const found = Array.from(document.querySelectorAll<HTMLElement>('[role="menuitem"]')).find(
    (el) => el.textContent?.trim() === label,
  );
  if (!found) throw new Error(`no menu item labelled "${label}"`);
  return found;
}

async function rightClick(target: Element): Promise<MouseEvent> {
  const event = new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 40, clientY: 50 });
  await fireEvent(target, event);
  await tick();
  return event;
}

function toastMessages(): string[] {
  return getToasts().map((t) => `${t.type}: ${t.message}`);
}

describe('<ImageMenuHost>', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('GetAttachmentThumbnail', async () => ({ data: 'iVBORw0KGgo=', mimeType: 'image/png' }));
    mockAttachmentDownload();
    vi.mocked(copyMenuImage).mockReset().mockResolvedValue(undefined);
    vi.mocked(saveMenuImage).mockReset().mockResolvedValue(undefined);
    vi.mocked(canSaveMenuImage).mockReset().mockReturnValue(true);
    vi.mocked(saveMenuImageLabel).mockReset().mockReturnValue('Save Image');
    for (const t of [...getToasts()]) removeToast(t.id);
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  describe('opens on every attachment image surface', () => {
    it('a user message image, but not its file chip', async () => {
      render(ImageMenuHost);
      const { getByLabelText, getByTestId } = render(UserMessage, {
        props: {
          pane: pane(),
          item: makeItem({
            kind: 'user_text',
            role: 'user',
            summary: 'look [Image #1]',
            meta: JSON.stringify({ attachments: [image, file] }),
          }),
        },
      });

      const chip = getByTestId('user-message-file-attachments').firstElementChild!;
      const unclaimed = await rightClick(chip);
      expect(unclaimed.defaultPrevented).toBe(false);
      expect(menu()).toBeNull();

      const claimed = await rightClick(getByLabelText('Preview hero.png'));
      expect(claimed.defaultPrevented).toBe(true);
      expect(menu()).not.toBeNull();
      expect(item('Copy Image')).toBeTruthy();
      expect(item('Save Image')).toBeTruthy();
    });

    it('a composer image thumb, anywhere on it, but not a file chip', async () => {
      render(ImageMenuHost);
      const { getByTestId, getByLabelText } = render(ComposerAttachmentRow, {
        props: { attachments: [asRow(image), asRow(file)], onRemove: vi.fn() },
      });

      await rightClick(getByTestId('attachment-file-chip'));
      expect(menu()).toBeNull();

      // The #N badge sits beside the preview button, not inside it.
      await rightClick(getByLabelText('Image 1'));
      expect(menu()).not.toBeNull();
      item('Copy Image').click();
      expect(copyMenuImage).toHaveBeenCalledWith(REF);
    });

    it('a generated image', async () => {
      render(ImageMenuHost);
      const { getByLabelText } = render(GeneratedImageMessage, {
        props: {
          pane: pane() as never,
          item: makeItem({
            id: 'image:img-1',
            summary: 'A dashboard',
            meta: JSON.stringify({
              generatedImage: { sourceItemId: 'img-1', provider: 'codex', prompt: 'A dashboard' },
              attachments: [image],
            }),
          }),
        },
      });
      await rightClick(getByLabelText('Preview hero.png'));
      expect(menu()).not.toBeNull();
    });
  });

  describe('rows', () => {
    async function openOnTile(): Promise<void> {
      render(ImageMenuHost);
      const { getByLabelText } = render(ComposerAttachmentRow, {
        props: { attachments: [asRow(image)], onRemove: vi.fn() },
      });
      await rightClick(getByLabelText('Preview hero.png'));
    }

    it('Copy reaches the clipboard path synchronously with the click and confirms', async () => {
      await openOnTile();
      item('Copy Image').dispatchEvent(new MouseEvent('click', { bubbles: true }));
      // No await between the click and the call: the clipboard write must
      // happen inside the gesture.
      expect(copyMenuImage).toHaveBeenCalledWith(REF);
      await tick();
      await tick();
      expect(menu()).toBeNull();
      expect(toastMessages()).toEqual(['success: Image copied']);
    });

    it('a failed copy is an error toast and no success toast', async () => {
      vi.mocked(copyMenuImage).mockRejectedValue(
        new Error('Could not copy the image: Document is not focused.'),
      );
      await openOnTile();
      await fireEvent.click(item('Copy Image'));
      await waitFor(() =>
        expect(toastMessages()).toEqual(['error: Could not copy the image: Document is not focused.']),
      );
    });

    it('Save hands the attachment to the save policy', async () => {
      await openOnTile();
      await fireEvent.click(item('Save Image'));
      expect(saveMenuImage).toHaveBeenCalledWith(REF);
      expect(menu()).toBeNull();
    });

    it('names the save row by what it will do', async () => {
      vi.mocked(saveMenuImageLabel).mockReturnValue('Open on GitHub');
      await openOnTile();
      expect(saveMenuImageLabel).toHaveBeenCalledWith(REF);
      await fireEvent.click(item('Open on GitHub'));
      expect(saveMenuImage).toHaveBeenCalledWith(REF);
    });

    it('Save is inert and says why when this device may not write there', async () => {
      vi.mocked(canSaveMenuImage).mockReturnValue(false);
      await openOnTile();
      const save = item('Save Image');
      expect(save.getAttribute('aria-disabled')).toBe('true');
      expect(save.getAttribute('title')).toBe('Not granted to this device');
      await fireEvent.click(save);
      expect(saveMenuImage).not.toHaveBeenCalled();
    });
  });

  describe('over the lightbox', () => {
    async function openLightbox() {
      const onClose = vi.fn();
      render(ImageMenuHost);
      const view = render(ExpandedImageDialog, {
        props: {
          preview: {
            images: [
              { ...image, url: 'blob:full-1' },
              { ...image, id: 'att-3', filename: 'two.png', url: 'blob:full-3' },
            ],
            index: 0,
          },
          onClose,
        },
      });
      const dialog = view.getByRole('dialog');
      const picture = view.getByRole('img', { name: 'hero.png' });
      // The lightbox takes focus on mount; let that settle before a menu
      // opens over it, as it has long before a person right-clicks.
      await waitFor(() => expect(dialog.contains(document.activeElement)).toBe(true));
      await tick();
      return { onClose, dialog, picture };
    }

    it('opens on the full-size image', async () => {
      const { picture } = await openLightbox();
      await rightClick(picture);
      expect(menu()).not.toBeNull();
      await fireEvent.click(item('Copy Image'));
      expect(copyMenuImage).toHaveBeenCalledWith(REF);
    });

    it('Escape closes the menu only, and the next Escape reaches the lightbox', async () => {
      const { onClose, dialog, picture } = await openLightbox();
      await rightClick(picture);
      await waitFor(() => expect(document.activeElement?.getAttribute('role')).toBe('menuitem'));

      await fireEvent.keyDown(document.activeElement!, { key: 'Escape' });
      expect(menu()).toBeNull();
      expect(onClose).not.toHaveBeenCalled();

      // Focus is back where the lightbox's key handler can see it.
      await waitFor(() => expect(dialog.contains(document.activeElement)).toBe(true));
      await fireEvent.keyDown(document.activeElement!, { key: 'Escape' });
      expect(onClose).toHaveBeenCalledTimes(1);
    });

    it('arrow keys move through the menu without paging the lightbox', async () => {
      const { picture } = await openLightbox();
      await rightClick(picture);
      await waitFor(() => expect(document.activeElement?.textContent?.trim()).toBe('Copy Image'));
      await fireEvent.keyDown(document.activeElement!, { key: 'ArrowDown' });
      expect(document.activeElement?.textContent?.trim()).toBe('Save Image');
      await fireEvent.keyDown(document.activeElement!, { key: 'ArrowRight' });
      expect(document.querySelector('[role="dialog"] img')?.getAttribute('alt')).toBe('hero.png');
    });

    it('choosing a row leaves the lightbox open', async () => {
      const { onClose, picture } = await openLightbox();
      await rightClick(picture);
      await fireEvent.click(item('Save Image'));
      expect(saveMenuImage).toHaveBeenCalledWith(REF);
      expect(onClose).not.toHaveBeenCalled();
    });

    it('a press on the backdrop dismisses the menu, not the lightbox', async () => {
      const { onClose, picture } = await openLightbox();
      await rightClick(picture);
      const backdrop = document.querySelector<HTMLElement>('[role="dialog"] > button')!;

      await fireEvent.pointerDown(backdrop, { button: 0 });
      await fireEvent.mouseDown(backdrop, { button: 0 });
      await fireEvent.click(backdrop);
      await tick();
      expect(menu()).toBeNull();
      expect(onClose).not.toHaveBeenCalled();

      // With no menu up, the backdrop closes the lightbox as before.
      await fireEvent.pointerDown(backdrop, { button: 0 });
      await fireEvent.mouseDown(backdrop, { button: 0 });
      await fireEvent.click(backdrop);
      expect(onClose).toHaveBeenCalledTimes(1);
    });
  });

  it('a press outside a menu that is not over a dialog passes through', async () => {
    render(ImageMenuHost);
    const { getByLabelText } = render(ComposerAttachmentRow, {
      props: { attachments: [asRow(image)], onRemove: vi.fn() },
    });
    const elsewhere = document.createElement('button');
    const clicked = vi.fn();
    elsewhere.addEventListener('click', clicked);
    document.body.appendChild(elsewhere);

    await rightClick(getByLabelText('Preview hero.png'));
    await fireEvent.pointerDown(elsewhere, { button: 0 });
    await fireEvent.mouseDown(elsewhere, { button: 0 });
    await fireEvent.click(elsewhere);
    expect(menu()).toBeNull();
    expect(clicked).toHaveBeenCalledTimes(1);
  });
});
