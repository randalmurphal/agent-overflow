import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import DiscussionEditor from './DiscussionEditor.svelte';
import type { DiscussionDefinition } from '../../types/discussion';
import { createEmptyDiscussionDefinition } from '../../types/discussion';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';

// happy-dom lacks Element.animate, which Svelte's transition:fade/scale calls.
// Stub a no-op animation controller so dialogs (ConfirmDialog) can mount.
if (typeof Element !== 'undefined' && !('animate' in Element.prototype)) {
  (Element.prototype as unknown as { animate: unknown }).animate = function () {
    return {
      cancel() {},
      finish() {},
      play() {},
      pause() {},
      reverse() {},
      addEventListener() {},
      removeEventListener() {},
      onfinish: null,
      oncancel: null,
      finished: Promise.resolve(),
      effect: null,
      startTime: 0,
      currentTime: 0,
      playState: 'finished',
      playbackRate: 1,
    };
  };
}

function savedDef(overrides: Partial<DiscussionDefinition> = {}): DiscussionDefinition {
  return {
    id: 'd1',
    name: 'code-review',
    description: 'scrutinizer/defender pair',
    scope: 'global',
    projectId: '',
    participants: [
      { role: 'scrutinizer', description: 'looks for flaws', system: 'You are picky.', provider: undefined, model: undefined },
      { role: 'defender', description: 'justifies choices', system: 'You argue pragmatism.', provider: undefined, model: undefined },
    ],
    settings: { maxTurns: 8 },
    createdAt: 10,
    updatedAt: 20,
    ...overrides,
  };
}

