import { afterEach, describe, expect, it } from 'vitest';
import { userEvent } from 'vitest/browser';
// Import the real production stylesheet so Blink resolves color-mix(),
// custom properties, direct-child hover selectors, and @layer utilities
// against the same cascade the app ships. happy-dom cannot detect IACVT
// declarations (such as an undefined var(--…)) or unlayered rules overriding
// Tailwind's disabled:opacity-40.
import '../app.css';

const hosts: HTMLElement[] = [];

function createStage(): { stage: HTMLDivElement; idle: HTMLDivElement } {
  const stage = document.createElement('div');
  stage.style.cssText = 'position:fixed;top:0;left:0;width:480px;height:240px;display:flex;flex-direction:column;gap:12px;padding:16px;';
  const idle = document.createElement('div');
  idle.style.cssText = 'width:40px;height:24px;';
  idle.textContent = 'idle';
  stage.appendChild(idle);
  document.body.appendChild(stage);
  hosts.push(stage);
  return { stage, idle };
}

function isOpaqueTint(color: string): boolean {
  if (!color || color === 'transparent') return false;
  // Computed transparent colors serialize as rgba(0, 0, 0, 0) or color(... / 0).
  if (/^rgba\(\s*0\s*,\s*0\s*,\s*0\s*,\s*0\s*\)$/.test(color)) return false;
  if (/\/\s*0(?:\.0+)?\s*\)$/.test(color)) return false;
  return true;
}

afterEach(() => {
  for (const host of hosts.splice(0)) host.remove();
});

describe('app.css direct-child hover and diff-line contracts', () => {
  it('paints .ao-diff-line ::before only when hovered or targeted and never collapses to transparent', async () => {
    const { stage, idle } = createStage();
    const line = document.createElement('div');
    line.className = 'ao-diff-line relative';
    line.style.cssText = 'width:320px;height:24px;';
    line.textContent = '+ added line';
    stage.appendChild(line);

    await userEvent.hover(idle);
    expect(getComputedStyle(line, '::before').content).toBe('none');

    await userEvent.hover(line);
    const hoverBefore = getComputedStyle(line, '::before');
    expect(hoverBefore.content).toBe('""');
    expect(isOpaqueTint(hoverBefore.backgroundColor)).toBe(true);

    // Review target highlight must paint while unhovered and remain painted when hovered.
    await userEvent.hover(idle);
    line.setAttribute('data-review-target', 'true');
    const targetBefore = getComputedStyle(line, '::before');
    expect(targetBefore.content).toBe('""');
    expect(isOpaqueTint(targetBefore.backgroundColor)).toBe(true);

    await userEvent.hover(line);
    const targetHoverBefore = getComputedStyle(line, '::before');
    expect(targetHoverBefore.content).toBe('""');
    expect(isOpaqueTint(targetHoverBefore.backgroundColor)).toBe(true);
  });

  it('reveals enabled .ao-hover-reveal controls on hover without overriding disabled opacity', async () => {
    const { stage, idle } = createStage();
    const row = document.createElement('div');
    row.className = 'ao-tool-row';
    row.style.cssText = 'width:320px;height:32px;';
    const header = document.createElement('div');
    header.className = 'ao-tool-header';
    header.style.cssText = 'display:flex;gap:8px;';

    const enabledBtn = document.createElement('button');
    enabledBtn.type = 'button';
    enabledBtn.className = 'ao-hover-reveal opacity-0 disabled:opacity-40';
    enabledBtn.textContent = 'Run';

    const disabledBtn = document.createElement('button');
    disabledBtn.type = 'button';
    disabledBtn.disabled = true;
    disabledBtn.className = 'ao-hover-reveal opacity-0 disabled:opacity-40';
    disabledBtn.textContent = 'Wait';

    header.append(enabledBtn, disabledBtn);
    row.appendChild(header);
    stage.appendChild(row);

    await userEvent.hover(idle);
    expect(getComputedStyle(enabledBtn).opacity).toBe('0');
    expect(getComputedStyle(disabledBtn).opacity).toBe('0.4');

    await userEvent.hover(row);
    expect(getComputedStyle(enabledBtn).opacity).toBe('1');
    expect(getComputedStyle(disabledBtn).opacity).toBe('0.4');
  });

  it('reveals direct-child assistant copy slot and diff add button on hover', async () => {
    const { stage, idle } = createStage();
    const msg = document.createElement('div');
    msg.className = 'ao-assistant-msg';
    msg.style.cssText = 'width:320px;height:28px;';
    const meta = document.createElement('div');
    meta.setAttribute('data-testid', 'assistant-message-meta');
    const copySlot = document.createElement('div');
    copySlot.setAttribute('data-testid', 'assistant-message-copy-slot');
    const copyBtn = document.createElement('button');
    copyBtn.type = 'button';
    copyBtn.className = 'ao-hover-reveal opacity-0';
    copyBtn.textContent = 'Copy';
    copySlot.appendChild(copyBtn);
    meta.appendChild(copySlot);
    msg.appendChild(meta);

    const diffLine = document.createElement('div');
    diffLine.className = 'ao-diff-line';
    diffLine.style.cssText = 'width:320px;height:24px;';
    const action = document.createElement('span');
    action.className = 'ao-diff-action';
    const addBtn = document.createElement('button');
    addBtn.type = 'button';
    addBtn.className = 'ao-diff-add-btn opacity-0';
    addBtn.textContent = '+';
    action.appendChild(addBtn);
    diffLine.appendChild(action);

    stage.append(msg, diffLine);

    await userEvent.hover(idle);
    expect(getComputedStyle(copyBtn).opacity).toBe('0');
    expect(getComputedStyle(addBtn).opacity).toBe('0');

    await userEvent.hover(msg);
    expect(getComputedStyle(copyBtn).opacity).toBe('1');

    await userEvent.hover(diffLine);
    expect(getComputedStyle(addBtn).opacity).toBe('1');
  });
});
