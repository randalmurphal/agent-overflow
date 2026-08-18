import { afterEach, describe, expect, it, vi } from 'vitest';
import { focusApprovalActionContainer, focusApprovalActionFromKey } from './approvalActionKeyboard';

function mountActionRow(): { container: HTMLDivElement; buttons: HTMLButtonElement[] } {
  const container = document.createElement('div');
  container.tabIndex = -1;
  const buttons = ['approve', 'deny', 'always'].map((label) => {
    const button = document.createElement('button');
    button.textContent = label;
    container.appendChild(button);
    return button;
  });
  document.body.appendChild(container);
  return { container, buttons };
}

function navKey(key: string): KeyboardEvent {
  return new KeyboardEvent('keydown', { key, cancelable: true });
}

describe('approvalActionKeyboard', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  // The container focus fires on PROMPT ARRIVAL — a backend event, not a
  // user gesture. A bare focus() would sync-scroll the pane strip to the
  // prompt's pane even when the user had scrolled away from it.
  it('focuses the container without scrolling', () => {
    const { container } = mountActionRow();
    const focus = vi.spyOn(container, 'focus');

    focusApprovalActionContainer(container);

    expect(focus).toHaveBeenCalledWith({ preventScroll: true });
  });

  it('arrow navigation moves between buttons without scrolling', () => {
    const { container, buttons } = mountActionRow();
    const spies = buttons.map((button) => vi.spyOn(button, 'focus'));

    focusApprovalActionFromKey(navKey('ArrowRight'), container);
    expect(spies[0]).toHaveBeenCalledWith({ preventScroll: true });

    buttons[0].focus();
    focusApprovalActionFromKey(navKey('ArrowRight'), container);
    expect(spies[1]).toHaveBeenCalledWith({ preventScroll: true });

    buttons[1].focus();
    focusApprovalActionFromKey(navKey('ArrowLeft'), container);
    expect(spies[0]).toHaveBeenLastCalledWith({ preventScroll: true });
  });

  it('ignores modified keys and empty rows', () => {
    const { container, buttons } = mountActionRow();
    const spies = buttons.map((button) => vi.spyOn(button, 'focus'));

    focusApprovalActionFromKey(
      new KeyboardEvent('keydown', { key: 'ArrowRight', ctrlKey: true, cancelable: true }),
      container,
    );
    for (const spy of spies) expect(spy).not.toHaveBeenCalled();

    // A row with no enabled buttons is a no-op, not a crash.
    focusApprovalActionFromKey(navKey('ArrowRight'), undefined);
  });
});