describe('<DiscussionEditor>', () => {
  beforeEach(() => {
    setBindingMock('CreateDiscussion', async () => {});
    setBindingMock('UpdateDiscussion', async () => {});
    setBindingMock('DeleteDiscussion', async () => {});
  });

  it('blocks saving a new discussion when the name is missing', async () => {
    const onSaved = () => {};
    const createMock = getBindingMock('CreateDiscussion');
    const { getByRole } = render(DiscussionEditor, {
      props: {
        initial: createEmptyDiscussionDefinition(),
        isNew: true,
        onSaved,
      },
    });
    const submit = getByRole('button', { name: /create/i }) as HTMLButtonElement;
    await fireEvent.click(submit);
    expect(createMock?.mock.calls.length ?? 0).toBe(0);
    const alert = getByRole('alert');
    expect(alert.textContent).toMatch(/discussion name is required/i);
  });

  it('blocks saving when any participant is missing a system prompt', async () => {
    const draft = {
      ...createEmptyDiscussionDefinition(),
      name: 'refinement',
    };
    // participants have empty system prompts by default
    const createMock = setBindingMock('CreateDiscussion', async () => {});
    const { getByRole } = render(DiscussionEditor, {
      props: {
        initial: draft,
        isNew: true,
        onSaved: () => {},
      },
    });
    await fireEvent.click(getByRole('button', { name: /create/i }));
    expect(createMock.mock.calls.length).toBe(0);
    const alert = getByRole('alert');
    expect(alert.textContent).toMatch(/system prompt/i);
  });

  it('creates a new discussion via CreateDiscussion with the edited fields', async () => {
    const savedEvents: DiscussionDefinition[] = [];
    const createMock = setBindingMock('CreateDiscussion', async () => {});
    const { container, getByRole } = render(DiscussionEditor, {
      props: {
        initial: createEmptyDiscussionDefinition(),
        isNew: true,
        onSaved: (d) => savedEvents.push(d),
      },
    });
    const nameInput = container.querySelector<HTMLInputElement>('#discussion-name')!;
    const p0System = container.querySelector<HTMLTextAreaElement>('#participant-0-system')!;
    const p1System = container.querySelector<HTMLTextAreaElement>('#participant-1-system')!;
    await fireEvent.input(nameInput, { target: { value: 'interrogate' } });
    await fireEvent.input(p0System, { target: { value: 'You prosecute.' } });
    await fireEvent.input(p1System, { target: { value: 'You defend.' } });

    await fireEvent.click(getByRole('button', { name: /create/i }));
    await Promise.resolve();
    await Promise.resolve();

    expect(createMock.mock.calls.length).toBe(1);
    const sentDraft = createMock.mock.calls[0][0] as DiscussionDefinition;
    expect(sentDraft.name).toBe('interrogate');
    expect(sentDraft.participants[0].system).toBe('You prosecute.');
    expect(sentDraft.participants[1].system).toBe('You defend.');
    expect(sentDraft.scope).toBe('global');
    expect(savedEvents.length).toBe(1);
  });

  it('requires a project path when scope is project-scoped', async () => {
    const createMock = setBindingMock('CreateDiscussion', async () => {});
    const { container, getByRole } = render(DiscussionEditor, {
      props: {
        initial: {
          ...createEmptyDiscussionDefinition(),
          name: 'per-project',
          participants: [
            { role: 'a', description: '', system: 'say a', provider: undefined, model: undefined },
            { role: 'b', description: '', system: 'say b', provider: undefined, model: undefined },
          ],
        },
        isNew: true,
        onSaved: () => {},
      },
    });
    const scopeSelect = container.querySelector<HTMLSelectElement>('#discussion-scope')!;
    await fireEvent.change(scopeSelect, { target: { value: 'project' } });
    await fireEvent.click(getByRole('button', { name: /create/i }));
    expect(createMock.mock.calls.length).toBe(0);
    const alert = getByRole('alert');
    expect(alert.textContent).toMatch(/project-scoped discussions require a project path/i);
  });

  it('edits an existing discussion via UpdateDiscussion using the original name+scope as key', async () => {
    const original = savedDef();
    const updateMock = setBindingMock('UpdateDiscussion', async () => {});
    const { container, getByRole } = render(DiscussionEditor, {
      props: {
        initial: original,
        isNew: false,
        onSaved: () => {},
        onDeleted: () => {},
      },
    });
    const descInput = container.querySelector<HTMLInputElement>('#discussion-description')!;
    await fireEvent.input(descInput, { target: { value: 'updated summary' } });
    await fireEvent.click(getByRole('button', { name: /save changes/i }));
    await Promise.resolve();
    await Promise.resolve();

    expect(updateMock.mock.calls.length).toBe(1);
    const [prevName, prevScope, next] = updateMock.mock.calls[0] as [string, string, DiscussionDefinition];
    expect(prevName).toBe('code-review');
    expect(prevScope).toBe('global');
    expect(next.description).toBe('updated summary');
  });

  it('opens a confirm dialog before deleting, then calls DeleteDiscussion on confirm', async () => {
    const original = savedDef();
    const deleteMock = setBindingMock('DeleteDiscussion', async () => {});
    const { getByRole, findByRole } = render(DiscussionEditor, {
      props: {
        initial: original,
        isNew: false,
        onSaved: () => {},
        onDeleted: () => {},
      },
    });
    await fireEvent.click(getByRole('button', { name: /delete discussion/i }));
    const dialog = await findByRole('dialog');
    expect(dialog).toBeInTheDocument();
    // Deletion hasn't happened yet.
    expect(deleteMock.mock.calls.length).toBe(0);
    // Confirm. The dialog only has two buttons — Cancel and the
    // danger Confirm. Destructive ConfirmDialogs autofocus Cancel, so
    // we resolve Confirm by name instead of by [data-autofocus] like
    // the older non-destructive variant did.
    const confirm = dialog.querySelector<HTMLButtonElement>('button.bg-error, button[class*="bg-error"]')
      ?? Array.from(dialog.querySelectorAll<HTMLButtonElement>('button')).find((b) => /confirm|delete/i.test(b.textContent ?? ''));
    if (!confirm) throw new Error('confirm button not found');
    await fireEvent.click(confirm);
    await Promise.resolve();
    await Promise.resolve();
    expect(deleteMock.mock.calls.length).toBe(1);
    expect(deleteMock.mock.calls[0]).toEqual(['code-review', 'global']);
  });

  it('adds and removes participants, enforcing a minimum of two', async () => {
    const { container, getByRole } = render(DiscussionEditor, {
      props: {
        initial: createEmptyDiscussionDefinition(),
        isNew: true,
        onSaved: () => {},
      },
    });
    const selectFieldsets = () => Array.from(container.querySelectorAll<HTMLFieldSetElement>('fieldset[aria-label^="Participant"]'));

    let fieldsets = selectFieldsets();
    expect(fieldsets.length).toBe(2);

    await fireEvent.click(getByRole('button', { name: /add participant/i }));
    fieldsets = selectFieldsets();
    expect(fieldsets.length).toBe(3);

    const removeButtons = fieldsets.map((fs) => fs.querySelector<HTMLButtonElement>('button[aria-label^="Remove Participant"]')!);
    expect(removeButtons[2].disabled).toBe(false);
    await fireEvent.click(removeButtons[2]);
    fieldsets = selectFieldsets();
    expect(fieldsets.length).toBe(2);

    // Both remove buttons are now disabled (minimum of two enforced).
    const stillThere = fieldsets.map((fs) => fs.querySelector<HTMLButtonElement>('button[aria-label^="Remove Participant"]')!);
    expect(stillThere[0].disabled).toBe(true);
    expect(stillThere[1].disabled).toBe(true);
  });
});
